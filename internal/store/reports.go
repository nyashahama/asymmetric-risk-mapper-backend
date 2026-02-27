package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/scoring"
	"github.com/sqlc-dev/pqtype"
)

// ─── INPUT TYPES ─────────────────────────────────────────────────────────────

// PersistScoredReportParams is everything the worker hands to the store once
// scoring and AI hedge generation are complete.
type PersistScoredReportParams struct {
	ReportID         uuid.UUID
	Risks            []scoring.ScoredRisk // sorted, ranked — from scoring.ComputeRisks
	AIHedges         map[string]string    // question_id → AI-generated hedge text; may be nil
	ExecutiveSummary string               // AI-generated; empty string is fine
	TopPriorityHTML  string               // AI-generated; empty string is fine
}

// ─── ERRORS ──────────────────────────────────────────────────────────────────

// ErrReportAlreadyExists is returned by InitialiseReport when a report row for
// the session already exists. The webhook handler should treat this as
// idempotent success — a duplicate delivery of payment_intent.succeeded should
// not create a second report.
var ErrReportAlreadyExists = errors.New("store: report already exists for session")

// ─── METHODS ─────────────────────────────────────────────────────────────────

// InitialiseReport is called by the Stripe webhook handler on
// payment_intent.succeeded. It atomically:
//
//  1. Marks the session as paid.
//  2. Checks whether a report row already exists (idempotency guard).
//  3. Creates a new report row in draft status.
//
// If the session was already marked paid and a report already exists (duplicate
// webhook delivery), ErrReportAlreadyExists is returned. The caller should log
// this at debug level and return HTTP 200 to Stripe immediately — no further
// work is needed.
//
// If MarkSessionPaid succeeds but CreateReport fails, the whole transaction
// rolls back so the session remains unpaid. The next webhook delivery will
// retry cleanly.
func (s *Store) InitialiseReport(ctx context.Context, stripePaymentIntent string) (db.Report, error) {
	var report db.Report

	err := s.withTx(ctx, func(ctx context.Context, q db.Querier) error {
		// 1. Mark session paid. MarkSessionPaid matches on stripe_payment_intent,
		//    so it is safe to call for any PI string.
		session, err := q.MarkSessionPaid(ctx, sql.NullString{
			String: stripePaymentIntent,
			Valid:  true,
		})
		if err != nil {
			return fmt.Errorf("InitialiseReport: mark session paid: %w", err)
		}

		// 2. Idempotency guard — report may already exist from a prior delivery.
		existing, err := q.GetReportBySessionID(ctx, session.ID)
		if err == nil {
			// Row found — surface the sentinel and return the existing report so
			// the caller can enqueue it for processing if its status is not ready.
			report = existing
			return ErrReportAlreadyExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("InitialiseReport: check existing report: %w", err)
		}

		// 3. Create draft report.
		created, err := q.CreateReport(ctx, session.ID)
		if err != nil {
			return fmt.Errorf("InitialiseReport: create report: %w", err)
		}

		report = created
		return nil
	})

	if errors.Is(err, ErrReportAlreadyExists) {
		return report, ErrReportAlreadyExists
	}
	if err != nil {
		return db.Report{}, err
	}

	return report, nil
}

// PersistScoredReport is called by the background worker once scoring and AI
// hedge generation are complete. It atomically:
//
//  1. Sets the report status to processing (acquires the work slot).
//  2. Inserts one risk_result row per ScoredRisk.
//  3. Updates any risk_results rows that have an AI-generated hedge.
//  4. Finalises the report (status=ready, sets scores and JSON snapshot).
//
// If any step fails the entire transaction rolls back, leaving the report in
// its previous state. The worker's retry loop will pick it up again via
// ListPendingReports.
//
// The risks_json snapshot is computed here from p.Risks so that the serialised
// report is consistent with the individual risk_results rows written in the
// same transaction.
func (s *Store) PersistScoredReport(ctx context.Context, p PersistScoredReportParams) (db.Report, error) {
	var report db.Report

	err := s.withTx(ctx, func(ctx context.Context, q db.Querier) error {
		// 1. Claim the report for processing. This is a CAS-style update: if
		//    another worker process already set status=processing, this still
		//    succeeds (it is idempotent for the status field). The real guard
		//    against double-processing is the serializable transaction — only one
		//    writer can commit risk_results rows for a given report_id.
		if _, err := q.SetReportProcessing(ctx, p.ReportID); err != nil {
			return fmt.Errorf("PersistScoredReport: set processing: %w", err)
		}

		// 2. Insert risk_result rows. We capture the returned IDs so we can apply
		//    AI hedges in step 3 without a follow-up SELECT.
		resultIDs := make(map[string]uuid.UUID, len(p.Risks)) // question_id → risk_result.id

		for _, risk := range p.Risks {
			row, err := q.InsertRiskResult(ctx, db.InsertRiskResultParams{
				ReportID:    p.ReportID,
				QuestionID:  risk.QuestionID,
				Rank:        int16(risk.Rank),
				RiskName:    risk.RiskName,
				RiskDesc:    risk.RiskDesc,
				Probability: int16(risk.P),
				Impact:      int16(risk.I),
				Score:       int16(risk.Score),
				Tier:        db.RiskTier(risk.Tier), // scoring.RiskTier and db.RiskTier share string values
				Hedge:       risk.Hedge,
				Section:     risk.Section,
			})
			if err != nil {
				return fmt.Errorf("PersistScoredReport: insert risk %q: %w", risk.QuestionID, err)
			}
			resultIDs[risk.QuestionID] = row.ID
		}

		// 3. Apply AI hedges where available.
		for questionID, aiHedge := range p.AIHedges {
			rowID, ok := resultIDs[questionID]
			if !ok {
				// AI hedge references a question not in the scored set — skip
				// rather than error, so a partial AI response doesn't abort the
				// whole report.
				continue
			}
			if aiHedge == "" {
				continue
			}
			if _, err := q.SetAIHedge(ctx, db.SetAIHedgeParams{
				ID: rowID,
				AiHedge: sql.NullString{
					String: aiHedge,
					Valid:  true,
				},
			}); err != nil {
				return fmt.Errorf("PersistScoredReport: set AI hedge for %q: %w", questionID, err)
			}
		}

		// 4. Compute aggregate stats and serialise the risks snapshot.
		overallScore := scoring.OverallScore(p.Risks)
		criticalCount := scoring.CriticalCount(p.Risks)

		risksJSON, err := json.Marshal(p.Risks)
		if err != nil {
			return fmt.Errorf("PersistScoredReport: marshal risks JSON: %w", err)
		}

		finalised, err := q.FinalizeReport(ctx, db.FinalizeReportParams{
			ID:            p.ReportID,
			OverallScore:  sql.NullInt16{Int16: int16(overallScore), Valid: true},
			CriticalCount: sql.NullInt16{Int16: int16(criticalCount), Valid: true},
			RisksJson: pqtype.NullRawMessage{
				RawMessage: risksJSON,
				Valid:      true,
			},
			ExecutiveSummary: sql.NullString{
				String: p.ExecutiveSummary,
				Valid:  p.ExecutiveSummary != "",
			},
			TopPriorityHtml: sql.NullString{
				String: p.TopPriorityHTML,
				Valid:  p.TopPriorityHTML != "",
			},
		})
		if err != nil {
			return fmt.Errorf("PersistScoredReport: finalize report: %w", err)
		}

		report = finalised
		return nil
	})

	if err != nil {
		return db.Report{}, err
	}

	return report, nil
}

// MarkReportFailed sets the report status to error with a descriptive message.
// Called by the worker when scoring or AI generation fails permanently (i.e.
// after exhausting retries). This is a single-query write — no transaction
// needed — but it lives here because it is logically part of the report
// lifecycle and the worker should not call db.Querier directly for this.
func (s *Store) MarkReportFailed(ctx context.Context, reportID uuid.UUID, reason string) (db.Report, error) {
	report, err := s.q.SetReportError(ctx, db.SetReportErrorParams{
		ID: reportID,
		ErrorMessage: sql.NullString{
			String: reason,
			Valid:  true,
		},
	})
	if err != nil {
		return db.Report{}, fmt.Errorf("MarkReportFailed: %w", err)
	}
	return report, nil
}