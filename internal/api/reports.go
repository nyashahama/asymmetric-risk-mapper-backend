package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
)

// ─── GET /api/report/:accessToken ────────────────────────────────────────────

// reportRiskResponse is the per-risk shape returned in the API response.
// It flattens db.RiskResult into a clean JSON structure.
type reportRiskResponse struct {
	Rank        int16  `json:"rank"`
	QuestionID  string `json:"question_id"`
	RiskName    string `json:"risk_name"`
	RiskDesc    string `json:"risk_desc"`
	Probability int16  `json:"probability"`
	Impact      int16  `json:"impact"`
	Score       int16  `json:"score"`
	Tier        string `json:"tier"`
	Section     string `json:"section"`
	// Hedge is the AI-generated narrative if available, otherwise the static
	// hedge from question_definitions.
	Hedge string `json:"hedge"`
}

type reportResponse struct {
	ReportID         string               `json:"report_id"`
	Status           string               `json:"status"`
	BizName          string               `json:"biz_name,omitempty"`
	Industry         string               `json:"industry,omitempty"`
	Stage            string               `json:"stage,omitempty"`
	OverallScore     int16                `json:"overall_score"`
	CriticalCount    int16                `json:"critical_count"`
	ExecutiveSummary string               `json:"executive_summary,omitempty"`
	TopPriorityHTML  string               `json:"top_priority_html,omitempty"`
	Risks            []reportRiskResponse `json:"risks"`
	GeneratedAt      string               `json:"generated_at,omitempty"`
}

// handleGetReport serves the completed risk report. The access token is an
// opaque 24-byte base64url string stored on the report row — no session
// authentication is needed. The user receives this link in their email.
//
// Returns 404 for an unknown token. Returns 202 Accepted while the report is
// still being generated (status != ready) so the frontend can poll.
func (s *Server) handleGetReport(w http.ResponseWriter, r *http.Request) {
	accessToken := chi.URLParam(r, "accessToken")
	if accessToken == "" {
		respondErr(w, http.StatusBadRequest, "missing access token")
		return
	}

	// Load the report and its session context in one query.
	row, err := s.q.GetReportByAccessToken(r.Context(), accessToken)
	if errors.Is(err, sql.ErrNoRows) {
		respondErr(w, http.StatusNotFound, "report not found")
		return
	}
	if err != nil {
		s.respondInternalErr(w, r, fmt.Errorf("get report: %w", err))
		return
	}

	// Report is still being generated — tell the client to poll.
	if row.Status != db.ReportStatusReady {
		respond(w, http.StatusAccepted, map[string]string{
			"status":  string(row.Status),
			"message": "report is being generated, please check back shortly",
		})
		return
	}

	// Load individual risk rows for the full detail view.
	// We use risk_results rather than the risks_json snapshot so the response
	// always reflects AI hedges written after initial generation.
	results, err := s.q.GetRiskResultsByReport(r.Context(), row.ID)
	if err != nil {
		s.respondInternalErr(w, r, fmt.Errorf("get risk results: %w", err))
		return
	}

	risks := make([]reportRiskResponse, len(results))
	for i, rr := range results {
		hedge := rr.Hedge
		if rr.AiHedge.Valid && rr.AiHedge.String != "" {
			hedge = rr.AiHedge.String
		}
		risks[i] = reportRiskResponse{
			Rank:        rr.Rank,
			QuestionID:  rr.QuestionID,
			RiskName:    rr.RiskName,
			RiskDesc:    rr.RiskDesc,
			Probability: rr.Probability,
			Impact:      rr.Impact,
			Score:       rr.Score,
			Tier:        string(rr.Tier),
			Section:     rr.Section,
			Hedge:       hedge,
		}
	}

	generatedAt := ""
	if row.GeneratedAt.Valid {
		generatedAt = row.GeneratedAt.Time.UTC().Format("2006-01-02T15:04:05Z")
	}

	respond(w, http.StatusOK, reportResponse{
		ReportID:         row.ID.String(),
		Status:           string(row.Status),
		BizName:          row.BizName.String,
		Industry:         row.Industry.String,
		Stage:            row.Stage.String,
		OverallScore:     row.OverallScore.Int16,
		CriticalCount:    row.CriticalCount.Int16,
		ExecutiveSummary: row.ExecutiveSummary.String,
		TopPriorityHTML:  row.TopPriorityHtml.String,
		Risks:            risks,
		GeneratedAt:      generatedAt,
	})
}