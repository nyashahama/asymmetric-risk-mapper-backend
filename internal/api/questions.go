package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
)

// ─── GET /api/session/:sessionID/questions ────────────────────────────────────
//
// Returns all question definitions, ordered by section and display_order,
// with the session's existing answers merged in. This single endpoint gives
// the frontend everything it needs to render the questionnaire and restore
// state on page load or back-navigation.
//
// Requires X-Anon-Token — the requireAnonToken middleware runs first.

// questionOption is a single radio/select option with its pre-computed scores.
// Sent to the client so the browser can render the preview heat map without
// knowing the scoring config format.
type questionOption struct {
	Label  string `json:"label"`
	PScore int    `json:"p_score"`
	IScore int    `json:"i_score"`
}

// questionResponse is the per-question shape returned to the browser.
type questionResponse struct {
	ID           string           `json:"id"`
	SectionID    string           `json:"section_id"`
	SectionTitle string           `json:"section_title"`
	DisplayOrder int16            `json:"display_order"`
	Text         string           `json:"text"`
	Subtext      string           `json:"subtext,omitempty"`
	Type         string           `json:"type"`
	Options      []questionOption `json:"options,omitempty"` // nil for text questions
	Placeholder  string           `json:"placeholder,omitempty"`
	Required     bool             `json:"required"`
	IsScoring    bool             `json:"is_scoring"`
	// Risk metadata — used client-side for the heat map preview labels.
	RiskName string `json:"risk_name"`
	RiskDesc string `json:"risk_desc"`
	// SavedAnswer is the session's current answer for this question, or ""
	// if the user hasn't answered it yet. Included so the client can restore
	// state in a single round-trip without a separate GET /answers call.
	SavedAnswer string `json:"saved_answer"`
}

type getQuestionsResponse struct {
	Questions []questionResponse `json:"questions"`
	// TotalAnswered is the count of non-empty answers — used by the frontend
	// to render the progress bar without counting locally.
	TotalAnswered int `json:"total_answered"`
}

// radioScoringConfig is used only for JSON unmarshalling inside this handler.
// The full scoring package is the source of truth for server-side scoring;
// this is only for extracting option labels and preview scores for the client.
type radioScoringConfig struct {
	Opts    []string `json:"opts"`
	PScores []int    `json:"p_scores"`
	IScores []int    `json:"i_scores"`
}

func (s *Server) handleGetQuestions(w http.ResponseWriter, r *http.Request) {
	sessionID, err := parseUUID(chi.URLParam(r, "sessionID"))
	if err != nil {
		respondErr(w, http.StatusBadRequest, "invalid session_id")
		return
	}

	// Fetch all question definitions ordered by section + display_order.
	questions, err := s.q.GetAllQuestionDefinitions(r.Context())
	if err != nil {
		s.respondInternalErr(w, r, fmt.Errorf("get questions: %w", err))
		return
	}

	// Fetch the session's existing answers in one query and index by question_id.
	// GetAnswersBySession joins with question_definitions so we can use it here
	// as a convenient source of truth; we only care about the answer fields.
	answerRows, err := s.q.GetAnswersBySession(r.Context(), sessionID)
	if err != nil {
		s.respondInternalErr(w, r, fmt.Errorf("get answers: %w", err))
		return
	}

	savedAnswers := make(map[string]string, len(answerRows))
	for _, a := range answerRows {
		savedAnswers[a.QuestionID] = a.AnswerText
	}

	totalAnswered := 0
	out := make([]questionResponse, 0, len(questions))

	for _, q := range questions {
		saved := savedAnswers[q.ID]
		if saved != "" {
			totalAnswered++
		}

		qr := questionResponse{
			ID:           q.ID,
			SectionID:    string(q.SectionID),
			SectionTitle: q.SectionTitle,
			DisplayOrder: q.DisplayOrder,
			Text:         q.Text,
			Subtext:      q.Subtext.String,
			Type:         string(q.Type),
			Placeholder:  q.Placeholder.String,
			Required:     q.Required,
			IsScoring:    q.IsScoring,
			RiskName:     q.RiskName,
			RiskDesc:     q.RiskDesc,
			SavedAnswer:  saved,
		}

		// For radio questions, unpack the scoring config into labelled options
		// so the client can render choices and compute preview scores without
		// parsing the raw JSONB. Text questions have no options.
		if q.Type == db.QuestionTypeRadio {
			var cfg radioScoringConfig
			if err := json.Unmarshal(q.ScoringConfig, &cfg); err == nil && len(cfg.Opts) > 0 {
				opts := make([]questionOption, len(cfg.Opts))
				for i, label := range cfg.Opts {
					p, i2 := 1, 1
					if i < len(cfg.PScores) {
						p = cfg.PScores[i]
					}
					if i < len(cfg.IScores) {
						i2 = cfg.IScores[i]
					}
					opts[i] = questionOption{Label: label, PScore: p, IScore: i2}
				}
				qr.Options = opts
			}
		}

		out = append(out, qr)
	}

	respond(w, http.StatusOK, getQuestionsResponse{
		Questions:     out,
		TotalAnswered: totalAnswered,
	})
}
