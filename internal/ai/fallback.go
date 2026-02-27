package ai

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/scoring"
)

// fallbackHedger wraps two Hedger implementations. It calls the primary first;
// if that returns an error it logs the failure and tries the secondary.
// This gives you Anthropic as the default with DeepSeek as the safety net
// (or vice versa — the choice is made in main.go).
type fallbackHedger struct {
	primary   Hedger
	secondary Hedger
	logger    *slog.Logger
}

// NewFallbackHedger returns a Hedger that calls primary and, on failure,
// falls back to secondary. Either argument may be nil — if primary is nil
// it goes straight to secondary; if secondary is nil and primary fails, the
// primary error is returned directly.
func NewFallbackHedger(primary, secondary Hedger, logger *slog.Logger) Hedger {
	return &fallbackHedger{
		primary:   primary,
		secondary: secondary,
		logger:    logger,
	}
}

// GenerateHedges tries the primary Hedger. If it fails and a secondary is
// configured, it logs the primary error and tries the secondary.
func (f *fallbackHedger) GenerateHedges(ctx context.Context, risks []scoring.ScoredRisk) (HedgeResult, error) {
	if f.primary != nil {
		result, err := f.primary.GenerateHedges(ctx, risks)
		if err == nil {
			return result, nil
		}
		f.logger.Warn("ai: primary hedger failed, trying secondary",
			"error", err,
			"risks", len(risks),
		)
		if f.secondary == nil {
			return HedgeResult{}, fmt.Errorf("ai: primary failed and no secondary configured: %w", err)
		}
	}

	return f.secondary.GenerateHedges(ctx, risks)
}