// Package email defines the interface for transactional email delivery and
// provides a Resend-backed implementation.
package email

import "context"

// ReportReadyParams holds the data needed to send the report delivery email.
type ReportReadyParams struct {
	To          string // recipient email address
	BizName     string // used in the subject line; may be empty
	AccessToken string // opaque token — inserted into the report URL
}

// ReceiptParams holds the data for the post-payment receipt email.
type ReceiptParams struct {
	To          string
	BizName     string
	AmountCents int64  // e.g. 5900 for $59.00
	Currency    string // e.g. "usd"
}

// Sender is the interface the worker and webhook handler use to send email.
// Tests inject a stub that records calls without hitting the network.
type Sender interface {
	// SendReportReady sends the "your report is ready" email with the access
	// token link. Called by the worker after PersistScoredReport succeeds.
	SendReportReady(ctx context.Context, p ReportReadyParams) error

	// SendReceipt sends the payment receipt. Called by the webhook handler
	// immediately after payment confirmation, before the report is generated.
	SendReceipt(ctx context.Context, p ReceiptParams) error
}