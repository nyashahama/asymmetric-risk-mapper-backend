package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/email"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/store"
	stripeinternal "github.com/nyashahama/asymmetric-risk-mapper-backend/internal/stripe"
)

// StripeWebhookHandler returns an http.Handler for the Stripe webhook endpoint.
//
// WHY HTTP/1.1 AND NOT gRPC:
// Stripe POSTs raw JSON with a Stripe-Signature header to a plain HTTPS URL.
// It has no gRPC support and cannot be proxied through grpc-gateway without
// losing the raw body required for HMAC verification. We mount this handler
// on a separate HTTP/1.1 listener in main.go using cmux, keeping it completely
// isolated from the gRPC listener.
//
// The handler logic is identical to the old REST implementation — all business
// logic unchanged, only the wiring differs.
//
// Idempotency: Stripe delivers events at-least-once and retries on non-2xx.
// Every operation uses upsert/insert-or-ignore so replays are safe.
func (s *Server) StripeWebhookHandler() http.Handler {
	return http.HandlerFunc(s.handleStripeWebhook)
}

func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	// ── 1. Read and size-limit the body ───────────────────────────────────────
	r.Body = http.MaxBytesReader(w, r.Body, 65536) // 64 KB
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "could not read request body")
		return
	}

	// ── 2. Verify the Stripe-Signature header ─────────────────────────────────
	sig := r.Header.Get("Stripe-Signature")
	event, err := s.stripe.VerifyWebhook(payload, sig, s.cfg.StripeWebhookSecret)
	if err != nil {
		s.logger.Warn("StripeWebhook: invalid signature", "error", err)
		httpErr(w, http.StatusBadRequest, "invalid webhook signature")
		return
	}

	// ── 3. Idempotency: record the event, skip if already processed ───────────
	// UpsertStripeEvent uses ON CONFLICT DO NOTHING. Duplicate event_id →
	// sql.ErrNoRows from sqlc — treat as idempotent success.
	_, err = s.q.UpsertStripeEvent(r.Context(), stripeinternal.ToUpsertParams(event, payload))
	if errors.Is(err, sql.ErrNoRows) {
		s.logger.Debug("StripeWebhook: duplicate event, skipping", "event_id", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		s.logger.Error("StripeWebhook: upsert event failed", "error", err)
		httpErr(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// ── 4. Dispatch by event type ─────────────────────────────────────────────
	var handlerErr error

	switch event.Type {
	case "payment_intent.succeeded":
		handlerErr = s.onPaymentSucceeded(r, event)

	case "payment_intent.payment_failed":
		handlerErr = s.onPaymentFailed(r, event)

	case "charge.refunded":
		handlerErr = s.onChargeRefunded(r, event)

	default:
		s.logger.Debug("StripeWebhook: unhandled event type", "type", event.Type)
	}

	// ── 5. Mark event processed or failed ────────────────────────────────────
	if handlerErr != nil {
		s.logger.Error("StripeWebhook: handler error",
			"event_id", event.ID,
			"type", event.Type,
			"error", handlerErr,
		)
		_, _ = s.q.MarkStripeEventFailed(r.Context(),
			stripeinternal.ToMarkFailedParams(event.ID, handlerErr))
		// Return 500 so Stripe retries delivery.
		httpErr(w, http.StatusInternalServerError, "webhook handler failed")
		return
	}

	_, _ = s.q.MarkStripeEventProcessed(r.Context(), event.ID)
	w.WriteHeader(http.StatusOK)
}

// ─── EVENT HANDLERS ───────────────────────────────────────────────────────────

func (s *Server) onPaymentSucceeded(r *http.Request, event stripeinternal.Event) error {
	piID, err := stripeinternal.ExtractPaymentIntentID(event)
	if err != nil {
		return fmt.Errorf("onPaymentSucceeded: extract PI id: %w", err)
	}

	report, err := s.store.InitialiseReport(r.Context(), piID)
	if errors.Is(err, store.ErrReportAlreadyExists) {
		s.logger.Debug("StripeWebhook: report already exists, re-enqueueing if not ready",
			"report_id", report.ID,
		)
		if report.Status != "ready" && report.Status != "error" {
			_ = s.worker.Enqueue(r.Context(), report.ID)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("onPaymentSucceeded: initialise report: %w", err)
	}

	// Send receipt email immediately — don't wait for the report.
	session, dbErr := s.q.GetSessionByID(r.Context(), report.SessionID)
	if dbErr == nil && session.Email.Valid {
		receiptErr := s.mailer.SendReceipt(r.Context(), email.ReceiptParams{
			To:          session.Email.String,
			BizName:     session.BizName.String,
			AmountCents: 5900,
			Currency:    "usd",
		})
		s.logAndIgnoreEmailErr("StripeWebhook/onPaymentSucceeded", receiptErr, "send receipt")
	}

	if err := s.worker.Enqueue(r.Context(), report.ID); err != nil {
		s.logger.Warn("StripeWebhook: enqueue failed, will be picked up by poller",
			"report_id", report.ID,
			"error", err,
		)
	}

	return nil
}

func (s *Server) onPaymentFailed(r *http.Request, event stripeinternal.Event) error {
	piID, err := stripeinternal.ExtractPaymentIntentID(event)
	if err != nil {
		return fmt.Errorf("onPaymentFailed: extract PI id: %w", err)
	}

	_, err = s.q.MarkSessionPaymentFailed(r.Context(), sql.NullString{
		String: piID,
		Valid:  true,
	})
	if err != nil {
		return fmt.Errorf("onPaymentFailed: mark session failed: %w", err)
	}

	return nil
}

func (s *Server) onChargeRefunded(r *http.Request, event stripeinternal.Event) error {
	piID, err := stripeinternal.ExtractPIFromCharge(event)
	if err != nil {
		s.logger.Warn("StripeWebhook: charge.refunded without PI id", "event_id", event.ID)
		return nil
	}

	var rawPayload map[string]json.RawMessage
	if err := json.Unmarshal(event.DataRaw, &rawPayload); err != nil {
		return nil // best-effort only
	}

	s.logger.Info("StripeWebhook: charge refunded",
		"pi_id", piID,
		"event_id", event.ID,
	)

	return nil
}

// ─── HTTP HELPERS (webhook-only) ─────────────────────────────────────────────

// httpErr writes a plain JSON error response for the HTTP/1.1 webhook handler.
// Keep this separate from gRPC status errors — they serve different transports.
func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, msg)
}