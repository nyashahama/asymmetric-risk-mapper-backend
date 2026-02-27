// Package api implements the HTTP layer for the Asymmetric Risk Mapper.
// Handlers are methods on *Server. Each handler file is responsible for one
// resource group and only imports the dependencies it actually uses.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/email"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/store"
	stripeinternal "github.com/nyashahama/asymmetric-risk-mapper-backend/internal/stripe"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/worker"
)

// Config holds values read from environment variables at startup.
type Config struct {
	// BaseURL is used to construct the report access link in emails.
	// e.g. "https://app.asymmetricrisk.com"
	BaseURL string

	// StripeWebhookSecret is the signing secret from the Stripe dashboard.
	StripeWebhookSecret string

	// Env is "production", "staging", or "development".
	Env string
}

// Server holds all shared dependencies. Each handler file attaches methods to
// this type and uses only the fields it needs.
type Server struct {
	// q handles all single-query reads. Injected directly — no repo wrapper.
	q db.Querier

	// store handles multi-step atomic writes.
	store *store.Store

	// stripe creates PaymentIntents and verifies webhook signatures.
	stripe stripeinternal.Client

	// worker enqueues scoring jobs after payment confirmation.
	worker worker.Enqueuer

	// mailer sends transactional emails (receipt + report delivery).
	mailer email.Sender

	cfg    Config
	logger *slog.Logger
}

// NewServer constructs the Server and wires the chi router. The returned
// http.Handler is ready to pass to http.ListenAndServe.
func NewServer(
	q db.Querier,
	st *store.Store,
	stripeClient stripeinternal.Client,
	enqueuer worker.Enqueuer,
	mailer email.Sender,
	cfg Config,
	logger *slog.Logger,
) http.Handler {
	s := &Server{
		q:      q,
		store:  st,
		stripe: stripeClient,
		worker: enqueuer,
		mailer: mailer,
		cfg:    cfg,
		logger: logger,
	}

	return s.routes()
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()

	// ── Global middleware ─────────────────────────────────────────────────────
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(s.loggerMiddleware)
	r.Use(middleware.Recoverer)
	r.Use(s.corsMiddleware)
	r.Use(middleware.Timeout(30 * time.Second))

	// ── Health ────────────────────────────────────────────────────────────────
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// ── API v1 ────────────────────────────────────────────────────────────────
	r.Route("/api", func(r chi.Router) {

		// Sessions — no auth required (anonymous creation).
		r.Post("/session", s.handleCreateSession)

		// Session-scoped routes — require valid anon_token cookie/header.
		r.Route("/session/{sessionID}", func(r chi.Router) {
			r.Use(s.requireAnonToken)
			r.Patch("/context", s.handleUpdateContext)
			r.Put("/answers", s.handleUpsertAnswers)
			r.Post("/checkout", s.handleCreateCheckout)
		})

		// Stripe webhook — no auth (signature verification inside handler).
		r.Post("/webhooks/stripe", s.handleStripeWebhook)

		// Report access — no auth (opaque access token in URL).
		r.Get("/report/{accessToken}", s.handleGetReport)
	})

	return r
}