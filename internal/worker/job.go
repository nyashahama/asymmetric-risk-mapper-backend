package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/ai"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/email"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/scoring"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/store"
)

// Job holds the dependencies for the score-and-generate pipeline. Each step
// is a separate method so they can be tested independently and so the Run
// method reads like a spec.
type Job struct {
	q      db.Querier
	store  *store.Store
	hedger ai.Hedger
	mailer email.Sender
	logger *slog.Logger
}

// NewJob constructs a Job with all required dependencies.
func NewJob(
	q db.Querier,
	st *store.Store,
	hedger ai.Hedger,
	mailer email.Sender,
	logger *slog.Logger,
) *Job {
	return &Job{
		q:      q,
		store:  st,
		hedger: hedger,
		mailer: mailer,
		logger: logger,
	}
}

// Run executes the full pipeline for a single report:
//
//  1. Load answers from the database.
//  2. Score every answer → []ScoredRisk.
//  3. Call the AI to generate hedge narratives for critical/red risks.
//  4. Persist everything atomically via store.PersistScoredReport.
//  5. Send the delivery email.
//
// Any error is returned to the Runner, which will retry up to MaxRetries times
// before calling store.MarkReportFailed.
func (j *Job) Run(ctx context.Context, reportID uuid.UUID) error {
	log := j.logger.With("report_id", reportID)
	log.Info("job: starting")

	// ── 1. Load the report to get the session ID ──────────────────────────────
	report, err := j.q.GetReportByID(ctx, reportID)
	if err != nil {
		return fmt.Errorf("job: get report: %w", err)
	}

	// ── 2. Load answers with their question metadata ───────────────────────────
	rows, err := j.q.GetAnswersBySession(ctx, report.SessionID)
	if err != nil {
		return fmt.Errorf("job: get answers: %w", err)
	}

	if len(rows) == 0 {
		return fmt.Errorf("job: no answers found for session %s", report.SessionID)
	}

	log.Debug("job: loaded answers", "count", len(rows))

	// ── 3. Map db rows → scoring.AnswerRow (keeps scoring/ dep-free) ──────────
	answerRows := make([]scoring.AnswerRow, len(rows))
	for i, r := range rows {
		answerRows[i] = scoring.AnswerRow{
			QuestionID:    r.QuestionID,
			AnswerText:    r.AnswerText,
			SectionTitle:  string(r.SectionID), // SectionID enum used as display label
			RiskName:      r.RiskName,
			RiskDesc:      r.RiskDesc,
			Hedge:         r.Hedge,
			ScoringConfig: r.ScoringConfig,
			IsScoring:     r.IsScoring,
		}
	}

	// ── 4. Score ──────────────────────────────────────────────────────────────
	risks, err := scoring.ComputeRisks(answerRows)
	if err != nil {
		return fmt.Errorf("job: compute risks: %w", err)
	}

	log.Debug("job: scored risks",
		"total", len(risks),
		"critical", scoring.CriticalCount(risks),
		"overall_score", scoring.OverallScore(risks),
	)

	// ── 5. Generate AI hedge narratives ───────────────────────────────────────
	// Only send watch + red risks to the AI — these are the ones with
	// substantive hedging actions. Manage and ignore risks use the static
	// hedge text from question_definitions.
	priorityRisks := scoring.FilterByTier(risks, scoring.TierWatch, scoring.TierRed)

	var hedgeResult ai.HedgeResult
	if len(priorityRisks) > 0 {
		hedgeResult, err = j.hedger.GenerateHedges(ctx, priorityRisks)
		if err != nil {
			// AI failure is non-fatal: we log it and continue with static hedges.
			// The report is still valuable without AI narratives.
			log.Warn("job: AI hedge generation failed, using static hedges", "error", err)
			hedgeResult = ai.HedgeResult{}
		}
	}

	// ── 6. Persist everything atomically ──────────────────────────────────────
	finalReport, err := j.store.PersistScoredReport(ctx, store.PersistScoredReportParams{
		ReportID:         reportID,
		Risks:            risks,
		AIHedges:         hedgeResult.Hedges,
		ExecutiveSummary: hedgeResult.ExecutiveSummary,
		TopPriorityHTML:  hedgeResult.TopPriorityHTML,
	})
	if err != nil {
		return fmt.Errorf("job: persist report: %w", err)
	}

	log.Info("job: report persisted",
		"overall_score", finalReport.OverallScore.Int16,
		"critical_count", finalReport.CriticalCount.Int16,
		"access_token", finalReport.AccessToken,
	)

	// ── 7. Send delivery email ────────────────────────────────────────────────
	// Load the session to get the recipient email address.
	session, err := j.q.GetSessionByID(ctx, report.SessionID)
	if err != nil {
		// Email failure should not fail the job — the report is ready and
		// accessible via the access token. Log and return nil.
		log.Error("job: could not load session for email delivery", "error", err)
		return nil
	}

	if !session.Email.Valid || session.Email.String == "" {
		log.Warn("job: session has no email address, skipping delivery email")
		return nil
	}

	if err := j.mailer.SendReportReady(ctx, email.ReportReadyParams{
		To:          session.Email.String,
		BizName:     session.BizName.String,
		AccessToken: finalReport.AccessToken,
	}); err != nil {
		// Log but do not fail — the user can still access their report via the
		// token. A failed email is surfaced in the email_log table.
		log.Error("job: failed to send report email",
			"to", session.Email.String,
			"error", err,
		)
	}

	return nil
}
