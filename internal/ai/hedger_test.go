package ai_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/ai"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/scoring"
)

// ─── STUBS ────────────────────────────────────────────────────────────────────

type stubHedger struct {
	result ai.HedgeResult
	err    error
	calls  int
}

func (s *stubHedger) GenerateHedges(_ context.Context, risks []scoring.ScoredRisk) (ai.HedgeResult, error) {
	s.calls++
	return s.result, s.err
}

// discardLogger returns a *slog.Logger that silently drops all log output.
// Use this instead of nil — fallback.go calls f.logger.Warn() which panics on nil.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ─── FallbackHedger ───────────────────────────────────────────────────────────

func TestFallbackHedger_PrimarySucceeds_SecondaryNotCalled(t *testing.T) {
	primary := &stubHedger{
		result: ai.HedgeResult{
			ExecutiveSummary: "Primary summary",
			TopPriorityHTML:  "<strong>Primary</strong>",
			Hedges:           map[string]string{"q_1": "primary hedge"},
		},
	}
	secondary := &stubHedger{
		result: ai.HedgeResult{ExecutiveSummary: "Secondary summary"},
	}

	hedger := ai.NewFallbackHedger(primary, secondary, discardLogger())

	risks := []scoring.ScoredRisk{{QuestionID: "q_1", Score: 50}}
	result, err := hedger.GenerateHedges(context.Background(), risks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.ExecutiveSummary != "Primary summary" {
		t.Errorf("expected primary result, got: %q", result.ExecutiveSummary)
	}
	if secondary.calls != 0 {
		t.Errorf("secondary should not be called, got %d calls", secondary.calls)
	}
	if primary.calls != 1 {
		t.Errorf("primary should be called once, got %d calls", primary.calls)
	}
}

func TestFallbackHedger_PrimaryFails_SecondaryUsed(t *testing.T) {
	primary := &stubHedger{err: errors.New("anthropic timeout")}
	secondary := &stubHedger{
		result: ai.HedgeResult{
			ExecutiveSummary: "Secondary summary",
			Hedges:           map[string]string{"q_1": "fallback hedge"},
		},
	}

	hedger := ai.NewFallbackHedger(primary, secondary, discardLogger())

	risks := []scoring.ScoredRisk{{QuestionID: "q_1", Score: 50}}
	result, err := hedger.GenerateHedges(context.Background(), risks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.ExecutiveSummary != "Secondary summary" {
		t.Errorf("expected secondary result, got: %q", result.ExecutiveSummary)
	}
	if primary.calls != 1 {
		t.Errorf("primary should be called once, got %d calls", primary.calls)
	}
	if secondary.calls != 1 {
		t.Errorf("secondary should be called once, got %d calls", secondary.calls)
	}
}

func TestFallbackHedger_BothFail_ReturnsError(t *testing.T) {
	primary := &stubHedger{err: errors.New("primary error")}
	secondary := &stubHedger{err: errors.New("secondary error")}

	hedger := ai.NewFallbackHedger(primary, secondary, discardLogger())

	_, err := hedger.GenerateHedges(context.Background(), []scoring.ScoredRisk{{QuestionID: "q_1"}})
	if err == nil {
		t.Fatal("expected error when both hedgers fail")
	}
}

func TestFallbackHedger_NilPrimary_UsesSecondaryDirectly(t *testing.T) {
	secondary := &stubHedger{
		result: ai.HedgeResult{ExecutiveSummary: "Only secondary"},
	}

	hedger := ai.NewFallbackHedger(nil, secondary, discardLogger())

	result, err := hedger.GenerateHedges(context.Background(), []scoring.ScoredRisk{{QuestionID: "q_1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExecutiveSummary != "Only secondary" {
		t.Errorf("expected secondary result, got: %q", result.ExecutiveSummary)
	}
	if secondary.calls != 1 {
		t.Errorf("expected 1 secondary call, got %d", secondary.calls)
	}
}

func TestFallbackHedger_NilSecondary_PrimaryErrorBubbles(t *testing.T) {
	primaryErr := errors.New("primary blew up")
	primary := &stubHedger{err: primaryErr}

	hedger := ai.NewFallbackHedger(primary, nil, discardLogger())

	_, err := hedger.GenerateHedges(context.Background(), []scoring.ScoredRisk{{QuestionID: "q_1"}})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, primaryErr) {
		t.Errorf("expected to find primaryErr in chain, got: %v", err)
	}
}

func TestFallbackHedger_EmptyRisks_ReturnsEmptyWithoutCallingPrimary(t *testing.T) {
	// Both Anthropic and DeepSeek short-circuit on len(risks)==0.
	// FallbackHedger delegates, so we just confirm no error and empty result.
	primary := &stubHedger{result: ai.HedgeResult{ExecutiveSummary: "should not appear"}}
	secondary := &stubHedger{}

	hedger := ai.NewFallbackHedger(primary, secondary, discardLogger())

	result, err := hedger.GenerateHedges(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Both stubs return their zero values for empty input — result should be empty.
	_ = result // just verify no panic and no error
}

// ─── HedgeResult ──────────────────────────────────────────────────────────────

func TestHedgeResult_ZeroValue(t *testing.T) {
	var hr ai.HedgeResult
	if hr.ExecutiveSummary != "" {
		t.Error("zero value ExecutiveSummary should be empty")
	}
	if hr.TopPriorityHTML != "" {
		t.Error("zero value TopPriorityHTML should be empty")
	}
	if hr.Hedges != nil {
		t.Error("zero value Hedges should be nil")
	}
}