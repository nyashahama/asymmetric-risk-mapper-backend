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

// ─── POST /api/webhooks/stripe ────────────────────────────────────────────────

// handleStripeWebhook is the entry point for all Stripe webhook deliveries.
//
// Stripe delivers events at-least-once and may retry on non-2xx responses.
// The handler must be idempotent: every operation it performs uses
// upsert/insert-or-ignore patterns so replays are safe.
//
// The only events we act on are:
//   - payment_intent.succeeded  → initialise report + enqueue scoring job
//   - payment_intent.payment_failed → mark session failed (informational)
//   - charge.refunded           → update payment_status (for analytics)
func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	// ── 1. Read and size-limit the body ───────────────────────────────────────
	// Stripe recommends reading the raw body before any other processing so
	// the signature check runs against the exact bytes Stripe signed.
	r.Body = http.MaxBytesReader(w, r.Body, 65536) // 64 KB — generous for any Stripe event
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		respondErr(w, http.StatusBadRequest, "could not read request body")
		return
	}

	// ── 2. Verify the Stripe-Signature header ─────────────────────────────────
	sig := r.Header.Get("Stripe-Signature")
	event, err := s.stripe.VerifyWebhook(payload, sig, s.cfg.StripeWebhookSecret)
	if err != nil {
		s.logger.Warn("webhook: invalid signature", "error", err, logField(r))
		respondErr(w, http.StatusBadRequest, "invalid webhook signature")
		return
	}

	// ── 3. Idempotency: record the event, skip if already processed ───────────
	// UpsertStripeEvent uses ON CONFLICT DO NOTHING. When a duplicate event_id
	// is received Postgres returns zero rows, which sqlc surfaces as
	// sql.ErrNoRows — not a nil struct. We treat that as an idempotent success
	// and ack immediately so Stripe stops retrying.
	_, err = s.q.UpsertStripeEvent(r.Context(), stripeinternal.ToUpsertParams(event, payload))
	if errors.Is(err, sql.ErrNoRows) {
		s.logger.Debug("webhook: duplicate event, skipping", "event_id", event.ID, logField(r))
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		s.respondInternalErr(w, r, fmt.Errorf("upsert stripe event: %w", err))
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
		// Unknown event type — ack immediately so Stripe stops retrying.
		s.logger.Debug("webhook: unhandled event type", "type", event.Type, logField(r))
	}

	// ── 5. Mark event processed (or failed) ───────────────────────────────────
	if handlerErr != nil {
		s.logger.Error("webhook: handler error",
			"event_id", event.ID,
			"type", event.Type,
			"error", handlerErr,
			logField(r),
		)
		// Record the failure in stripe_events so the poller can investigate.
		_, _ = s.q.MarkStripeEventFailed(r.Context(), stripeinternal.ToMarkFailedParams(event.ID, handlerErr))
		// Return 500 so Stripe retries delivery.
		respondErr(w, http.StatusInternalServerError, "webhook handler failed")
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

	// InitialiseReport atomically marks the session paid and creates the report
	// row. ErrReportAlreadyExists means a duplicate delivery — still a success.
	report, err := s.store.InitialiseReport(r.Context(), piID)
	if errors.Is(err, store.ErrReportAlreadyExists) {
		s.logger.Debug("webhook: report already exists, re-enqueueing if not ready",
			"report_id", report.ID,
			logField(r),
		)
		// Re-enqueue if the report is not yet in a terminal state — handles the
		// case where the worker crashed mid-processing.
		if report.Status != "ready" && report.Status != "error" {
			_ = s.worker.Enqueue(r.Context(), report.ID)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("onPaymentSucceeded: initialise report: %w", err)
	}

	// Send the receipt email immediately — don't wait for the report.
	session, dbErr := s.q.GetSessionByID(r.Context(), report.SessionID)
	if dbErr == nil && session.Email.Valid {
		receiptErr := s.mailer.SendReceipt(r.Context(), email.ReceiptParams{
			To:          session.Email.String,
			BizName:     session.BizName.String,
			AmountCents: 5900,
			Currency:    "usd",
		})
		s.logAndIgnoreEmailErr(r, receiptErr, "send receipt")
	}

	// Enqueue the scoring job. The worker handles errors and retries.
	if err := s.worker.Enqueue(r.Context(), report.ID); err != nil {
		// Enqueueing failed (queue full) — the poller will pick it up.
		s.logger.Warn("webhook: enqueue failed, will be picked up by poller",
			"report_id", report.ID,
			"error", err,
			logField(r),
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
	// Extract the PaymentIntent ID from the charge object inside the event.
	piID, err := stripeinternal.ExtractPIFromCharge(event)
	if err != nil {
		// Refund events without a linked PI are informational only.
		s.logger.Warn("webhook: charge.refunded without PI id", "event_id", event.ID, logField(r))
		return nil
	}

	var rawPayload map[string]json.RawMessage
	if err := json.Unmarshal(event.DataRaw, &rawPayload); err != nil {
		return nil // best-effort only for refund tracking
	}

	// Mark the session refunded using a direct query.
	// There is no sqlc query for this specific update, so we reuse the
	// MarkSessionPaymentFailed path — or add a dedicated query. For now,
	// we log it; add a MarkSessionRefunded query to queries.sql if needed.
	s.logger.Info("webhook: charge refunded",
		"pi_id", piID,
		"event_id", event.ID,
		logField(r),
	)

	return nil
}
