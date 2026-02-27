package stripe

import (
	"context"
	"fmt"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/paymentintent"
	"github.com/stripe/stripe-go/v82/webhook"
)

// stripeClient is the concrete implementation of Client backed by the
// official stripe-go SDK. Construct it with NewClient.
type stripeClient struct {
	secretKey string
}

// NewClient returns a Client backed by the Stripe SDK.
// secretKey is your STRIPE_SECRET_KEY env var.
func NewClient(secretKey string) Client {
	return &stripeClient{secretKey: secretKey}
}

// CreatePaymentIntent creates a Stripe Customer (for receipt emails) and a
// PaymentIntent in one call. The Customer ID is stored on the session so
// Stripe's dashboard shows purchases per customer.
func (c *stripeClient) CreatePaymentIntent(ctx context.Context, p CreatePaymentIntentParams) (PaymentIntent, error) {
	stripe.Key = c.secretKey

	// Create or retrieve a Customer so the PI is attached to an email address
	// in the Stripe dashboard.
	custParams := &stripe.CustomerParams{
		Email: stripe.String(p.Email),
	}
	cust, err := customer.New(custParams)
	if err != nil {
		return PaymentIntent{}, fmt.Errorf("stripe: create customer: %w", err)
	}

	// Build metadata including any caller-supplied values.
	meta := make(map[string]string, len(p.Metadata)+1)
	for k, v := range p.Metadata {
		meta[k] = v
	}

	piParams := &stripe.PaymentIntentParams{
		Amount:   stripe.Int64(p.AmountCents),
		Currency: stripe.String(p.Currency),
		Customer: stripe.String(cust.ID),
		// Automatically collect payment method details via Stripe.js.
		AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{
			Enabled: stripe.Bool(true),
		},
		Metadata: meta,
	}
	// Propagate context deadline to the Stripe HTTP call.
	piParams.Context = ctx

	pi, err := paymentintent.New(piParams)
	if err != nil {
		return PaymentIntent{}, fmt.Errorf("stripe: create payment intent: %w", err)
	}

	return PaymentIntent{
		ID:           pi.ID,
		ClientSecret: pi.ClientSecret,
		CustomerID:   cust.ID,
	}, nil
}

// GetClientSecret retrieves the client_secret for an existing PaymentIntent.
// Used when the session already has a PI (checkout retry path).
func (c *stripeClient) GetClientSecret(ctx context.Context, paymentIntentID string) (string, error) {
	stripe.Key = c.secretKey

	params := &stripe.PaymentIntentParams{}
	params.Context = ctx

	pi, err := paymentintent.Get(paymentIntentID, params)
	if err != nil {
		return "", fmt.Errorf("stripe: get payment intent %s: %w", paymentIntentID, err)
	}

	return pi.ClientSecret, nil
}

// VerifyWebhook validates the Stripe-Signature header and returns the parsed
// event. Returns an error if the signature is invalid or the tolerance window
// (300 seconds by default in the Stripe SDK) has expired.
func (c *stripeClient) VerifyWebhook(payload []byte, sigHeader string, secret string) (Event, error) {
	stripeEvent, err := webhook.ConstructEvent(payload, sigHeader, secret)
	if err != nil {
		return Event{}, fmt.Errorf("stripe: webhook verification failed: %w", err)
	}

	return Event{
		ID:      stripeEvent.ID,
		Type:    string(stripeEvent.Type),
		DataRaw: stripeEvent.Data.Raw,
	}, nil
}