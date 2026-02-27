package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/store"
	stripeinternal "github.com/nyashahama/asymmetric-risk-mapper-backend/internal/stripe"
)

// ─── POST /api/session/:sessionID/checkout ────────────────────────────────────

type createCheckoutRequest struct {
	Email string `json:"email"`
}

type createCheckoutResponse struct {
	// ClientSecret is the Stripe PaymentIntent client_secret. The browser
	// passes this to Stripe.js to render the payment UI and confirm the charge.
	ClientSecret string `json:"client_secret"`
	// IsExisting is true when the session already had a PaymentIntent (i.e. the
	// user opened checkout twice). The browser should use the returned secret
	// normally — the PI is still valid and confirmable.
	IsExisting bool `json:"is_existing,omitempty"`
}

// handleCreateCheckout creates a Stripe PaymentIntent for the session and
// returns the client_secret to the browser.
//
// Race-safety: two concurrent calls for the same session are handled by
// store.AttachPaymentIntent using a serializable transaction. The second call
// receives ErrPaymentIntentAlreadyAttached and returns the existing
// client_secret rather than creating a second PI.
func (s *Server) handleCreateCheckout(w http.ResponseWriter, r *http.Request) {
	sessionID, err := parseUUID(chi.URLParam(r, "sessionID"))
	if err != nil {
		respondErr(w, http.StatusBadRequest, "invalid session_id")
		return
	}

	var req createCheckoutRequest
	if !decode(w, r, &req) {
		return
	}

	if req.Email == "" {
		respondErr(w, http.StatusBadRequest, "email is required")
		return
	}

	// ── Fast path: session already has a PI ───────────────────────────────────
	// Check before calling Stripe to avoid creating an unnecessary PI object.
	// The store transaction is the authoritative guard; this is just an
	// optimisation to skip the Stripe API call in the common retry case.
	existingSession, err := s.q.GetSessionByID(r.Context(), sessionID)
	if err != nil {
		s.respondInternalErr(w, r, fmt.Errorf("get session: %w", err))
		return
	}

	if existingSession.StripePaymentIntent.Valid && existingSession.StripePaymentIntent.String != "" {
		clientSecret, err := s.stripe.GetClientSecret(r.Context(), existingSession.StripePaymentIntent.String)
		if err != nil {
			// PI exists in our DB but Stripe can't find it — unusual.
			// Fall through to create a new one.
			s.logger.Warn("checkout: existing PI not found in Stripe, creating new",
				"pi", existingSession.StripePaymentIntent.String,
				"error", err,
				logField(r),
			)
		} else {
			respond(w, http.StatusOK, createCheckoutResponse{
				ClientSecret: clientSecret,
				IsExisting:   true,
			})
			return
		}
	}

	// ── Create a new Stripe PaymentIntent ─────────────────────────────────────
	pi, err := s.stripe.CreatePaymentIntent(r.Context(), stripeinternal.CreatePaymentIntentParams{
		AmountCents: 5900, // $59.00 — fixed price
		Currency:    "usd",
		Email:       req.Email,
		Metadata: map[string]string{
			"session_id": sessionID.String(),
		},
	})
	if err != nil {
		s.respondInternalErr(w, r, fmt.Errorf("create payment intent: %w", err))
		return
	}

	// ── Atomically attach the PI to the session ───────────────────────────────
	_, err = s.store.AttachPaymentIntent(r.Context(), store.AttachPaymentIntentParams{
		SessionID:           sessionID,
		StripeCustomerID:    pi.CustomerID,
		StripePaymentIntent: pi.ID,
		Email:               req.Email,
	})

	if errors.Is(err, store.ErrPaymentIntentAlreadyAttached) {
		// Lost the race — another request beat us to it. Fetch the winning PI's
		// client_secret and return it. The PI we just created will expire unused
		// in Stripe after 24h — an acceptable cost of this rare race.
		s.logger.Info("checkout: lost race, returning existing PI",
			"session_id", sessionID,
			logField(r),
		)
		session, dbErr := s.q.GetSessionByID(r.Context(), sessionID)
		if dbErr != nil {
			s.respondInternalErr(w, r, fmt.Errorf("get session after race: %w", dbErr))
			return
		}
		clientSecret, stripeErr := s.stripe.GetClientSecret(r.Context(), session.StripePaymentIntent.String)
		if stripeErr != nil {
			s.respondInternalErr(w, r, fmt.Errorf("get client secret after race: %w", stripeErr))
			return
		}
		respond(w, http.StatusOK, createCheckoutResponse{
			ClientSecret: clientSecret,
			IsExisting:   true,
		})
		return
	}

	if err != nil {
		s.respondInternalErr(w, r, fmt.Errorf("attach payment intent: %w", err))
		return
	}

	respond(w, http.StatusOK, createCheckoutResponse{
		ClientSecret: pi.ClientSecret,
	})
}