package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	apiv1 "github.com/nyashahama/asymmetric-risk-mapper-backend/gen/api/v1"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─── GetReport ────────────────────────────────────────────────────────────────
//
// Public RPC — no auth required. The access token in the request message is an
// opaque 24-byte base64url string stored on the report row. The user receives
// this token in their delivery email.
//
// Returns NotFound for an unknown token.
// Returns a response with status="processing" (HTTP 202 equivalent) while the
// report is still being generated — the client should poll with back-off.

func (s *Server) GetReport(
	ctx context.Context,
	req *apiv1.GetReportRequest,
) (*apiv1.GetReportResponse, error) {
	if req.AccessToken == "" {
		return nil, status.Error(codes.InvalidArgument, "access_token is required")
	}

	// Load the report and its session context in one query.
	row, err := s.q.GetReportByAccessToken(ctx, req.AccessToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "report not found")
	}
	if err != nil {
		return nil, s.internalErr(ctx, "GetReport", fmt.Errorf("get report: %w", err))
	}

	// Report is still being generated — tell the client to poll.
	// Return a valid response (not an error) so the client can inspect status.
	if row.Status != db.ReportStatusReady {
		return &apiv1.GetReportResponse{
			ReportId: row.ID.String(),
			Status:   string(row.Status),
		}, nil
	}

	// Load individual risk rows for the full detail view.
	// We use risk_results rather than the risks_json snapshot so the response
	// always reflects AI hedges written after initial generation.
	results, err := s.q.GetRiskResultsByReport(ctx, row.ID)
	if err != nil {
		return nil, s.internalErr(ctx, "GetReport", fmt.Errorf("get risk results: %w", err))
	}

	risks := make([]*apiv1.RiskResult, len(results))
	for i, rr := range results {
		hedge := rr.Hedge
		if rr.AiHedge.Valid && rr.AiHedge.String != "" {
			hedge = rr.AiHedge.String
		}
		risks[i] = &apiv1.RiskResult{
			Rank:        int32(rr.Rank),
			QuestionId:  rr.QuestionID,
			RiskName:    rr.RiskName,
			RiskDesc:    rr.RiskDesc,
			Probability: int32(rr.Probability),
			Impact:      int32(rr.Impact),
			Score:       int32(rr.Score),
			Tier:        string(rr.Tier),
			Section:     rr.Section,
			Hedge:       hedge,
		}
	}

	generatedAt := ""
	if row.GeneratedAt.Valid {
		generatedAt = row.GeneratedAt.Time.UTC().Format("2006-01-02T15:04:05Z")
	}

	return &apiv1.GetReportResponse{
		ReportId:         row.ID.String(),
		Status:           string(row.Status),
		BizName:          row.BizName.String,
		Industry:         row.Industry.String,
		Stage:            row.Stage.String,
		OverallScore:     int32(row.OverallScore.Int16),
		CriticalCount:    int32(row.CriticalCount.Int16),
		ExecutiveSummary: row.ExecutiveSummary.String,
		TopPriorityHtml:  row.TopPriorityHtml.String,
		Risks:            risks,
		GeneratedAt:      generatedAt,
	}, nil
}