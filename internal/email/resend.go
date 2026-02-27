package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// resendClient is the concrete Sender backed by the Resend API.
type resendClient struct {
	apiKey     string
	fromAddr   string // e.g. "reports@asymmetricrisk.com"
	fromName   string // e.g. "Asymmetric Risk"
	baseURL    string // report access URL base, e.g. "https://app.asymmetricrisk.com"
	httpClient *http.Client
}

// NewResendClient returns a Sender that delivers email via Resend.
func NewResendClient(apiKey, fromAddr, fromName, baseURL string) Sender {
	return &resendClient{
		apiKey:   apiKey,
		fromAddr: fromAddr,
		fromName: fromName,
		baseURL:  baseURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// ─── RESEND API SHAPES ────────────────────────────────────────────────────────

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
}

type resendResponse struct {
	ID    string `json:"id"`
	Error *struct {
		Name       string `json:"name"`
		Message    string `json:"message"`
		StatusCode int    `json:"statusCode"`
	} `json:"error"`
}

// ─── SENDER IMPLEMENTATION ────────────────────────────────────────────────────

// SendReportReady sends the "your report is ready" delivery email.
func (c *resendClient) SendReportReady(ctx context.Context, p ReportReadyParams) error {
	subject := "Your Risk Assessment is Ready"
	if p.BizName != "" {
		subject = fmt.Sprintf("%s — Your Risk Assessment is Ready", p.BizName)
	}

	reportURL := fmt.Sprintf("%s/report/%s", c.baseURL, p.AccessToken)

	html := reportReadyHTML(p.BizName, reportURL)

	return c.send(ctx, p.To, subject, html)
}

// SendReceipt sends the post-payment receipt email.
func (c *resendClient) SendReceipt(ctx context.Context, p ReceiptParams) error {
	subject := "Your payment was received"
	if p.BizName != "" {
		subject = fmt.Sprintf("%s — Payment Confirmed", p.BizName)
	}

	amount := fmt.Sprintf("$%.2f", float64(p.AmountCents)/100)
	html := receiptHTML(p.BizName, amount)

	return c.send(ctx, p.To, subject, html)
}

// ─── HTTP SEND ────────────────────────────────────────────────────────────────

func (c *resendClient) send(ctx context.Context, to, subject, html string) error {
	from := fmt.Sprintf("%s <%s>", c.fromName, c.fromAddr)

	reqBody := resendRequest{
		From:    from,
		To:      []string{to},
		Subject: subject,
		HTML:    html,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("email: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.resend.com/emails",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		return fmt.Errorf("email: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("email: http request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("email: read response: %w", err)
	}

	var parsed resendResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return fmt.Errorf("email: unmarshal response (status %d): %w", resp.StatusCode, err)
	}

	if parsed.Error != nil {
		return fmt.Errorf("email: Resend error %s: %s", parsed.Error.Name, parsed.Error.Message)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("email: unexpected status %d: %.200s", resp.StatusCode, string(respBytes))
	}

	return nil
}

// ─── HTML TEMPLATES ───────────────────────────────────────────────────────────

func reportReadyHTML(bizName, reportURL string) string {
	greeting := "Hello"
	if bizName != "" {
		greeting = fmt.Sprintf("Hello %s", bizName)
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family: sans-serif; color: #1a1a1a; max-width: 560px; margin: 0 auto; padding: 24px;">
  <h2 style="margin-bottom: 8px;">Your Risk Assessment is Ready</h2>
  <p>%s,</p>
  <p>Your Asymmetric Risk assessment has been completed. Your personalised report
  identifies your highest-priority risks and includes tailored mitigation strategies.</p>
  <p style="margin: 32px 0;">
    <a href="%s"
       style="background: #0f172a; color: #ffffff; padding: 12px 24px;
              border-radius: 6px; text-decoration: none; font-weight: 600;">
      View Your Report
    </a>
  </p>
  <p style="color: #6b7280; font-size: 14px;">
    Bookmark this link — it is your permanent access to your report.<br>
    If the button above does not work, copy this URL:<br>
    <a href="%s" style="color: #6b7280;">%s</a>
  </p>
  <hr style="border: none; border-top: 1px solid #e5e7eb; margin: 32px 0;">
  <p style="color: #9ca3af; font-size: 12px;">
    Asymmetric Risk Mapper · One-time assessment · No account required
  </p>
</body>
</html>`, greeting, reportURL, reportURL, reportURL)
}

func receiptHTML(bizName, amount string) string {
	greeting := "Hello"
	if bizName != "" {
		greeting = fmt.Sprintf("Hello %s", bizName)
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family: sans-serif; color: #1a1a1a; max-width: 560px; margin: 0 auto; padding: 24px;">
  <h2 style="margin-bottom: 8px;">Payment Confirmed</h2>
  <p>%s,</p>
  <p>We have received your payment of <strong>%s</strong> for the
  Asymmetric Risk assessment. Your report is now being generated and you
  will receive a separate email with a link to view it shortly.</p>
  <p style="color: #6b7280; font-size: 14px;">
    If you have any questions, reply to this email.
  </p>
  <hr style="border: none; border-top: 1px solid #e5e7eb; margin: 32px 0;">
  <p style="color: #9ca3af; font-size: 12px;">
    Asymmetric Risk Mapper · One-time assessment · No account required
  </p>
</body>
</html>`, greeting, amount)
}