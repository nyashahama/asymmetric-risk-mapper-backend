package stripe_test

import (
	"encoding/json"
	"testing"

	stripeinternal "github.com/nyashahama/asymmetric-risk-mapper-backend/internal/stripe"
)

// ─── ExtractPaymentIntentID ───────────────────────────────────────────────────

func TestExtractPaymentIntentID_Success(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"id":     "pi_abc123",
		"object": "payment_intent",
		"status": "succeeded",
	})

	event := stripeinternal.Event{
		ID:      "evt_test",
		Type:    "payment_intent.succeeded",
		DataRaw: json.RawMessage(raw),
	}

	piID, err := stripeinternal.ExtractPaymentIntentID(event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if piID != "pi_abc123" {
		t.Errorf("expected pi_abc123, got %q", piID)
	}
}

func TestExtractPaymentIntentID_EmptyIDReturnsError(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"id": "", "object": "payment_intent"})
	event := stripeinternal.Event{DataRaw: json.RawMessage(raw)}

	_, err := stripeinternal.ExtractPaymentIntentID(event)
	if err == nil {
		t.Error("expected error for empty id, got nil")
	}
}

func TestExtractPaymentIntentID_MalformedJSONReturnsError(t *testing.T) {
	event := stripeinternal.Event{DataRaw: json.RawMessage(`{bad json`)}

	_, err := stripeinternal.ExtractPaymentIntentID(event)
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
}

// ─── ExtractPIFromCharge ──────────────────────────────────────────────────────

func TestExtractPIFromCharge_Success(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"id":             "ch_test123",
		"object":         "charge",
		"payment_intent": "pi_abc456",
	})

	event := stripeinternal.Event{
		ID:      "evt_refund",
		Type:    "charge.refunded",
		DataRaw: json.RawMessage(raw),
	}

	piID, err := stripeinternal.ExtractPIFromCharge(event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if piID != "pi_abc456" {
		t.Errorf("expected pi_abc456, got %q", piID)
	}
}

func TestExtractPIFromCharge_MissingPIReturnsError(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"id": "ch_test", "object": "charge"})
	event := stripeinternal.Event{DataRaw: json.RawMessage(raw)}

	_, err := stripeinternal.ExtractPIFromCharge(event)
	if err == nil {
		t.Error("expected error when payment_intent is missing")
	}
}

func TestExtractPIFromCharge_EmptyPIReturnsError(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"payment_intent": ""})
	event := stripeinternal.Event{DataRaw: json.RawMessage(raw)}

	_, err := stripeinternal.ExtractPIFromCharge(event)
	if err == nil {
		t.Error("expected error for empty payment_intent")
	}
}

// ─── ToUpsertParams ───────────────────────────────────────────────────────────

func TestToUpsertParams_SetsAllFields(t *testing.T) {
	payload := []byte(`{"id":"evt_123","type":"payment_intent.succeeded"}`)
	event := stripeinternal.Event{
		ID:   "evt_123",
		Type: "payment_intent.succeeded",
	}

	params := stripeinternal.ToUpsertParams(event, payload)

	if params.StripeEventID != "evt_123" {
		t.Errorf("StripeEventID: got %q", params.StripeEventID)
	}
	if params.Type != "payment_intent.succeeded" {
		t.Errorf("Type: got %q", params.Type)
	}
	if string(params.Payload) != string(payload) {
		t.Errorf("Payload mismatch")
	}
}

// ─── ToMarkFailedParams ───────────────────────────────────────────────────────

func TestToMarkFailedParams_SetsErrorMessage(t *testing.T) {
	testErr := &testError{"something went wrong"}
	params := stripeinternal.ToMarkFailedParams("evt_456", testErr)

	if params.StripeEventID != "evt_456" {
		t.Errorf("StripeEventID: got %q", params.StripeEventID)
	}
	if !params.Error.Valid {
		t.Error("expected Error.Valid=true")
	}
	if params.Error.String != "something went wrong" {
		t.Errorf("error message: got %q", params.Error.String)
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }