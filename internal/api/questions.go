package api

import (
	"context"
	"encoding/json"
	"fmt"

	apiv1 "github.com/nyashahama/asymmetric-risk-mapper-backend/gen/api/v1"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─── GetQuestions ─────────────────────────────────────────────────────────────
//
// Protected by anonTokenInterceptor.
//
// Returns all question definitions ordered by section and display_order, with
// the session's existing answers merged in. One round-trip gives the frontend
// everything it needs to render the questionnaire and restore state.

// radioScoringConfig is used only for JSON unmarshalling inside this handler
// to extract option labels and preview scores for the client.
type radioScoringConfig struct {
	Opts    []string `json:"opts"`
	PScores []int    `json:"p_scores"`
	IScores []int    `json:"i_scores"`
}

func (s *Server) GetQuestions(
	ctx context.Context,
	req *apiv1.GetQuestionsRequest,
) (*apiv1.GetQuestionsResponse, error) {
	sessionID, err := parseUUID(req.SessionId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid session_id")
	}

	// Fetch all question definitions ordered by section + display_order.
	questions, err := s.q.GetAllQuestionDefinitions(ctx)
	if err != nil {
		return nil, s.internalErr(ctx, "GetQuestions", fmt.Errorf("get questions: %w", err))
	}

	// Fetch the session's existing answers in one query and index by question_id.
	answerRows, err := s.q.GetAnswersBySession(ctx, sessionID)
	if err != nil {
		return nil, s.internalErr(ctx, "GetQuestions", fmt.Errorf("get answers: %w", err))
	}

	savedAnswers := make(map[string]string, len(answerRows))
	for _, a := range answerRows {
		savedAnswers[a.QuestionID] = a.AnswerText
	}

	totalAnswered := int32(0)
	out := make([]*apiv1.Question, 0, len(questions))

	for _, q := range questions {
		saved := savedAnswers[q.ID]
		if saved != "" {
			totalAnswered++
		}

		qp := &apiv1.Question{
			Id:           q.ID,
			SectionId:    string(q.SectionID),
			SectionTitle: q.SectionTitle,
			DisplayOrder: int32(q.DisplayOrder),
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
		// parsing the raw JSONB.
		if q.Type == db.QuestionTypeRadio {
			var cfg radioScoringConfig
			if err := json.Unmarshal(q.ScoringConfig, &cfg); err == nil && len(cfg.Opts) > 0 {
				opts := make([]*apiv1.QuestionOption, len(cfg.Opts))
				for i, label := range cfg.Opts {
					p, iScore := int32(1), int32(1)
					if i < len(cfg.PScores) {
						p = int32(cfg.PScores[i])
					}
					if i < len(cfg.IScores) {
						iScore = int32(cfg.IScores[i])
					}
					opts[i] = &apiv1.QuestionOption{
						Label:  label,
						PScore: p,
						IScore: iScore,
					}
				}
				qp.Options = opts
			}
		}

		out = append(out, qp)
	}

	return &apiv1.GetQuestionsResponse{
		Questions:     out,
		TotalAnswered: totalAnswered,
	}, nil
}