package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/scoring"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/store"
)

// ─── TEST INFRASTRUCTURE ──────────────────────────────────────────────────────

// openTestDB returns a *sql.DB from DATABASE_URL. Skips if the env var is
// not set so the test suite still passes in CI without a Postgres instance.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping store integration tests")
	}
	pool, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := pool.PingContext(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// withRollback runs fn inside a transaction that is always rolled back,
// leaving the database clean after each test.
func withRollback(t *testing.T, pool *sql.DB, fn func(ctx context.Context, q db.Querier, st *store.Store)) {
	t.Helper()
	ctx := context.Background()

	// The store uses serializable transactions internally, so we open a
	// plain read-committed wrapper here just to seed data that we roll back.
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin rollback tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	q := db.New(pool).WithTx(tx)
	// The store is given the outer pool so its internal transactions can see
	// the seeded rows (same connection via the shared pool in tests).
	st := store.New(pool, db.New(pool))

	fn(ctx, q, st)
}

// seedSession inserts a minimal anonymous session and returns it.
func seedSession(t *testing.T, ctx context.Context, q db.Querier, suffix string) db.Session {
	t.Helper()
	s, err := q.CreateSession(ctx, db.CreateSessionParams{
		AnonToken: fmt.Sprintf("test_token_%s_%s", t.Name(), suffix),
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return s
}

// attachPI attaches a fake Stripe PI to a session so InitialiseReport can
// call MarkSessionPaid, which looks up the session by stripe_payment_intent.
func attachPI(t *testing.T, ctx context.Context, q db.Querier, sessionID uuid.UUID , piID string) {
	t.Helper()
	_, err := q.AttachStripeCustomer(ctx, db.AttachStripeCustomerParams{
		ID:                  sessionID,
		StripePaymentIntent: sql.NullString{String: piID, Valid: true},
		Email:               sql.NullString{String: "test@example.com", Valid: true},
	})
	if err != nil {
		t.Fatalf("attachPI: %v", err)
	}
}

// ─── AttachPaymentIntent ──────────────────────────────────────────────────────

func TestAttachPaymentIntent_FirstCallSucceeds(t *testing.T) {
	pool := openTestDB(t)

	ctx := context.Background()
	q := db.New(pool)
	session, err := q.CreateSession(ctx, db.CreateSessionParams{AnonToken: "tok_attach_first_" + t.Name()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.ExecContext(ctx, "DELETE FROM sessions WHERE id=$1", session.ID) })

	st := store.New(pool, q)
	updated, err := st.AttachPaymentIntent(ctx, store.AttachPaymentIntentParams{
		SessionID:           session.ID,
		StripeCustomerID:    "cus_test_first",
		StripePaymentIntent: "pi_test_first_" + t.Name(),
		Email:               "test@example.com",
	})
	if err != nil {
		t.Fatalf("AttachPaymentIntent: %v", err)
	}
	if !updated.StripePaymentIntent.Valid {
		t.Error("expected StripePaymentIntent to be set")
	}
	if updated.Email.String != "test@example.com" {
		t.Errorf("email: got %q", updated.Email.String)
	}
}

func TestAttachPaymentIntent_SecondCallReturnsErrAlreadyAttached(t *testing.T) {
	pool := openTestDB(t)

	ctx := context.Background()
	q := db.New(pool)
	session, err := q.CreateSession(ctx, db.CreateSessionParams{AnonToken: "tok_attach_second_" + t.Name()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.ExecContext(ctx, "DELETE FROM sessions WHERE id=$1", session.ID) })

	st := store.New(pool, q)
	params := store.AttachPaymentIntentParams{
		SessionID:           session.ID,
		StripeCustomerID:    "cus_test",
		StripePaymentIntent: "pi_test_race_" + t.Name(),
		Email:               "test@example.com",
	}

	if _, err := st.AttachPaymentIntent(ctx, params); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call for same session must return the sentinel error.
	params.StripePaymentIntent = "pi_test_duplicate_" + t.Name()
	_, err = st.AttachPaymentIntent(ctx, params)
	if !errors.Is(err, store.ErrPaymentIntentAlreadyAttached) {
		t.Errorf("expected ErrPaymentIntentAlreadyAttached, got: %v", err)
	}
}

// ─── InitialiseReport ─────────────────────────────────────────────────────────

func TestInitialiseReport_CreatesDraftReport(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	q := db.New(pool)
	st := store.New(pool, q)

	piID := "pi_init_draft_" + t.Name()
	session, err := q.CreateSession(ctx, db.CreateSessionParams{AnonToken: "tok_draft_" + t.Name()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.ExecContext(ctx, "DELETE FROM reports WHERE session_id=$1", session.ID)
		_, _ = pool.ExecContext(ctx, "DELETE FROM sessions WHERE id=$1", session.ID)
	})

	_, err = q.AttachStripeCustomer(ctx, db.AttachStripeCustomerParams{
		ID:                  session.ID,
		StripePaymentIntent: sql.NullString{String: piID, Valid: true},
		Email:               sql.NullString{String: "x@example.com", Valid: true},
	})
	if err != nil {
		t.Fatalf("attach pi: %v", err)
	}

	report, err := st.InitialiseReport(ctx, piID)
	if err != nil {
		t.Fatalf("InitialiseReport: %v", err)
	}
	if report.ID.String() == "" {
		t.Error("expected non-empty report ID")
	}
	if report.Status != db.ReportStatusDraft {
		t.Errorf("expected status draft, got %s", report.Status)
	}
	if report.SessionID != session.ID {
		t.Error("session ID mismatch")
	}
	if report.AccessToken == "" {
		t.Error("expected non-empty access token")
	}
}

func TestInitialiseReport_DuplicateDeliveryReturnsErrAlreadyExists(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	q := db.New(pool)
	st := store.New(pool, q)

	piID := "pi_idem_" + t.Name()
	session, err := q.CreateSession(ctx, db.CreateSessionParams{AnonToken: "tok_idem_" + t.Name()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.ExecContext(ctx, "DELETE FROM reports WHERE session_id=$1", session.ID)
		_, _ = pool.ExecContext(ctx, "DELETE FROM sessions WHERE id=$1", session.ID)
	})

	_, err = q.AttachStripeCustomer(ctx, db.AttachStripeCustomerParams{
		ID:                  session.ID,
		StripePaymentIntent: sql.NullString{String: piID, Valid: true},
	})
	if err != nil {
		t.Fatalf("attach pi: %v", err)
	}

	first, err := st.InitialiseReport(ctx, piID)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	second, err := st.InitialiseReport(ctx, piID)
	if !errors.Is(err, store.ErrReportAlreadyExists) {
		t.Errorf("expected ErrReportAlreadyExists, got: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("returned report ID mismatch: got %s, want %s", second.ID, first.ID)
	}
}

func TestInitialiseReport_MarksSessionPaid(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	q := db.New(pool)
	st := store.New(pool, q)

	piID := "pi_paid_" + t.Name()
	session, err := q.CreateSession(ctx, db.CreateSessionParams{AnonToken: "tok_paid_" + t.Name()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.ExecContext(ctx, "DELETE FROM reports WHERE session_id=$1", session.ID)
		_, _ = pool.ExecContext(ctx, "DELETE FROM sessions WHERE id=$1", session.ID)
	})

	_, err = q.AttachStripeCustomer(ctx, db.AttachStripeCustomerParams{
		ID:                  session.ID,
		StripePaymentIntent: sql.NullString{String: piID, Valid: true},
	})
	if err != nil {
		t.Fatalf("attach pi: %v", err)
	}

	if _, err := st.InitialiseReport(ctx, piID); err != nil {
		t.Fatalf("InitialiseReport: %v", err)
	}

	updated, err := q.GetSessionByID(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetSessionByID: %v", err)
	}
	if updated.PaymentStatus != db.PaymentStatusPaid {
		t.Errorf("expected payment_status=paid, got %s", updated.PaymentStatus)
	}
	if !updated.PaidAt.Valid {
		t.Error("expected paid_at to be set")
	}
}

// ─── MarkReportFailed ─────────────────────────────────────────────────────────

func TestMarkReportFailed_SetsErrorStatus(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	q := db.New(pool)
	st := store.New(pool, q)

	piID := "pi_fail_" + t.Name()
	session, err := q.CreateSession(ctx, db.CreateSessionParams{AnonToken: "tok_fail_" + t.Name()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.ExecContext(ctx, "DELETE FROM reports WHERE session_id=$1", session.ID)
		_, _ = pool.ExecContext(ctx, "DELETE FROM sessions WHERE id=$1", session.ID)
	})

	_, err = q.AttachStripeCustomer(ctx, db.AttachStripeCustomerParams{
		ID:                  session.ID,
		StripePaymentIntent: sql.NullString{String: piID, Valid: true},
	})
	if err != nil {
		t.Fatalf("attach pi: %v", err)
	}

	report, err := st.InitialiseReport(ctx, piID)
	if err != nil {
		t.Fatalf("InitialiseReport: %v", err)
	}

	failed, err := st.MarkReportFailed(ctx, report.ID, "ai service unavailable")
	if err != nil {
		t.Fatalf("MarkReportFailed: %v", err)
	}
	if failed.Status != db.ReportStatusError {
		t.Errorf("expected status=error, got %s", failed.Status)
	}
	if !failed.ErrorMessage.Valid || failed.ErrorMessage.String != "ai service unavailable" {
		t.Errorf("error message: %+v", failed.ErrorMessage)
	}
}

// ─── PersistScoredReport ──────────────────────────────────────────────────────

func TestPersistScoredReport_FinalizesReport(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	q := db.New(pool)
	st := store.New(pool, q)

	piID := "pi_persist_" + t.Name()
	session, err := q.CreateSession(ctx, db.CreateSessionParams{AnonToken: "tok_persist_" + t.Name()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.ExecContext(ctx, "DELETE FROM risk_results WHERE report_id IN (SELECT id FROM reports WHERE session_id=$1)", session.ID)
		_, _ = pool.ExecContext(ctx, "DELETE FROM reports WHERE session_id=$1", session.ID)
		_, _ = pool.ExecContext(ctx, "DELETE FROM sessions WHERE id=$1", session.ID)
	})

	_, err = q.AttachStripeCustomer(ctx, db.AttachStripeCustomerParams{
		ID:                  session.ID,
		StripePaymentIntent: sql.NullString{String: piID, Valid: true},
	})
	if err != nil {
		t.Fatalf("attach pi: %v", err)
	}

	report, err := st.InitialiseReport(ctx, piID)
	if err != nil {
		t.Fatalf("InitialiseReport: %v", err)
	}

	// Seed question_definitions the risks reference.
	// (skip if q_cash_runway already exists from your seed data)
	risks := []scoring.ScoredRisk{
		{
			QuestionID: "q_cash_runway",
			Rank:       1,
			RiskName:   "Cash Runway Risk",
			RiskDesc:   "Running out of cash",
			Hedge:      "Maintain 6+ months runway",
			Section:    "snapshot",
			P:          9, I: 9, Score: 81,
			Tier: scoring.TierWatch,
		},
	}

	finalised, err := st.PersistScoredReport(ctx, store.PersistScoredReportParams{
		ReportID:         report.ID,
		Risks:            risks,
		AIHedges:         map[string]string{"q_cash_runway": "AI-generated hedge narrative"},
		ExecutiveSummary: "High risk posture.",
		TopPriorityHTML:  "<strong>Act now.</strong>",
	})
	if err != nil {
		t.Fatalf("PersistScoredReport: %v", err)
	}

	if finalised.Status != db.ReportStatusReady {
		t.Errorf("expected status=ready, got %s", finalised.Status)
	}
	if !finalised.OverallScore.Valid || finalised.OverallScore.Int16 != 81 {
		t.Errorf("overall score: %+v", finalised.OverallScore)
	}
	if !finalised.CriticalCount.Valid || finalised.CriticalCount.Int16 != 1 {
		t.Errorf("critical count: %+v", finalised.CriticalCount)
	}
	if !finalised.ExecutiveSummary.Valid || finalised.ExecutiveSummary.String != "High risk posture." {
		t.Errorf("executive summary: %+v", finalised.ExecutiveSummary)
	}
	if !finalised.GeneratedAt.Valid {
		t.Error("expected generated_at to be set")
	}
}