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

// deepseekClient is the concrete Hedger backed by the DeepSeek API.
// DeepSeek exposes an OpenAI-compatible /v1/chat/completions endpoint, so the
// request/response shapes are standard OpenAI chat format — not Anthropic's.
type deepseekClient struct {
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewDeepSeekClient returns a Hedger that calls the DeepSeek API.
//   - apiKey: your DEEPSEEK_API_KEY
//   - model:  e.g. "deepseek-chat" or "deepseek-reasoner"
func NewDeepSeekClient(apiKey, model string) Hedger {
	return &deepseekClient{
		apiKey: apiKey,
		model:  model,
		httpClient: &http.Client{
			Timeout: 90 * time.Second,
		},
	}
}

// ─── OPENAI-COMPATIBLE API SHAPES ────────────────────────────────────────────

type openAIRequest struct {
	Model          string          `json:"model"`
	Messages       []openAIMessage `json:"messages"`
	MaxTokens      int             `json:"max_tokens"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// responseFormat instructs the model to return valid JSON.
// DeepSeek honours {"type": "json_object"} the same way OpenAI does.
type responseFormat struct {
	Type string `json:"type"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// ─── IMPLEMENTATION ───────────────────────────────────────────────────────────

// GenerateHedges calls the DeepSeek API and returns AI-authored hedge
// narratives for the provided risks.
func (c *deepseekClient) GenerateHedges(ctx context.Context, risks []scoring.ScoredRisk) (HedgeResult, error) {
	if len(risks) == 0 {
		return HedgeResult{}, nil
	}

	reqBody := openAIRequest{
		Model:     c.model,
		MaxTokens: 2048,
		// json_object mode guarantees the response is valid JSON — no fence stripping needed.
		ResponseFormat: &responseFormat{Type: "json_object"},
		Messages: []openAIMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: buildPrompt(risks)},
		},
	}

	raw, err := c.call(ctx, reqBody)
	if err != nil {
		return HedgeResult{}, err
	}

	// json_object mode should give us clean JSON, but strip fences defensively.
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var parsed hedgeJSON
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return HedgeResult{}, fmt.Errorf("deepseek: parse response JSON: %w (raw: %.200s)", err, raw)
	}

	return HedgeResult{
		Hedges:           parsed.Hedges,
		ExecutiveSummary: parsed.ExecutiveSummary,
		TopPriorityHTML:  parsed.TopPriority,
	}, nil
}

// call sends one request to the DeepSeek chat completions endpoint and returns
// the text content of the first choice.
func (c *deepseekClient) call(ctx context.Context, reqBody openAIRequest) (string, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("deepseek: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.deepseek.com/v1/chat/completions",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		return "", fmt.Errorf("deepseek: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("deepseek: http request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("deepseek: read response: %w", err)
	}

	var parsed openAIResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", fmt.Errorf("deepseek: unmarshal response: %w", err)
	}

	if parsed.Error != nil {
		return "", fmt.Errorf("deepseek: API error %s: %s", parsed.Error.Type, parsed.Error.Message)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("deepseek: unexpected status %d: %.200s", resp.StatusCode, string(respBytes))
	}

	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("deepseek: no choices in response")
	}

	return parsed.Choices[0].Message.Content, nil
}