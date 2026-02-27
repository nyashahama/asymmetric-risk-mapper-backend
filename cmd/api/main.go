package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq" // postgres driver

	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/ai"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/api"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/config"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/email"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/store"
	stripeinternal "github.com/nyashahama/asymmetric-risk-mapper-backend/internal/stripe"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/worker"
)

func main() {
	// ── Logger ────────────────────────────────────────────────────────────────
	// JSON in production, pretty text in development.
	var logger *slog.Logger
	if os.Getenv("ENV") == "production" {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))
	} else {
		logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}))
	}
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// ── Config ────────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	logger.Info("config loaded", "env", cfg.Env, "port", cfg.Port)

	// ── Database ──────────────────────────────────────────────────────────────
	pool, queries, err := openDB(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	logger.Info("database connected")

	// ── Store (atomic multi-step writes) ──────────────────────────────────────
	st := store.New(pool, queries)

	// ── Stripe ────────────────────────────────────────────────────────────────
	stripeClient := stripeinternal.NewClient(cfg.StripeSecretKey)

	// ── AI ────────────────────────────────────────────────────────────────────
	// DeepSeek is primary. Anthropic is the fallback when ANTHROPIC_API_KEY is
	// also set. In production, set both keys for maximum resilience.
	var hedger ai.Hedger
	switch {
	case cfg.DeepSeekAPIKey != "" && cfg.AnthropicAPIKey != "":
		primary := ai.NewDeepSeekClient(cfg.DeepSeekAPIKey, cfg.DeepSeekModel)
		secondary := ai.NewAnthropicClient(cfg.AnthropicAPIKey, cfg.AnthropicModel)
		hedger = ai.NewFallbackHedger(primary, secondary, logger)
		logger.Info("ai: using DeepSeek with Anthropic fallback")
	case cfg.DeepSeekAPIKey != "":
		hedger = ai.NewDeepSeekClient(cfg.DeepSeekAPIKey, cfg.DeepSeekModel)
		logger.Info("ai: using DeepSeek only")
	default:
		hedger = ai.NewAnthropicClient(cfg.AnthropicAPIKey, cfg.AnthropicModel)
		logger.Info("ai: using Anthropic only")
	}

	// ── Email (Resend) ────────────────────────────────────────────────────────
	mailer := email.NewResendClient(
		cfg.ResendAPIKey,
		cfg.EmailFromAddr,
		cfg.EmailFromName,
		cfg.BaseURL,
	)

	// ── Worker ────────────────────────────────────────────────────────────────
	job := worker.NewJob(queries, st, hedger, mailer, logger)
	runner := worker.NewRunner(job, st, queries, worker.RunnerConfig{
		Workers:      cfg.WorkerCount,
		PollInterval: cfg.PollInterval,
		JobTimeout:   cfg.JobTimeout,
		MaxRetries:   cfg.MaxRetries,
	}, logger)

	// ── HTTP server ───────────────────────────────────────────────────────────
	handler := api.NewServer(
		queries,
		st,
		stripeClient,
		runner, // *Runner satisfies worker.Enqueuer
		mailer,
		api.Config{
			BaseURL:             cfg.BaseURL,
			StripeWebhookSecret: cfg.StripeWebhookSecret,
			Env:                 cfg.Env,
		},
		logger,
	)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second, // generous — report endpoint can be slow on first hit
		IdleTimeout:  120 * time.Second,
	}

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	// Root context cancelled by OS signal. Worker and HTTP server both respect it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start the worker pool in a background goroutine. It blocks until ctx is done.
	go runner.Start(ctx)

	// Start the HTTP server in a background goroutine.
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// Block until either a signal arrives or the server dies unexpectedly.
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		return fmt.Errorf("server error: %w", err)
	}

	// Give in-flight HTTP requests up to 20 seconds to finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}

	// The worker goroutine will exit when ctx is cancelled (already done).
	// runner.Start blocks until all worker goroutines finish — nothing extra needed.
	logger.Info("shutdown complete")
	return nil
}

// openDB opens the connection pool and prepares all sqlc statements.
// Using db.Prepare (rather than db.New) means every query is validated against
// the database schema at startup — the server refuses to start if the schema
// is out of sync.
func openDB(dsn string) (*sql.DB, *db.Queries, error) {
	pool, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open: %w", err)
	}

	// Tune the connection pool.
	pool.SetMaxOpenConns(25)
	pool.SetMaxIdleConns(10)
	pool.SetConnMaxLifetime(5 * time.Minute)
	pool.SetConnMaxIdleTime(2 * time.Minute)

	// Verify the connection is reachable before proceeding.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("ping: %w", err)
	}

	// Prepare all sqlc statements. This validates the SQL against the live
	// schema — any mismatch (missing column, renamed table) is caught here,
	// not at the first query execution.
	queries, err := db.Prepare(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("prepare statements: %w", err)
	}

	return pool, queries, nil
}