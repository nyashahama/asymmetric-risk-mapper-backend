package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
)

// ─── INPUT TYPES ─────────────────────────────────────────────────────────────

// AttachPaymentIntentParams groups the Stripe and email fields written
// together when checkout is initiated.
type AttachPaymentIntentParams struct {
	SessionID           uuid.UUID
	StripeCustomerID    string
	StripePaymentIntent string
	Email               string
}

// ─── ERRORS ──────────────────────────────────────────────────────────────────

// ErrPaymentIntentAlreadyAttached is returned when a session already has a
// Stripe PaymentIntent set. The checkout handler should treat this as a
// recoverable condition and return the existing client_secret to the browser
// rather than creating a second PaymentIntent.
var ErrPaymentIntentAlreadyAttached = errors.New("store: payment intent already attached to session")

// ─── METHODS ─────────────────────────────────────────────────────────────────

// AttachPaymentIntent atomically guards against double-attachment of a Stripe
// PaymentIntent to a session, then writes the customer ID, PI, and email.
//
// Race scenario without this guard:
//  1. Two browser tabs call POST /checkout simultaneously.
//  2. Both read the session, see no PI, and call Stripe.
//  3. Both try to write — the second write silently overwrites the first PI,
//     orphaning a Stripe PaymentIntent that will never be confirmed.
//
// With serializable isolation the second concurrent transaction will see the
// first commit and hit the already-attached check, returning
// ErrPaymentIntentAlreadyAttached. The handler then reads the existing PI from
// the session and returns its client_secret — no orphaned object, no charge.
func (s *Store) AttachPaymentIntent(ctx context.Context, p AttachPaymentIntentParams) (db.Session, error) {
	var session db.Session

	err := s.withTx(ctx, func(ctx context.Context, q db.Querier) error {
		// Re-read the session inside the transaction so we see the latest
		// committed state under serializable isolation.
		existing, err := q.GetSessionByID(ctx, p.SessionID)
		if err != nil {
			return fmt.Errorf("AttachPaymentIntent: get session: %w", err)
		}

		// Guard: if a PI is already set, surface the sentinel error. The handler
		// must still return HTTP 200 with the existing client_secret — a second
		// tab opening checkout is not a hard error for the user.
		if existing.StripePaymentIntent.Valid && existing.StripePaymentIntent.String != "" {
			session = existing
			return ErrPaymentIntentAlreadyAttached
		}

		updated, err := q.AttachStripeCustomer(ctx, db.AttachStripeCustomerParams{
			ID: p.SessionID,
			StripeCustomerID: sql.NullString{
				String: p.StripeCustomerID,
				Valid:  p.StripeCustomerID != "",
			},
			StripePaymentIntent: sql.NullString{
				String: p.StripePaymentIntent,
				Valid:  true,
			},
			Email: sql.NullString{
				String: p.Email,
				Valid:  p.Email != "",
			},
		})
		if err != nil {
			return fmt.Errorf("AttachPaymentIntent: attach stripe customer: %w", err)
		}

		session = updated
		return nil
	})

	// Unwrap the sentinel so callers can check with errors.Is without needing
	// to look inside a wrapped error chain.
	if errors.Is(err, ErrPaymentIntentAlreadyAttached) {
		return session, ErrPaymentIntentAlreadyAttached
	}
	if err != nil {
		return db.Session{}, err
	}

	return session, nil
}