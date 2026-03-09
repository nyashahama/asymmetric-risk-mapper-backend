package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq" // postgres driver
	"github.com/soheilhy/cmux"

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

	// ── Store ─────────────────────────────────────────────────────────────────
	st := store.New(pool, queries)

	// ── Stripe ────────────────────────────────────────────────────────────────
	stripeClient := stripeinternal.NewClient(cfg.StripeSecretKey)

	// ── AI (DeepSeek primary, Anthropic fallback) ─────────────────────────────
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

	// ── Email ─────────────────────────────────────────────────────────────────
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

	// ── gRPC server + Stripe webhook handler ──────────────────────────────────
	// NewServer returns both the *grpc.Server (for Serve) and the *api.Server
	// (for StripeWebhookHandler). They must be separate because *grpc.Server
	// is the transport and *api.Server holds the application logic including
	// the HTTP/1.1 webhook handler that Stripe requires.
	grpcSrv, apiSrv := api.NewServer(
		queries,
		st,
		stripeClient,
		runner,
		mailer,
		api.Config{
			BaseURL:             cfg.BaseURL,
			StripeWebhookSecret: cfg.StripeWebhookSecret,
			Env:                 cfg.Env,
		},
		logger,
	)

	// ── Single TCP listener, split by protocol with cmux ──────────────────────
	lis, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	mux := cmux.New(lis)

	// HTTP/2 frames with "application/grpc" content-type → gRPC server.
	grpcLis := mux.MatchWithWriters(
		cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"),
	)

	// Everything else (HTTP/1.1) → Stripe webhook + health check.
	httpLis := mux.Match(cmux.Any())

	webhookMux := http.NewServeMux()
	webhookMux.Handle("/api/webhooks/stripe", apiSrv.StripeWebhookHandler())
	webhookMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	httpSrv := &http.Server{
		Handler:      webhookMux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go runner.Start(ctx)

	go func() {
		if err := mux.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
			logger.Error("cmux serve error", "error", err)
		}
	}()

	grpcErrCh := make(chan error, 1)
	go func() {
		logger.Info("gRPC server listening", "addr", lis.Addr())
		if err := grpcSrv.Serve(grpcLis); err != nil {
			grpcErrCh <- err
		}
	}()

	httpErrCh := make(chan error, 1)
	go func() {
		logger.Info("HTTP (webhook) server listening", "addr", lis.Addr())
		if err := httpSrv.Serve(httpLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErrCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-grpcErrCh:
		return fmt.Errorf("gRPC server error: %w", err)
	case err := <-httpErrCh:
		return fmt.Errorf("HTTP server error: %w", err)
	}

	stopped := make(chan struct{})
	go func() {
		grpcSrv.GracefulStop()
		close(stopped)
	}()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	select {
	case <-stopped:
		logger.Info("gRPC server stopped gracefully")
	case <-shutdownCtx.Done():
		logger.Warn("gRPC graceful stop timed out, forcing")
		grpcSrv.Stop()
	}

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP server shutdown error", "error", err)
	}

	logger.Info("shutdown complete")
	return nil
}

func openDB(dsn string) (*sql.DB, *db.Queries, error) {
	pool, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open: %w", err)
	}

	pool.SetMaxOpenConns(25)
	pool.SetMaxIdleConns(10)
	pool.SetConnMaxLifetime(5 * time.Minute)
	pool.SetConnMaxIdleTime(2 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("ping: %w", err)
	}

	queries := db.New(pool)
	return pool, queries, nil
}