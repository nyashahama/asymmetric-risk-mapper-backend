package scoring_test

import (
	"encoding/json"
	"testing"

	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/scoring"
)

// ─── ScoreAnswer — radio ──────────────────────────────────────────────────────

func TestScoreAnswer_Radio_KnownOptions(t *testing.T) {
	cfg := json.RawMessage(`{
		"type": "radio",
		"opts": ["Less than 3 months","3–6 months","6–12 months","12+ months"],
		"p_scores": [9,6,3,1],
		"i_scores": [9,6,3,1]
	}`)

	tests := []struct {
		answer string
		wantP  int
		wantI  int
	}{
		{"Less than 3 months", 9, 9},
		{"3–6 months", 6, 6},
		{"6–12 months", 3, 3},
		{"12+ months", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.answer, func(t *testing.T) {
			p, i, err := scoring.ScoreAnswer(cfg, tt.answer)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p != tt.wantP || i != tt.wantI {
				t.Errorf("got P=%d I=%d, want P=%d I=%d", p, i, tt.wantP, tt.wantI)
			}
		})
	}
}

func TestScoreAnswer_Radio_UnknownAnswerFallsBackToMin(t *testing.T) {
	cfg := json.RawMessage(`{
		"type":"radio","opts":["A","B"],"p_scores":[5,8],"i_scores":[4,9]
	}`)
	for _, answer := range []string{"", "unknown", "  "} {
		p, i, err := scoring.ScoreAnswer(cfg, answer)
		if err != nil {
			t.Fatalf("answer=%q: unexpected error: %v", answer, err)
		}
		if p != 1 || i != 1 {
			t.Errorf("answer=%q: got P=%d I=%d, want P=1 I=1", answer, p, i)
		}
	}
}

func TestScoreAnswer_Radio_LeadingTrailingSpaceTrimmed(t *testing.T) {
	cfg := json.RawMessage(`{
		"type":"radio","opts":["Yes"],"p_scores":[8],"i_scores":[9]
	}`)
	// Answer with surrounding whitespace should match "Yes".
	p, i, err := scoring.ScoreAnswer(cfg, "  Yes  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != 8 || i != 9 {
		t.Errorf("got P=%d I=%d, want P=8 I=9", p, i)
	}
}

// ─── ScoreAnswer — text ───────────────────────────────────────────────────────

func TestScoreAnswer_Text(t *testing.T) {
	cfg := json.RawMessage(`{
		"type":"text","threshold":10,
		"p_short":2,"p_long":6,
		"i_short":2,"i_long":8
	}`)
	tests := []struct {
		name   string
		answer string
		wantP  int
		wantI  int
	}{
		{"empty → short", "", 2, 2},
		{"exactly at threshold → short", "0123456789", 2, 2},  // len==10, threshold==10, NOT > 10
		{"one over threshold → long", "01234567890", 6, 8},
		{"long answer → long", "this is a much longer answer text", 6, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, i, err := scoring.ScoreAnswer(cfg, tt.answer)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p != tt.wantP || i != tt.wantI {
				t.Errorf("got P=%d I=%d, want P=%d I=%d", p, i, tt.wantP, tt.wantI)
			}
		})
	}
}

// ─── ScoreAnswer — invalid configs ───────────────────────────────────────────

func TestScoreAnswer_InvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  json.RawMessage
	}{
		{"empty", json.RawMessage(``)},
		{"malformed JSON", json.RawMessage(`{bad}`)},
		{"unknown type", json.RawMessage(`{"type":"checkbox"}`)},
		{"radio mismatched p_scores length", json.RawMessage(`{
			"type":"radio","opts":["A","B"],"p_scores":[1],"i_scores":[1,2]
		}`)},
		{"radio mismatched i_scores length", json.RawMessage(`{
			"type":"radio","opts":["A"],"p_scores":[1],"i_scores":[1,2]
		}`)},
		{"radio p_score out of range high", json.RawMessage(`{
			"type":"radio","opts":["A"],"p_scores":[11],"i_scores":[1]
		}`)},
		{"radio i_score out of range low", json.RawMessage(`{
			"type":"radio","opts":["A"],"p_scores":[1],"i_scores":[0]
		}`)},
		{"radio empty opts", json.RawMessage(`{
			"type":"radio","opts":[],"p_scores":[],"i_scores":[]
		}`)},
		{"text negative threshold", json.RawMessage(`{
			"type":"text","threshold":-1,"p_short":2,"p_long":6,"i_short":2,"i_long":8
		}`)},
		{"text p_short out of range", json.RawMessage(`{
			"type":"text","threshold":5,"p_short":0,"p_long":6,"i_short":2,"i_long":8
		}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := scoring.ScoreAnswer(tt.cfg, "anything")
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// ─── GetTier ──────────────────────────────────────────────────────────────────

func TestGetTier(t *testing.T) {
	// Thresholds: highProb = p >= 6, highImpact = i >= 7
	tests := []struct {
		p, i int
		want scoring.RiskTier
	}{
		// Watch: high prob AND high impact
		{6, 7, scoring.TierWatch},
		{10, 10, scoring.TierWatch},
		{6, 10, scoring.TierWatch},
		{9, 7, scoring.TierWatch},

		// Red: low prob AND high impact
		{5, 7, scoring.TierRed},
		{1, 10, scoring.TierRed},
		{5, 10, scoring.TierRed},

		// Manage: high prob AND low impact
		{6, 6, scoring.TierManage},
		{10, 1, scoring.TierManage},
		{6, 1, scoring.TierManage},

		// Ignore: low prob AND low impact
		{1, 1, scoring.TierIgnore},
		{5, 6, scoring.TierIgnore},
		{5, 1, scoring.TierIgnore},
	}
	for _, tt := range tests {
		got := scoring.GetTier(tt.p, tt.i)
		if got != tt.want {
			t.Errorf("GetTier(%d,%d) = %q, want %q", tt.p, tt.i, got, tt.want)
		}
	}
}

// ─── ComputeRisks ─────────────────────────────────────────────────────────────

func makeRadioCfg(opt string, p, i int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"type":     "radio",
		"opts":     []string{opt},
		"p_scores": []int{p},
		"i_scores": []int{i},
	})
	return b
}

func TestComputeRisks_SortsDescByScore(t *testing.T) {
	rows := []scoring.AnswerRow{
		{QuestionID: "q_low", AnswerText: "opt", IsScoring: true, ScoringConfig: makeRadioCfg("opt", 3, 3)},
		{QuestionID: "q_high", AnswerText: "opt", IsScoring: true, ScoringConfig: makeRadioCfg("opt", 9, 9)},
		{QuestionID: "q_mid", AnswerText: "opt", IsScoring: true, ScoringConfig: makeRadioCfg("opt", 5, 6)},
	}

	risks, err := scoring.ComputeRisks(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(risks) != 3 {
		t.Fatalf("expected 3 risks, got %d", len(risks))
	}

	// Scores: q_high=81, q_mid=30, q_low=9
	wantOrder := []string{"q_high", "q_mid", "q_low"}
	for idx, want := range wantOrder {
		if risks[idx].QuestionID != want {
			t.Errorf("position %d: got %s, want %s", idx, risks[idx].QuestionID, want)
		}
		if risks[idx].Rank != idx+1 {
			t.Errorf("position %d: rank=%d, want %d", idx, risks[idx].Rank, idx+1)
		}
	}
}

func TestComputeRisks_TieBreakAlphabeticalByQuestionID(t *testing.T) {
	cfg := makeRadioCfg("opt", 5, 5) // score 25 each
	rows := []scoring.AnswerRow{
		{QuestionID: "q_z", AnswerText: "opt", IsScoring: true, ScoringConfig: cfg},
		{QuestionID: "q_a", AnswerText: "opt", IsScoring: true, ScoringConfig: cfg},
		{QuestionID: "q_m", AnswerText: "opt", IsScoring: true, ScoringConfig: cfg},
	}

	risks, err := scoring.ComputeRisks(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if risks[0].QuestionID != "q_a" || risks[1].QuestionID != "q_m" || risks[2].QuestionID != "q_z" {
		t.Errorf("tie-break wrong: got %s %s %s", risks[0].QuestionID, risks[1].QuestionID, risks[2].QuestionID)
	}
}

func TestComputeRisks_SkipsNonScoringRows(t *testing.T) {
	rows := []scoring.AnswerRow{
		{QuestionID: "q_skip", AnswerText: "opt", IsScoring: false, ScoringConfig: makeRadioCfg("opt", 9, 9)},
		{QuestionID: "q_score", AnswerText: "opt", IsScoring: true, ScoringConfig: makeRadioCfg("opt", 5, 5)},
	}

	risks, err := scoring.ComputeRisks(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(risks) != 1 {
		t.Fatalf("expected 1 risk, got %d", len(risks))
	}
	if risks[0].QuestionID != "q_score" {
		t.Errorf("expected q_score, got %s", risks[0].QuestionID)
	}
}

func TestComputeRisks_EmptyInput(t *testing.T) {
	risks, err := scoring.ComputeRisks(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(risks) != 0 {
		t.Errorf("expected empty slice, got %d risks", len(risks))
	}
}

func TestComputeRisks_SetsCorrectScore(t *testing.T) {
	rows := []scoring.AnswerRow{
		{QuestionID: "q1", AnswerText: "opt", IsScoring: true, ScoringConfig: makeRadioCfg("opt", 9, 9)},
	}
	risks, err := scoring.ComputeRisks(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if risks[0].Score != 81 {
		t.Errorf("expected score 81 (9×9), got %d", risks[0].Score)
	}
	if risks[0].P != 9 || risks[0].I != 9 {
		t.Errorf("expected P=9 I=9, got P=%d I=%d", risks[0].P, risks[0].I)
	}
	if risks[0].Tier != scoring.TierWatch {
		t.Errorf("expected tier watch, got %s", risks[0].Tier)
	}
}

func TestComputeRisks_PopulatesRiskMetadata(t *testing.T) {
	rows := []scoring.AnswerRow{
		{
			QuestionID:    "q_cash_runway",
			AnswerText:    "opt",
			IsScoring:     true,
			RiskName:      "Cash Runway Risk",
			RiskDesc:      "Running out of cash",
			Hedge:         "Maintain 6+ months runway",
			SectionTitle:  "snapshot",
			ScoringConfig: makeRadioCfg("opt", 9, 9),
		},
	}
	risks, err := scoring.ComputeRisks(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := risks[0]
	if r.RiskName != "Cash Runway Risk" {
		t.Errorf("risk name: got %q", r.RiskName)
	}
	if r.RiskDesc != "Running out of cash" {
		t.Errorf("risk desc: got %q", r.RiskDesc)
	}
	if r.Hedge != "Maintain 6+ months runway" {
		t.Errorf("hedge: got %q", r.Hedge)
	}
	if r.Section != "snapshot" {
		t.Errorf("section: got %q", r.Section)
	}
}

func TestComputeRisks_BadConfigReturnsError(t *testing.T) {
	rows := []scoring.AnswerRow{
		{QuestionID: "q_bad", AnswerText: "opt", IsScoring: true, ScoringConfig: json.RawMessage(`{bad}`)},
	}
	_, err := scoring.ComputeRisks(rows)
	if err == nil {
		t.Error("expected error for bad scoring config")
	}
}

// ─── OverallScore ─────────────────────────────────────────────────────────────

func TestOverallScore(t *testing.T) {
	tests := []struct {
		name  string
		risks []scoring.ScoredRisk
		want  int
	}{
		{"nil", nil, 0},
		{"empty", []scoring.ScoredRisk{}, 0},
		{"single 50", []scoring.ScoredRisk{{Score: 50}}, 50},
		{"rounds up: 10+11=21/2=10.5→11", []scoring.ScoredRisk{{Score: 10}, {Score: 11}}, 11},
		{"exact: 20+20=40/2=20", []scoring.ScoredRisk{{Score: 20}, {Score: 20}}, 20},
		{"three values: 81+30+9=120/3=40", []scoring.ScoredRisk{{Score: 81}, {Score: 30}, {Score: 9}}, 40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoring.OverallScore(tt.risks)
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// ─── CriticalCount ───────────────────────────────────────────────────────────

func TestCriticalCount(t *testing.T) {
	risks := []scoring.ScoredRisk{
		{Tier: scoring.TierWatch},
		{Tier: scoring.TierWatch},
		{Tier: scoring.TierRed},
		{Tier: scoring.TierManage},
		{Tier: scoring.TierIgnore},
	}
	got := scoring.CriticalCount(risks)
	if got != 2 {
		t.Errorf("expected 2, got %d", got)
	}
}

func TestCriticalCount_Zero(t *testing.T) {
	risks := []scoring.ScoredRisk{
		{Tier: scoring.TierRed},
		{Tier: scoring.TierManage},
	}
	if got := scoring.CriticalCount(risks); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestCriticalCount_Empty(t *testing.T) {
	if got := scoring.CriticalCount(nil); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

// ─── FilterByTier ────────────────────────────────────────────────────────────

func TestFilterByTier_SingleTier(t *testing.T) {
	risks := []scoring.ScoredRisk{
		{QuestionID: "q1", Tier: scoring.TierWatch},
		{QuestionID: "q2", Tier: scoring.TierRed},
		{QuestionID: "q3", Tier: scoring.TierManage},
	}
	got := scoring.FilterByTier(risks, scoring.TierWatch)
	if len(got) != 1 || got[0].QuestionID != "q1" {
		t.Errorf("unexpected filter result: %+v", got)
	}
}

func TestFilterByTier_MultiTier(t *testing.T) {
	risks := []scoring.ScoredRisk{
		{QuestionID: "q1", Tier: scoring.TierWatch},
		{QuestionID: "q2", Tier: scoring.TierRed},
		{QuestionID: "q3", Tier: scoring.TierManage},
		{QuestionID: "q4", Tier: scoring.TierIgnore},
	}
	got := scoring.FilterByTier(risks, scoring.TierWatch, scoring.TierRed)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	if got[0].QuestionID != "q1" || got[1].QuestionID != "q2" {
		t.Errorf("wrong questions filtered: %v %v", got[0].QuestionID, got[1].QuestionID)
	}
}

func TestFilterByTier_NoMatch(t *testing.T) {
	risks := []scoring.ScoredRisk{
		{Tier: scoring.TierManage},
	}
	got := scoring.FilterByTier(risks, scoring.TierWatch)
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d", len(got))
	}
}

func TestFilterByTier_PreservesOrder(t *testing.T) {
	risks := []scoring.ScoredRisk{
		{QuestionID: "q3", Tier: scoring.TierWatch, Score: 30},
		{QuestionID: "q1", Tier: scoring.TierWatch, Score: 90},
		{QuestionID: "q2", Tier: scoring.TierWatch, Score: 60},
	}
	got := scoring.FilterByTier(risks, scoring.TierWatch)
	if got[0].QuestionID != "q3" || got[1].QuestionID != "q1" || got[2].QuestionID != "q2" {
		t.Error("FilterByTier should preserve original order")
	}
}

// ─── ParseScoringConfig ───────────────────────────────────────────────────────

func TestParseScoringConfig_RadioValid(t *testing.T) {
	cfg, err := scoring.ParseScoringConfig(json.RawMessage(`{
		"type":"radio","opts":["A","B"],"p_scores":[1,5],"i_scores":[2,8]
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.IsRadio() {
		t.Error("expected IsRadio() = true")
	}
	if cfg.IsText() {
		t.Error("expected IsText() = false")
	}
	rc := cfg.Radio()
	if len(rc.Opts) != 2 {
		t.Errorf("expected 2 opts, got %d", len(rc.Opts))
	}
}

func TestParseScoringConfig_TextValid(t *testing.T) {
	cfg, err := scoring.ParseScoringConfig(json.RawMessage(`{
		"type":"text","threshold":10,"p_short":2,"p_long":6,"i_short":2,"i_long":8
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.IsText() {
		t.Error("expected IsText() = true")
	}
	tc := cfg.Text()
	if tc.Threshold != 10 {
		t.Errorf("expected threshold 10, got %d", tc.Threshold)
	}
}