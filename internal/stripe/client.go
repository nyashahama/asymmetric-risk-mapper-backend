// Package stripe defines the interface for Stripe API calls and webhook
// verification, and provides helpers used by the api and worker packages.
package stripe

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
)

// ─── TYPES ────────────────────────────────────────────────────────────────────

// CreatePaymentIntentParams holds the inputs for creating a Stripe PI.
type CreatePaymentIntentParams struct {
	AmountCents int64
	Currency    string
	Email       string
	Metadata    map[string]string
}

// PaymentIntent is the subset of a Stripe PaymentIntent that callers need.
type PaymentIntent struct {
	ID           string
	ClientSecret string
	CustomerID   string // may be empty if no Customer was created
}

// Event is a parsed Stripe webhook event. DataRaw contains the raw JSON of the
// event's data.object so handlers can unmarshal only what they need.
type Event struct {
	ID      string
	Type    string
	DataRaw json.RawMessage
}

// ─── CLIENT INTERFACE ─────────────────────────────────────────────────────────

// Client is the interface the api and worker packages use for all Stripe calls.
// The concrete implementation wraps the official stripe-go SDK.
// Tests inject a stub.
type Client interface {
	// CreatePaymentIntent creates a new PI and returns its client_secret.
	CreatePaymentIntent(ctx context.Context, p CreatePaymentIntentParams) (PaymentIntent, error)

	// GetClientSecret retrieves the client_secret for an existing PI by ID.
	// Used when the session already has a PI attached (checkout retry path).
	GetClientSecret(ctx context.Context, paymentIntentID string) (string, error)

	// VerifyWebhook validates the Stripe-Signature header and returns the
	// parsed event. Returns an error if the signature is invalid or expired.
	VerifyWebhook(payload []byte, sigHeader string, secret string) (Event, error)
}

// ─── HELPERS USED BY api/ ────────────────────────────────────────────────────

// ToUpsertParams converts a parsed Event and its raw payload into the params
// needed by db.Querier.UpsertStripeEvent. Returns nil if the event was already
// present (ON CONFLICT DO NOTHING path).
func ToUpsertParams(event Event, rawPayload []byte) db.UpsertStripeEventParams {
	return db.UpsertStripeEventParams{
		StripeEventID: event.ID,
		Type:          event.Type,
		Payload:       json.RawMessage(rawPayload),
	}
}

// ToMarkFailedParams builds the params for db.Querier.MarkStripeEventFailed.
func ToMarkFailedParams(eventID string, err error) db.MarkStripeEventFailedParams {
	return db.MarkStripeEventFailedParams{
		StripeEventID: eventID,
		Error:         sql.NullString{String: err.Error(), Valid: true},
	}
}

// ExtractPaymentIntentID pulls the PaymentIntent id field from the event's
// data.object. Works for payment_intent.* events.
func ExtractPaymentIntentID(event Event) (string, error) {
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(event.DataRaw, &obj); err != nil {
		return "", fmt.Errorf("stripe: unmarshal payment intent id: %w", err)
	}
	if obj.ID == "" {
		return "", fmt.Errorf("stripe: payment intent id is empty in event %s", event.ID)
	}
	return obj.ID, nil
}

// ExtractPIFromCharge pulls the payment_intent field from a charge object.
// Works for charge.refunded events.
func ExtractPIFromCharge(event Event) (string, error) {
	var obj struct {
		PaymentIntent string `json:"payment_intent"`
	}
	if err := json.Unmarshal(event.DataRaw, &obj); err != nil {
		return "", fmt.Errorf("stripe: unmarshal charge: %w", err)
	}
	if obj.PaymentIntent == "" {
		return "", fmt.Errorf("stripe: no payment_intent on charge in event %s", event.ID)
	}
	return obj.PaymentIntent, nil
}