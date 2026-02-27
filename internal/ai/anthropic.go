package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/scoring"
)

// anthropicClient is the concrete Hedger backed by the Anthropic Messages API.
type anthropicClient struct {
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewAnthropicClient returns a Hedger that calls the Anthropic API.
//   - apiKey: your ANTHROPIC_API_KEY
//   - model:  e.g. "claude-opus-4-6"
func NewAnthropicClient(apiKey, model string) Hedger {
	return &anthropicClient{
		apiKey: apiKey,
		model:  model,
		httpClient: &http.Client{
			Timeout: 90 * time.Second,
		},
	}
}

// ─── ANTHROPIC API SHAPES ─────────────────────────────────────────────────────

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ─── HEDGE RESULT JSON ────────────────────────────────────────────────────────
// The model is prompted to respond in this exact JSON shape so we can parse
// it without regex heuristics.

type hedgeJSON struct {
	ExecutiveSummary string            `json:"executive_summary"`
	TopPriority      string            `json:"top_priority_html"`
	Hedges           map[string]string `json:"hedges"` // question_id → narrative
}

// ─── IMPLEMENTATION ───────────────────────────────────────────────────────────

const systemPrompt = `You are a risk management advisor for small and medium businesses.
You will receive a list of business risks identified through an assessment questionnaire.
Each risk has a name, description, probability (1-10), impact (1-10), tier (watch/red/manage/ignore), and a static hedge suggestion.

Your job is to produce:
1. An executive_summary: 2-3 sentences summarising the overall risk posture. Be direct and specific.
2. A top_priority_html: a short HTML fragment (1-2 sentences, may use <strong>) identifying the single most urgent action. No <html>, <body>, or block elements — inline only.
3. A hedges object: for each risk (keyed by question_id), write an improved, specific hedge narrative. 2-4 sentences. Focus on concrete actions with rough timelines. Do not pad or repeat the static hedge verbatim.

Respond ONLY with valid JSON matching this exact schema, no markdown fences, no preamble:
{
  "executive_summary": "...",
  "top_priority_html": "...",
  "hedges": {
    "question_id_1": "...",
    "question_id_2": "..."
  }
}`

// GenerateHedges calls the Anthropic API and returns AI-authored hedge
// narratives for the provided risks.
func (c *anthropicClient) GenerateHedges(ctx context.Context, risks []scoring.ScoredRisk) (HedgeResult, error) {
	if len(risks) == 0 {
		return HedgeResult{}, nil
	}

	userPrompt := buildPrompt(risks)

	reqBody := anthropicRequest{
		Model:     c.model,
		MaxTokens: 2048,
		System:    systemPrompt,
		Messages: []anthropicMessage{
			{Role: "user", Content: userPrompt},
		},
	}

	raw, err := c.call(ctx, reqBody)
	if err != nil {
		return HedgeResult{}, err
	}

	// Strip any accidental markdown fences the model may have added.
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var parsed hedgeJSON
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return HedgeResult{}, fmt.Errorf("ai: parse response JSON: %w (raw: %.200s)", err, raw)
	}

	return HedgeResult{
		Hedges:           parsed.Hedges,
		ExecutiveSummary: parsed.ExecutiveSummary,
		TopPriorityHTML:  parsed.TopPriority,
	}, nil
}

// call sends one request to the Anthropic Messages API and returns the
// text content of the first content block.
func (c *anthropicClient) call(ctx context.Context, reqBody anthropicRequest) (string, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ai: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		return "", fmt.Errorf("ai: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ai: http request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return "", fmt.Errorf("ai: read response body: %w", err)
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", fmt.Errorf("ai: unmarshal response: %w", err)
	}

	if parsed.Error != nil {
		return "", fmt.Errorf("ai: API error %s: %s", parsed.Error.Type, parsed.Error.Message)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ai: unexpected status %d: %.200s", resp.StatusCode, string(respBytes))
	}

	for _, block := range parsed.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}

	return "", fmt.Errorf("ai: no text content in response")
}

// buildPrompt serialises the risks into a compact prompt string.
func buildPrompt(risks []scoring.ScoredRisk) string {
	var sb strings.Builder
	sb.WriteString("Here are the business risks to analyse:\n\n")

	for _, r := range risks {
		fmt.Fprintf(&sb, "question_id: %s\n", r.QuestionID)
		fmt.Fprintf(&sb, "name: %s\n", r.RiskName)
		fmt.Fprintf(&sb, "description: %s\n", r.RiskDesc)
		fmt.Fprintf(&sb, "probability: %d/10, impact: %d/10, score: %d, tier: %s\n", r.P, r.I, r.Score, r.Tier)
		fmt.Fprintf(&sb, "static_hedge: %s\n", r.Hedge)
		sb.WriteString("---\n")
	}

	return sb.String()
}