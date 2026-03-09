package api

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/nyashahama/asymmetric-risk-mapper-backend/gen/api/v1"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/store"
	stripeinternal "github.com/nyashahama/asymmetric-risk-mapper-backend/internal/stripe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─── CreateCheckout ───────────────────────────────────────────────────────────
//
// Protected by anonTokenInterceptor.
//
// Creates a Stripe PaymentIntent for the session and returns the client_secret
// to the browser, which passes it to Stripe.js to render the payment UI.
//
// Race-safety: two concurrent calls for the same session are handled by
// store.AttachPaymentIntent using a serializable transaction. The second call
// receives ErrPaymentIntentAlreadyAttached and returns the existing
// client_secret rather than creating a second PI.

func (s *Server) CreateCheckout(
	ctx context.Context,
	req *apiv1.CreateCheckoutRequest,
) (*apiv1.CreateCheckoutResponse, error) {
	sessionID, err := parseUUID(req.SessionId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid session_id")
	}

	if req.Email == "" {
		return nil, status.Error(codes.InvalidArgument, "email is required")
	}

	// ── Fast path: session already has a PI ───────────────────────────────────
	// Optimistically check before calling Stripe to avoid creating an
	// unnecessary PI object. The store transaction is the authoritative guard;
	// this is just an optimisation to skip the Stripe API call in the common
	// retry case.
	existingSession, err := s.q.GetSessionByID(ctx, sessionID)
	if err != nil {
		return nil, s.internalErr(ctx, "CreateCheckout", fmt.Errorf("get session: %w", err))
	}

	if existingSession.StripePaymentIntent.Valid && existingSession.StripePaymentIntent.String != "" {
		clientSecret, err := s.stripe.GetClientSecret(ctx, existingSession.StripePaymentIntent.String)
		if err != nil {
			// PI exists in our DB but Stripe can't find it — unusual.
			// Log and fall through to create a new one.
			s.logger.Warn("CreateCheckout: existing PI not found in Stripe, creating new",
				"pi", existingSession.StripePaymentIntent.String,
				"error", err,
			)
		} else {
			return &apiv1.CreateCheckoutResponse{
				ClientSecret: clientSecret,
				IsExisting:   true,
			}, nil
		}
	}

	// ── Create a new Stripe PaymentIntent ─────────────────────────────────────
	pi, err := s.stripe.CreatePaymentIntent(ctx, stripeinternal.CreatePaymentIntentParams{
		AmountCents: 5900, // $59.00 — fixed price
		Currency:    "usd",
		Email:       req.Email,
		Metadata: map[string]string{
			"session_id": sessionID.String(),
		},
	})
	if err != nil {
		return nil, s.internalErr(ctx, "CreateCheckout", fmt.Errorf("create payment intent: %w", err))
	}

	// ── Atomically attach the PI to the session ───────────────────────────────
	_, err = s.store.AttachPaymentIntent(ctx, store.AttachPaymentIntentParams{
		SessionID:           sessionID,
		StripeCustomerID:    pi.CustomerID,
		StripePaymentIntent: pi.ID,
		Email:               req.Email,
	})

	if errors.Is(err, store.ErrPaymentIntentAlreadyAttached) {
		// Lost the race — another request beat us to it. Fetch the winning PI's
		// client_secret and return it. The PI we just created will expire unused
		// in Stripe after 24h — an acceptable cost of this rare race.
		s.logger.Info("CreateCheckout: lost race, returning existing PI",
			"session_id", sessionID,
		)
		raceSession, dbErr := s.q.GetSessionByID(ctx, sessionID)
		if dbErr != nil {
			return nil, s.internalErr(ctx, "CreateCheckout",
				fmt.Errorf("get session after race: %w", dbErr))
		}
		clientSecret, stripeErr := s.stripe.GetClientSecret(ctx, raceSession.StripePaymentIntent.String)
		if stripeErr != nil {
			return nil, s.internalErr(ctx, "CreateCheckout",
				fmt.Errorf("get client secret after race: %w", stripeErr))
		}
		return &apiv1.CreateCheckoutResponse{
			ClientSecret: clientSecret,
			IsExisting:   true,
		}, nil
	}

	if err != nil {
		return nil, s.internalErr(ctx, "CreateCheckout",
			fmt.Errorf("attach payment intent: %w", err))
	}

	return &apiv1.CreateCheckoutResponse{
		ClientSecret: pi.ClientSecret,
	}, nil
}