package api

import (
	"context"
	"database/sql"
	"fmt"

	apiv1 "github.com/nyashahama/asymmetric-risk-mapper-backend/gen/api/v1"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─── UpsertAnswers ────────────────────────────────────────────────────────────
//
// Protected by anonTokenInterceptor.
//
// Batch-upserts answers for a session. The client sends the full current answer
// set on every navigation (or a partial batch on debounce). Using upsert means
// it is safe to replay the same payload multiple times.
//
// Each answer is upserted independently — there is no all-or-nothing guarantee
// across the batch at the RPC level. If one upsert fails the handler returns
// Internal and the client can retry; successful upserts from the same batch are
// idempotent so retrying the full batch is safe.

func (s *Server) UpsertAnswers(
	ctx context.Context,
	req *apiv1.UpsertAnswersRequest,
) (*apiv1.UpsertAnswersResponse, error) {
	sessionID, err := parseUUID(req.SessionId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid session_id")
	}

	if len(req.Answers) == 0 {
		return nil, status.Error(codes.InvalidArgument, "answers must not be empty")
	}

	if len(req.Answers) > 100 {
		return nil, status.Error(codes.InvalidArgument, "too many answers in a single request (max 100)")
	}

	upserted := int32(0)
	for _, a := range req.Answers {
		if a.QuestionId == "" {
			return nil, status.Error(codes.InvalidArgument, "each answer must have a non-empty question_id")
		}

		params := db.UpsertAnswerParams{
			SessionID:  sessionID,
			QuestionID: a.QuestionId,
			AnswerText: a.AnswerText,
		}

		// optional int32 fields in proto3 are *int32 in the generated Go code.
		if a.ClientP != nil {
			params.ClientP = sql.NullInt16{Int16: int16(*a.ClientP), Valid: true}
		}
		if a.ClientI != nil {
			params.ClientI = sql.NullInt16{Int16: int16(*a.ClientI), Valid: true}
		}

		if _, err := s.q.UpsertAnswer(ctx, params); err != nil {
			return nil, s.internalErr(ctx, "UpsertAnswers",
				fmt.Errorf("upsert answer %q: %w", a.QuestionId, err))
		}
		upserted++
	}

	return &apiv1.UpsertAnswersResponse{Upserted: upserted}, nil
}