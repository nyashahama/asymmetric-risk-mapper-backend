// Package ai defines the interface for AI-generated hedge narrative generation
// and provides an Anthropic-backed implementation.
package ai

import (
	"context"

	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/scoring"
)

// HedgeResult is the structured output from a successful GenerateHedges call.
type HedgeResult struct {
	// Hedges maps question_id → AI-generated hedge narrative. May be nil if
	// the AI call failed or returned no usable content.
	Hedges map[string]string

	// ExecutiveSummary is a 2–3 sentence plain-English summary of the overall
	// risk posture, suitable for the report header.
	ExecutiveSummary string

	// TopPriorityHTML is a short formatted block (safe HTML) describing the
	// single most urgent action the business owner should take. Rendered
	// directly in the report view.
	TopPriorityHTML string
}

// Hedger is the interface the worker uses to generate AI narratives.
// The concrete implementation lives in anthropic.go (or openai.go).
// Tests inject a stub that returns canned responses.
type Hedger interface {
	// GenerateHedges accepts the watch + red risks for a session and returns
	// AI-authored hedge narratives keyed by question_id, plus the executive
	// summary and top-priority action block.
	//
	// Implementations must be safe to call concurrently.
	// A non-nil error means the entire call failed; the worker will fall back
	// to static hedges from question_definitions.hedge.
	GenerateHedges(ctx context.Context, risks []scoring.ScoredRisk) (HedgeResult, error)
}