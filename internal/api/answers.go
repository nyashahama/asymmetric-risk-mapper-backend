package api

import (
	"database/sql"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
)

// ─── PUT /api/session/:sessionID/answers ─────────────────────────────────────
//
// Accepts a batch of answers and upserts them. The browser sends the full
// current answer set on every navigation (or a partial batch on debounce).
// Using upsert means it is safe to replay the same payload multiple times.

type answerInput struct {
	QuestionID string `json:"question_id"`
	AnswerText string `json:"answer_text"`
	// ClientP and ClientI are the client-side preview scores computed in
	// risks.ts. Stored alongside the answer for auditability; the server
	// recomputes its own scores from scoring_config during report generation.
	ClientP *int16 `json:"client_p,omitempty"`
	ClientI *int16 `json:"client_i,omitempty"`
}

type upsertAnswersRequest struct {
	Answers []answerInput `json:"answers"`
}

type upsertAnswersResponse struct {
	Upserted int `json:"upserted"`
}

// handleUpsertAnswers batch-upserts answers for a session.
// Each answer is upserted independently — there is no all-or-nothing guarantee
// across the batch at the HTTP level. If one upsert fails, the handler returns
// 500 and the browser can retry; successful upserts from the same batch are
// idempotent so retrying the full batch is safe.
func (s *Server) handleUpsertAnswers(w http.ResponseWriter, r *http.Request) {
	sessionID, err := parseUUID(chi.URLParam(r, "sessionID"))
	if err != nil {
		respondErr(w, http.StatusBadRequest, "invalid session_id")
		return
	}

	var req upsertAnswersRequest
	if !decode(w, r, &req) {
		return
	}

	if len(req.Answers) == 0 {
		respondErr(w, http.StatusBadRequest, "answers must not be empty")
		return
	}

	if len(req.Answers) > 100 {
		respondErr(w, http.StatusBadRequest, "too many answers in a single request (max 100)")
		return
	}

	upserted := 0
	for _, a := range req.Answers {
		if a.QuestionID == "" {
			respondErr(w, http.StatusBadRequest, "each answer must have a non-empty question_id")
			return
		}

		params := db.UpsertAnswerParams{
			SessionID:  sessionID,
			QuestionID: a.QuestionID,
			AnswerText: a.AnswerText,
		}

		if a.ClientP != nil {
			params.ClientP = sql.NullInt16{Int16: *a.ClientP, Valid: true}
		}
		if a.ClientI != nil {
			params.ClientI = sql.NullInt16{Int16: *a.ClientI, Valid: true}
		}

		if _, err := s.q.UpsertAnswer(r.Context(), params); err != nil {
			s.respondInternalErr(w, r, fmt.Errorf("upsert answer %q: %w", a.QuestionID, err))
			return
		}
		upserted++
	}

	respond(w, http.StatusOK, upsertAnswersResponse{Upserted: upserted})
}