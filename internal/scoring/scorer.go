package scoring

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ─── CONSTANTS ────────────────────────────────────────────────────────────────

// Tier thresholds mirror risks.ts getRiskTier().
const (
	highImpactThreshold = 7 // i >= 7  → high impact
	highProbThreshold   = 6 // p >= 6  → high probability
)

// ─── TYPES ────────────────────────────────────────────────────────────────────

// RiskTier is the four-bucket classification. String values deliberately match
// the Postgres enum so they can be cast to db.RiskTier without conversion.
type RiskTier string

const (
	TierWatch  RiskTier = "watch"  // high prob + high impact — on fire, slowly
	TierRed    RiskTier = "red"    // low prob  + high impact — hedge now
	TierManage RiskTier = "manage" // high prob + low impact  — handle operationally
	TierIgnore RiskTier = "ignore" // low prob  + low impact  — not worth attention
)

// ScoredRisk is the fully computed output for a single question.
// Its field types are intentionally plain Go types so that it can be used
// without importing the db package — keeping scoring/ dependency-free.
//
// The worker maps this to db.InsertRiskResultParams for persistence.
type ScoredRisk struct {
	QuestionID string   // matches question_definitions.id
	Rank       int      // 1-indexed after sort; set by ComputeRisks
	RiskName   string   // question_definitions.risk_name
	RiskDesc   string   // question_definitions.risk_desc
	Hedge      string   // question_definitions.hedge (static)
	Section    string   // question_definitions.section_title
	P          int      // probability 1–10
	I          int      // impact      1–10
	Score      int      // P × I, max 100
	Tier       RiskTier
}

// AnswerRow is the minimal slice of db.GetAnswersBySessionRow that the scoring
// package needs. Using a local interface type keeps scoring/ import-free from
// the db package while remaining easy to construct in tests.
type AnswerRow struct {
	QuestionID    string
	AnswerText    string
	SectionTitle  string  // maps to db.GetAnswersBySessionRow.SectionID (used as label)
	RiskName      string
	RiskDesc      string
	Hedge         string
	ScoringConfig json.RawMessage
	IsScoring     bool
}

// ─── CORE FUNCTIONS ───────────────────────────────────────────────────────────

// clamp constrains a score value to [1, 10], matching risks.ts clamp().
func clamp(v int) int {
	if v < 1 {
		return 1
	}
	if v > 10 {
		return 10
	}
	return v
}

// ScoreAnswer computes probability and impact scores for a single answer.
// It is the Go equivalent of the pCalc/iCalc closures in risks.ts.
//
// For radio questions: looks up the answer in Opts and returns the
// corresponding PScores/IScores values. Falls back to (1, 1) for unrecognised
// answers (e.g. the user skipped an optional question).
//
// For text questions: scores based on whether the trimmed answer length
// exceeds the configured threshold.
//
// Returns an error only if rawConfig cannot be parsed; a missing/empty answer
// is NOT an error — it returns the minimum scores (1, 1).
func ScoreAnswer(rawConfig json.RawMessage, answer string) (p, i int, err error) {
	cfg, err := ParseScoringConfig(rawConfig)
	if err != nil {
		return 0, 0, fmt.Errorf("ScoreAnswer: %w", err)
	}

	answer = strings.TrimSpace(answer)

	switch {
	case cfg.IsRadio():
		rc := cfg.Radio()
		for idx, opt := range rc.Opts {
			if opt == answer {
				return clamp(rc.PScores[idx]), clamp(rc.IScores[idx]), nil
			}
		}
		// Answer not found in options (empty / skipped optional question).
		return 1, 1, nil

	case cfg.IsText():
		tc := cfg.Text()
		if len(answer) > tc.Threshold {
			return clamp(tc.PLong), clamp(tc.ILong), nil
		}
		return clamp(tc.PShort), clamp(tc.IShort), nil

	default:
		// ParseScoringConfig guarantees one of the two branches above, so this
		// is unreachable — but the compiler needs it.
		return 1, 1, nil
	}
}

// GetTier classifies a (probability, impact) pair into one of the four
// risk tiers. Mirrors risks.ts getRiskTier() exactly.
//
//	Watch  — high prob  AND high impact  (top-right, existential + imminent)
//	Red    — low prob   AND high impact  (top-left,  existential but unlikely)
//	Manage — high prob  AND low impact   (bottom-right, survivable)
//	Ignore — low prob   AND low impact   (bottom-left, not worth attention)
func GetTier(p, i int) RiskTier {
	highImpact := i >= highImpactThreshold
	highProb := p >= highProbThreshold

	switch {
	case highImpact && highProb:
		return TierWatch
	case highImpact && !highProb:
		return TierRed
	case !highImpact && highProb:
		return TierManage
	default:
		return TierIgnore
	}
}

// ComputeRisks scores all answers for a session and returns a sorted,
// ranked slice of ScoredRisk ready to be persisted.
//
// Rows where IsScoring=false (snapshot/context questions) are silently skipped,
// matching the risks.ts filter `q.sectionId !== "snapshot"`.
//
// The returned slice is sorted by Score descending (ties broken by QuestionID
// for determinism). Rank is 1-indexed and set on each element.
//
// Returns an error if any answer's scoring config cannot be parsed. In
// production the worker should treat this as a hard failure and set the report
// to error status.
func ComputeRisks(rows []AnswerRow) ([]ScoredRisk, error) {
	risks := make([]ScoredRisk, 0, len(rows))

	for _, row := range rows {
		if !row.IsScoring {
			continue
		}

		p, i, err := ScoreAnswer(row.ScoringConfig, row.AnswerText)
		if err != nil {
			return nil, fmt.Errorf("question %q: %w", row.QuestionID, err)
		}

		score := p * i

		risks = append(risks, ScoredRisk{
			QuestionID: row.QuestionID,
			RiskName:   row.RiskName,
			RiskDesc:   row.RiskDesc,
			Hedge:      row.Hedge,
			Section:    row.SectionTitle,
			P:          p,
			I:          i,
			Score:      score,
			Tier:       GetTier(p, i),
		})
	}

	// Sort descending by score; break ties by question ID for determinism.
	sort.Slice(risks, func(a, b int) bool {
		if risks[a].Score != risks[b].Score {
			return risks[a].Score > risks[b].Score
		}
		return risks[a].QuestionID < risks[b].QuestionID
	})

	// Assign 1-indexed rank.
	for idx := range risks {
		risks[idx].Rank = idx + 1
	}

	return risks, nil
}

// ─── AGGREGATE HELPERS ────────────────────────────────────────────────────────

// OverallScore computes the overall risk score (0–100) as a rounded mean of
// all individual scores. Returns 0 for an empty slice.
func OverallScore(risks []ScoredRisk) int {
	if len(risks) == 0 {
		return 0
	}
	total := 0
	for _, r := range risks {
		total += r.Score
	}
	return int(float64(total)/float64(len(risks)) + 0.5)
}

// CriticalCount returns the number of risks in the Watch tier — those that are
// both high-probability and high-impact. These are the ones flagged in the UI
// with "⚠ N Critical Risks Detected".
func CriticalCount(risks []ScoredRisk) int {
	n := 0
	for _, r := range risks {
		if r.Tier == TierWatch {
			n++
		}
	}
	return n
}

// FilterByTier returns only the risks matching any of the provided tiers,
// preserving existing order. Useful for AI hedge generation (watch + red only).
func FilterByTier(risks []ScoredRisk, tiers ...RiskTier) []ScoredRisk {
	set := make(map[RiskTier]struct{}, len(tiers))
	for _, t := range tiers {
		set[t] = struct{}{}
	}
	out := make([]ScoredRisk, 0, len(risks))
	for _, r := range risks {
		if _, ok := set[r.Tier]; ok {
			out = append(out, r)
		}
	}
	return out
}