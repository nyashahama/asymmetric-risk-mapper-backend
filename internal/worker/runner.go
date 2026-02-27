// Package worker contains the background job pipeline that scores answers,
// generates AI hedge narratives, persists the report, and sends the delivery
// email. It is intentionally decoupled from the HTTP layer: the api package
// holds a worker.Enqueuer interface and calls Enqueue — it never imports the
// concrete Runner or Job types.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/store"
)

// ─── ENQUEUER INTERFACE ───────────────────────────────────────────────────────

// Enqueuer is the narrow interface the api package uses to hand off work after
// a payment is confirmed. Keeping it here (not in api/) means api/ does not
// need to import worker/.
//
// The concrete implementation is *Runner. In tests, any struct with an Enqueue
// method satisfies the interface.
type Enqueuer interface {
	Enqueue(ctx context.Context, reportID uuid.UUID) error
}

// ─── RUNNER ───────────────────────────────────────────────────────────────────

// RunnerConfig holds tuning parameters for the Runner. All fields have
// sensible defaults if zero-valued; call DefaultRunnerConfig() to get them.
type RunnerConfig struct {
	// Workers is the number of concurrent job goroutines. Default: 3.
	Workers int

	// PollInterval is how often the fallback poller checks ListPendingReports
	// for jobs that were missed by the in-process channel (e.g. after a crash
	// or restart). Default: 30s.
	PollInterval time.Duration

	// JobTimeout is the per-job context deadline. Default: 5 minutes.
	// Set this longer than your AI provider's p99 latency.
	JobTimeout time.Duration

	// MaxRetries is the number of times a job is retried before the report is
	// marked as permanently failed. Default: 3.
	MaxRetries int
}

// DefaultRunnerConfig returns safe production defaults.
func DefaultRunnerConfig() RunnerConfig {
	return RunnerConfig{
		Workers:      3,
		PollInterval: 30 * time.Second,
		JobTimeout:   5 * time.Minute,
		MaxRetries:   3,
	}
}

// Runner manages a pool of worker goroutines. It accepts jobs via an in-process
// channel (fast path, used for new payments) and also polls the database
// periodically to pick up any reports that were in-flight when the process last
// restarted (recovery path).
type Runner struct {
	job    *Job
	store  *store.Store
	q      db.Querier
	cfg    RunnerConfig
	logger *slog.Logger

	queue chan uuid.UUID
	wg    sync.WaitGroup
}

// NewRunner constructs a Runner. Call Start() to begin processing.
func NewRunner(
	job *Job,
	st *store.Store,
	q db.Querier,
	cfg RunnerConfig,
	logger *slog.Logger,
) *Runner {
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultRunnerConfig().Workers
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultRunnerConfig().PollInterval
	}
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = DefaultRunnerConfig().JobTimeout
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = DefaultRunnerConfig().MaxRetries
	}

	return &Runner{
		job:    job,
		store:  st,
		q:      q,
		cfg:    cfg,
		logger: logger,
		// Buffer = Workers*2 so Enqueue never blocks under normal load.
		queue: make(chan uuid.UUID, cfg.Workers*2),
	}
}

// Enqueue pushes a reportID onto the in-process channel. It satisfies the
// Enqueuer interface. If the channel is full (very unlikely given the buffer
// sizing) it returns an error rather than blocking the HTTP response.
func (r *Runner) Enqueue(_ context.Context, reportID uuid.UUID) error {
	select {
	case r.queue <- reportID:
		r.logger.Info("worker: enqueued report", "report_id", reportID)
		return nil
	default:
		return errors.New("worker: queue is full, report will be picked up by poller")
	}
}

// Start launches the worker pool and the fallback poller. It blocks until ctx
// is cancelled. Call it in a goroutine from main:
//
//	go runner.Start(ctx)
func (r *Runner) Start(ctx context.Context) {
	r.logger.Info("worker: starting", "workers", r.cfg.Workers, "poll_interval", r.cfg.PollInterval)

	// Launch worker goroutines.
	for i := range r.cfg.Workers {
		r.wg.Add(1)
		go r.work(ctx, i)
	}

	// Launch fallback poller.
	r.wg.Add(1)
	go r.poll(ctx)

	r.wg.Wait()
	r.logger.Info("worker: stopped")
}

// work is the inner loop for each worker goroutine.
func (r *Runner) work(ctx context.Context, id int) {
	defer r.wg.Done()
	log := r.logger.With("worker_id", id)
	log.Info("worker: goroutine started")

	for {
		select {
		case <-ctx.Done():
			log.Info("worker: goroutine stopping")
			return
		case reportID := <-r.queue:
			r.runWithRetry(ctx, reportID, log)
		}
	}
}

// poll queries the database on PollInterval for any pending/processing reports
// that were not delivered via the channel (e.g. reports from before a restart).
func (r *Runner) poll(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	// Run once immediately on startup to pick up anything from before restart.
	r.pollOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.pollOnce(ctx)
		}
	}
}

func (r *Runner) pollOnce(ctx context.Context) {
	reports, err := r.q.ListPendingReports(ctx)
	if err != nil {
		r.logger.Error("worker: poll failed", "error", err)
		return
	}
	for _, rep := range reports {
		select {
		case r.queue <- rep.ID:
			r.logger.Debug("worker: poller enqueued report", "report_id", rep.ID)
		default:
			// Queue full — will be picked up next poll cycle.
		}
	}
}

// runWithRetry executes the job up to MaxRetries times. After exhausting
// retries it calls store.MarkReportFailed so the report is not picked up again.
func (r *Runner) runWithRetry(ctx context.Context, reportID uuid.UUID, log *slog.Logger) {
	var lastErr error

	for attempt := 1; attempt <= r.cfg.MaxRetries; attempt++ {
		jobCtx, cancel := context.WithTimeout(ctx, r.cfg.JobTimeout)
		lastErr = r.job.Run(jobCtx, reportID)
		cancel()

		if lastErr == nil {
			log.Info("worker: job completed", "report_id", reportID, "attempt", attempt)
			return
		}

		log.Warn("worker: job attempt failed",
			"report_id", reportID,
			"attempt", attempt,
			"max", r.cfg.MaxRetries,
			"error", lastErr,
		)

		if attempt < r.cfg.MaxRetries {
			// Exponential back-off: 2s, 4s, 8s …
			backoff := time.Duration(1<<attempt) * time.Second
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}

	// All retries exhausted — mark the report permanently failed.
	log.Error("worker: job permanently failed", "report_id", reportID, "error", lastErr)
	failCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := r.store.MarkReportFailed(failCtx, reportID, lastErr.Error()); err != nil {
		log.Error("worker: failed to mark report as failed", "report_id", reportID, "error", err)
	}
}
