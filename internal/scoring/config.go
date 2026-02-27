// Package scoring implements the server-side risk scoring logic that mirrors
// the pCalc/iCalc functions from risks.ts. It is intentionally dependency-free:
// it imports nothing from internal/ and can be tested without a database.
package scoring

import (
	"encoding/json"
	"fmt"
)

// configType is the discriminator field inside every scoring_config JSONB blob.
type configType string

const (
	configTypeRadio configType = "radio"
	configTypeText  configType = "text"
)

// rawConfig is used only to peek at the "type" field before full unmarshalling.
type rawConfig struct {
	Type configType `json:"type"`
}

// RadioConfig holds scoring parameters for radio / select questions.
// Each element of Opts corresponds to the same-indexed element of PScores and
// IScores.
//
// DB JSON shape:
//
//	{
//	  "type":     "radio",
//	  "opts":     ["Option A", "Option B", "Option C"],
//	  "p_scores": [1, 5, 9],
//	  "i_scores": [2, 4, 8]
//	}
type RadioConfig struct {
	Type    configType `json:"type"`
	Opts    []string   `json:"opts"`
	PScores []int      `json:"p_scores"`
	IScores []int      `json:"i_scores"`
}

// Validate checks that the slices have consistent lengths and every score is
// in [1, 10]. Call this once at seed/startup time, not on every request.
func (c RadioConfig) Validate() error {
	n := len(c.Opts)
	if n == 0 {
		return fmt.Errorf("radio config: opts must not be empty")
	}
	if len(c.PScores) != n {
		return fmt.Errorf("radio config: p_scores length %d != opts length %d", len(c.PScores), n)
	}
	if len(c.IScores) != n {
		return fmt.Errorf("radio config: i_scores length %d != opts length %d", len(c.IScores), n)
	}
	for i, s := range c.PScores {
		if s < 1 || s > 10 {
			return fmt.Errorf("radio config: p_scores[%d]=%d out of range [1,10]", i, s)
		}
	}
	for i, s := range c.IScores {
		if s < 1 || s > 10 {
			return fmt.Errorf("radio config: i_scores[%d]=%d out of range [1,10]", i, s)
		}
	}
	return nil
}

// TextConfig holds scoring parameters for free-text questions.
// The answer is scored based purely on whether its trimmed length exceeds
// Threshold characters — matching the risks.ts pattern of
// `v.trim().length > N ? longScore : shortScore`.
//
// DB JSON shape:
//
//	{
//	  "type":      "text",
//	  "threshold": 10,
//	  "p_short":   2,
//	  "p_long":    6,
//	  "i_short":   2,
//	  "i_long":    8
//	}
type TextConfig struct {
	Type      configType `json:"type"`
	Threshold int        `json:"threshold"`
	PShort    int        `json:"p_short"`
	PLong     int        `json:"p_long"`
	IShort    int        `json:"i_short"`
	ILong     int        `json:"i_long"`
}

// Validate checks that all score fields are in [1, 10].
func (c TextConfig) Validate() error {
	for name, v := range map[string]int{
		"p_short": c.PShort,
		"p_long":  c.PLong,
		"i_short": c.IShort,
		"i_long":  c.ILong,
	} {
		if v < 1 || v > 10 {
			return fmt.Errorf("text config: %s=%d out of range [1,10]", name, v)
		}
	}
	if c.Threshold < 0 {
		return fmt.Errorf("text config: threshold must be >= 0, got %d", c.Threshold)
	}
	return nil
}

// ScoringConfig is a discriminated union — either a RadioConfig or a TextConfig.
// It is parsed from the scoring_config JSONB column on question_definitions.
//
// Callers receive a *ScoringConfig and call ScoreAnswer on it; they never need
// to inspect the inner type directly.
type ScoringConfig struct {
	radio *RadioConfig
	text  *TextConfig
}

// ParseScoringConfig unmarshals a raw JSON blob from the database into a typed
// ScoringConfig. Returns an error if the JSON is malformed, the type field is
// unrecognised, or the config fails its own Validate() check.
func ParseScoringConfig(raw json.RawMessage) (*ScoringConfig, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("scoring config: empty JSON")
	}

	var probe rawConfig
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("scoring config: cannot read type field: %w", err)
	}

	switch probe.Type {
	case configTypeRadio:
		var cfg RadioConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("scoring config: cannot unmarshal radio config: %w", err)
		}
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		return &ScoringConfig{radio: &cfg}, nil

	case configTypeText:
		var cfg TextConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("scoring config: cannot unmarshal text config: %w", err)
		}
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		return &ScoringConfig{text: &cfg}, nil

	default:
		return nil, fmt.Errorf("scoring config: unknown type %q", probe.Type)
	}
}

// IsRadio reports whether this config is for a radio/select question.
func (sc *ScoringConfig) IsRadio() bool { return sc.radio != nil }

// IsText reports whether this config is for a text question.
func (sc *ScoringConfig) IsText() bool { return sc.text != nil }

// Radio returns the underlying RadioConfig. Panics if IsRadio() is false;
// callers should only use this after checking IsRadio().
func (sc *ScoringConfig) Radio() RadioConfig { return *sc.radio }

// Text returns the underlying TextConfig. Panics if IsText() is false.
func (sc *ScoringConfig) Text() TextConfig { return *sc.text }