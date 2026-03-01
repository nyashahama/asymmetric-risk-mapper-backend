package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/api"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/email"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/store"
	stripeinternal "github.com/nyashahama/asymmetric-risk-mapper-backend/internal/stripe"
)

// ─── STUBS ────────────────────────────────────────────────────────────────────

// stubQuerier satisfies db.Querier with in-memory state.
// Fields may be set per-test to control behaviour.
type stubQuerier struct {
	db.Querier                          // embedded to panic on unimplemented methods
	sessions       map[string]db.Session // keyed by anon_token
	sessionsByID   map[uuid.UUID]db.Session
	reports        map[string]db.GetReportByAccessTokenRow // keyed by access_token
	riskResults    map[uuid.UUID][]db.RiskResult
	createSessionErr error
	upsertAnswerErr  error
}

func newStubQuerier() *stubQuerier {
	return &stubQuerier{
		sessions:     make(map[string]db.Session),
		sessionsByID: make(map[uuid.UUID]db.Session),
		reports:      make(map[string]db.GetReportByAccessTokenRow),
		riskResults:  make(map[uuid.UUID][]db.RiskResult),
	}
}

func (q *stubQuerier) addSession(token string, s db.Session) {
	q.sessions[token] = s
	q.sessionsByID[s.ID] = s
}

func (q *stubQuerier) CreateSession(_ context.Context, p db.CreateSessionParams) (db.Session, error) {
	if q.createSessionErr != nil {
		return db.Session{}, q.createSessionErr
	}
	s := db.Session{
		ID:        uuid.New(),
		AnonToken: p.AnonToken,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	q.addSession(p.AnonToken, s)
	return s, nil
}

func (q *stubQuerier) GetSessionByAnonToken(_ context.Context, token string) (db.Session, error) {
	s, ok := q.sessions[token]
	if !ok {
		return db.Session{}, sql.ErrNoRows
	}
	return s, nil
}

func (q *stubQuerier) GetSessionByID(_ context.Context, id uuid.UUID) (db.Session, error) {
	s, ok := q.sessionsByID[id]
	if !ok {
		return db.Session{}, sql.ErrNoRows
	}
	return s, nil
}

func (q *stubQuerier) UpdateSessionContext(_ context.Context, p db.UpdateSessionContextParams) (db.Session, error) {
	s, ok := q.sessionsByID[p.ID]
	if !ok {
		return db.Session{}, sql.ErrNoRows
	}
	s.BizName = p.BizName
	s.Industry = p.Industry
	s.Stage = p.Stage
	q.sessionsByID[p.ID] = s
	for tok, sess := range q.sessions {
		if sess.ID == p.ID {
			q.sessions[tok] = s
		}
	}
	return s, nil
}

func (q *stubQuerier) UpsertAnswer(_ context.Context, p db.UpsertAnswerParams) (db.Answer, error) {
	if q.upsertAnswerErr != nil {
		return db.Answer{}, q.upsertAnswerErr
	}
	return db.Answer{
		ID:         uuid.New(),
		SessionID:  p.SessionID,
		QuestionID: p.QuestionID,
		AnswerText: p.AnswerText,
	}, nil
}

func (q *stubQuerier) GetReportByAccessToken(_ context.Context, token string) (db.GetReportByAccessTokenRow, error) {
	r, ok := q.reports[token]
	if !ok {
		return db.GetReportByAccessTokenRow{}, sql.ErrNoRows
	}
	return r, nil
}

func (q *stubQuerier) GetRiskResultsByReport(_ context.Context, id uuid.UUID) ([]db.RiskResult, error) {
	return q.riskResults[id], nil
}

func (q *stubQuerier) UpsertStripeEvent(_ context.Context, _ db.UpsertStripeEventParams) (db.StripeEvent, error) {
	return db.StripeEvent{}, nil
}

func (q *stubQuerier) MarkStripeEventProcessed(_ context.Context, _ string) (db.StripeEvent, error) {
	return db.StripeEvent{}, nil
}

func (q *stubQuerier) MarkStripeEventFailed(_ context.Context, _ db.MarkStripeEventFailedParams) (db.StripeEvent, error) {
	return db.StripeEvent{}, nil
}

func (q *stubQuerier) MarkSessionPaymentFailed(_ context.Context, _ sql.NullString) (db.Session, error) {
	return db.Session{}, nil
}

func (q *stubQuerier) AttachStripeCustomer(_ context.Context, p db.AttachStripeCustomerParams) (db.Session, error) {
	s, ok := q.sessionsByID[p.ID]
	if !ok {
		return db.Session{}, sql.ErrNoRows
	}
	s.StripePaymentIntent = p.StripePaymentIntent
	s.Email = p.Email
	q.sessionsByID[p.ID] = s
	return s, nil
}

// stubStore satisfies the subset of store.Store the API uses.
type stubStore struct {
	attachErr         error
	initialiseReport  db.Report
	initialiseErr     error
}

func (s *stubStore) AttachPaymentIntent(_ context.Context, _ store.AttachPaymentIntentParams) (db.Session, error) {
	return db.Session{}, s.attachErr
}

func (s *stubStore) InitialiseReport(_ context.Context, _ string) (db.Report, error) {
	return s.initialiseReport, s.initialiseErr
}

func (s *stubStore) MarkReportFailed(_ context.Context, _ uuid.UUID, _ string) (db.Report, error) {
	return db.Report{}, nil
}

// stubStripe is a controllable Stripe client.
type stubStripe struct {
	pi             stripeinternal.PaymentIntent
	clientSecret   string
	createErr      error
	getSecretErr   error
	verifyEvent    stripeinternal.Event
	verifyErr      error
}

func (s *stubStripe) CreatePaymentIntent(_ context.Context, _ stripeinternal.CreatePaymentIntentParams) (stripeinternal.PaymentIntent, error) {
	return s.pi, s.createErr
}

func (s *stubStripe) GetClientSecret(_ context.Context, _ string) (string, error) {
	return s.clientSecret, s.getSecretErr
}

func (s *stubStripe) VerifyWebhook(_ []byte, _ string, _ string) (stripeinternal.Event, error) {
	return s.verifyEvent, s.verifyErr
}

// stubWorker records enqueued jobs.
type stubWorker struct {
	enqueued []uuid.UUID
	err      error
}

func (w *stubWorker) Enqueue(_ context.Context, id uuid.UUID) error {
	w.enqueued = append(w.enqueued, id)
	return w.err
}

// stubMailer captures sent emails.
type stubMailer struct {
	receipts     []email.ReceiptParams
	reportReadys []email.ReportReadyParams
	err          error
}

func (m *stubMailer) SendReceipt(_ context.Context, p email.ReceiptParams) error {
	m.receipts = append(m.receipts, p)
	return m.err
}

func (m *stubMailer) SendReportReady(_ context.Context, p email.ReportReadyParams) error {
	m.reportReadys = append(m.reportReadys, p)
	return m.err
}

// ─── HELPERS ─────────────────────────────────────────────────────────────────

type testDeps struct {
	q       *stubQuerier
	stripe  *stubStripe
	worker  *stubWorker
	mailer  *stubMailer
	handler http.Handler
}

func newTestServer(t *testing.T, cfgOverrides ...func(*api.Config)) *testDeps {
	t.Helper()

	q := newStubQuerier()
	st := &stubStore{}
	fmt.Println(st)
	strp := &stubStripe{
		pi:           stripeinternal.PaymentIntent{ID: "pi_test", ClientSecret: "cs_test"},
		clientSecret: "cs_test",
	}
	wk := &stubWorker{}
	ml := &stubMailer{}

	cfg := api.Config{
		Env:                 "development",
		BaseURL:             "http://localhost:8080",
		StripeWebhookSecret: "whsec_test",
	}
	for _, fn := range cfgOverrides {
		fn(&cfg)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	handler := api.NewServer(q, nil, strp, wk, ml, cfg, logger)

	return &testDeps{
		q:       q,
		stripe:  strp,
		worker:  wk,
		mailer:  ml,
		handler: handler,
	}
}

func doRequest(t *testing.T, handler http.Handler, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.NewDecoder(rr.Body).Decode(dst); err != nil {
		t.Fatalf("decode response body: %v (raw: %s)", err, rr.Body.String())
	}
}

// sessionWithToken seeds a session in the stub querier and returns its ID and token.
func sessionWithToken(deps *testDeps) (uuid.UUID, string) {
	id := uuid.New()
	token := "test_tok_" + id.String()
	deps.q.addSession(token, db.Session{
		ID:        id,
		AnonToken: token,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	})
	return id, token
}

// ─── GET /healthz ─────────────────────────────────────────────────────────────

func TestHealthz(t *testing.T) {
	deps := newTestServer(t)
	rr := doRequest(t, deps.handler, http.MethodGet, "/healthz", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

// ─── POST /api/session ────────────────────────────────────────────────────────

func TestCreateSession_ReturnsSessionIDAndToken(t *testing.T) {
	deps := newTestServer(t)
	rr := doRequest(t, deps.handler, http.MethodPost, "/api/session",
		map[string]string{"biz_name": "Acme", "industry": "SaaS", "stage": "growth"}, nil)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		SessionID string `json:"session_id"`
		AnonToken string `json:"anon_token"`
	}
	decodeJSON(t, rr, &resp)

	if resp.SessionID == "" {
		t.Error("session_id should not be empty")
	}
	if resp.AnonToken == "" {
		t.Error("anon_token should not be empty")
	}
}

func TestCreateSession_OptionalContextFields(t *testing.T) {
	// Empty body is valid — all context fields are optional.
	deps := newTestServer(t)
	rr := doRequest(t, deps.handler, http.MethodPost, "/api/session", map[string]string{}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateSession_InvalidJSONReturns400(t *testing.T) {
	deps := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/session", bytes.NewBufferString(`{bad json`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	deps.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestCreateSession_UnknownFieldsReturns400(t *testing.T) {
	// DisallowUnknownFields is set on the decoder.
	deps := newTestServer(t)
	rr := doRequest(t, deps.handler, http.MethodPost, "/api/session",
		map[string]string{"unknown_field": "value"}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown field, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── PATCH /api/session/:sessionID/context ────────────────────────────────────

func TestUpdateContext_MissingTokenReturns401(t *testing.T) {
	deps := newTestServer(t)
	rr := doRequest(t, deps.handler,
		http.MethodPatch, "/api/session/"+uuid.New().String()+"/context",
		map[string]string{"biz_name": "Test"}, nil)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestUpdateContext_InvalidTokenReturns401(t *testing.T) {
	deps := newTestServer(t)
	rr := doRequest(t, deps.handler,
		http.MethodPatch, "/api/session/"+uuid.New().String()+"/context",
		map[string]string{"biz_name": "Test"},
		map[string]string{"X-Anon-Token": "totally_fake"})

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestUpdateContext_WrongSessionIDReturns403(t *testing.T) {
	deps := newTestServer(t)
	_, token := sessionWithToken(deps)

	rr := doRequest(t, deps.handler,
		http.MethodPatch, "/api/session/"+uuid.New().String()+"/context", // different UUID
		map[string]string{"biz_name": "Test"},
		map[string]string{"X-Anon-Token": token})

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateContext_ValidRequestUpdatesContext(t *testing.T) {
	deps := newTestServer(t)
	sessionID, token := sessionWithToken(deps)

	rr := doRequest(t, deps.handler,
		http.MethodPatch, "/api/session/"+sessionID.String()+"/context",
		map[string]string{"biz_name": "Acme Co", "industry": "SaaS", "stage": "growth"},
		map[string]string{"X-Anon-Token": token})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		BizName string `json:"biz_name"`
	}
	decodeJSON(t, rr, &resp)
	if resp.BizName != "Acme Co" {
		t.Errorf("biz_name: got %q", resp.BizName)
	}
}

// ─── PUT /api/session/:sessionID/answers ─────────────────────────────────────

func TestUpsertAnswers_EmptyBatchReturns400(t *testing.T) {
	deps := newTestServer(t)
	sessionID, token := sessionWithToken(deps)

	rr := doRequest(t, deps.handler,
		http.MethodPut, "/api/session/"+sessionID.String()+"/answers",
		map[string]any{"answers": []any{}},
		map[string]string{"X-Anon-Token": token})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUpsertAnswers_Over100ItemsReturns400(t *testing.T) {
	deps := newTestServer(t)
	sessionID, token := sessionWithToken(deps)

	answers := make([]map[string]string, 101)
	for i := range answers {
		answers[i] = map[string]string{"question_id": "q_x", "answer_text": "yes"}
	}

	rr := doRequest(t, deps.handler,
		http.MethodPut, "/api/session/"+sessionID.String()+"/answers",
		map[string]any{"answers": answers},
		map[string]string{"X-Anon-Token": token})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestUpsertAnswers_MissingQuestionIDReturns400(t *testing.T) {
	deps := newTestServer(t)
	sessionID, token := sessionWithToken(deps)

	rr := doRequest(t, deps.handler,
		http.MethodPut, "/api/session/"+sessionID.String()+"/answers",
		map[string]any{"answers": []map[string]string{{"question_id": "", "answer_text": "yes"}}},
		map[string]string{"X-Anon-Token": token})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestUpsertAnswers_ValidBatchReturnsUpsertedCount(t *testing.T) {
	deps := newTestServer(t)
	sessionID, token := sessionWithToken(deps)

	rr := doRequest(t, deps.handler,
		http.MethodPut, "/api/session/"+sessionID.String()+"/answers",
		map[string]any{
			"answers": []map[string]any{
				{"question_id": "q_cash_runway", "answer_text": "3–6 months", "client_p": 6, "client_i": 6},
				{"question_id": "q_key_person", "answer_text": "Yes", "client_p": 8, "client_i": 9},
			},
		},
		map[string]string{"X-Anon-Token": token})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Upserted int `json:"upserted"`
	}
	decodeJSON(t, rr, &resp)
	if resp.Upserted != 2 {
		t.Errorf("expected upserted=2, got %d", resp.Upserted)
	}
}

func TestUpsertAnswers_UpsertErrorReturns500(t *testing.T) {
	deps := newTestServer(t)
	sessionID, token := sessionWithToken(deps)
	deps.q.upsertAnswerErr = errors.New("db connection lost")

	rr := doRequest(t, deps.handler,
		http.MethodPut, "/api/session/"+sessionID.String()+"/answers",
		map[string]any{"answers": []map[string]string{{"question_id": "q_x", "answer_text": "yes"}}},
		map[string]string{"X-Anon-Token": token})

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

// ─── GET /api/report/:accessToken ────────────────────────────────────────────

func TestGetReport_UnknownTokenReturns404(t *testing.T) {
	deps := newTestServer(t)
	rr := doRequest(t, deps.handler, http.MethodGet, "/api/report/nonexistent", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestGetReport_DraftStatusReturns202(t *testing.T) {
	deps := newTestServer(t)
	token := "draft_token_abc"
	reportID := uuid.New()
	deps.q.reports[token] = db.GetReportByAccessTokenRow{
		ID:     reportID,
		Status: db.ReportStatusDraft,
	}

	rr := doRequest(t, deps.handler, http.MethodGet, "/api/report/"+token, nil, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	decodeJSON(t, rr, &resp)
	if resp["status"] != "draft" {
		t.Errorf("expected status=draft, got %q", resp["status"])
	}
}

func TestGetReport_ProcessingStatusReturns202(t *testing.T) {
	deps := newTestServer(t)
	token := "processing_token_abc"
	reportID := uuid.New()
	deps.q.reports[token] = db.GetReportByAccessTokenRow{
		ID:     reportID,
		Status: db.ReportStatusProcessing,
	}

	rr := doRequest(t, deps.handler, http.MethodGet, "/api/report/"+token, nil, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for processing, got %d", rr.Code)
	}
}

func TestGetReport_ReadyStatusReturns200WithBody(t *testing.T) {
	deps := newTestServer(t)
	token := "ready_token_abc"
	reportID := uuid.New()
	deps.q.reports[token] = db.GetReportByAccessTokenRow{
		ID:            reportID,
		Status:        db.ReportStatusReady,
		BizName:       sql.NullString{String: "Acme Co", Valid: true},
		OverallScore:  sql.NullInt16{Int16: 77, Valid: true},
		CriticalCount: sql.NullInt16{Int16: 2, Valid: true},
		ExecutiveSummary: sql.NullString{String: "High risk posture.", Valid: true},
	}
	deps.q.riskResults[reportID] = []db.RiskResult{
		{
			ID:          uuid.New(),
			Rank:        1,
			QuestionID:  "q_cash_runway",
			RiskName:    "Cash Runway Risk",
			Probability: 9,
			Impact:      9,
			Score:       81,
			Tier:        db.RiskTierWatch,
			Hedge:       "Maintain 6+ months runway",
			Section:     "snapshot",
		},
	}

	rr := doRequest(t, deps.handler, http.MethodGet, "/api/report/"+token, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Status        string `json:"status"`
		BizName       string `json:"biz_name"`
		OverallScore  int16  `json:"overall_score"`
		CriticalCount int16  `json:"critical_count"`
		Risks         []struct {
			QuestionID string `json:"question_id"`
			Score      int16  `json:"score"`
		} `json:"risks"`
	}
	decodeJSON(t, rr, &resp)

	if resp.Status != "ready" {
		t.Errorf("status: got %q", resp.Status)
	}
	if resp.OverallScore != 77 {
		t.Errorf("overall_score: got %d", resp.OverallScore)
	}
	if resp.CriticalCount != 2 {
		t.Errorf("critical_count: got %d", resp.CriticalCount)
	}
	if len(resp.Risks) != 1 {
		t.Fatalf("expected 1 risk, got %d", len(resp.Risks))
	}
	if resp.Risks[0].QuestionID != "q_cash_runway" {
		t.Errorf("risk question_id: got %q", resp.Risks[0].QuestionID)
	}
	if resp.Risks[0].Score != 81 {
		t.Errorf("risk score: got %d", resp.Risks[0].Score)
	}
}

func TestGetReport_ReadyUsesAIHedgeWhenAvailable(t *testing.T) {
	deps := newTestServer(t)
	token := "ready_ai_hedge_token"
	reportID := uuid.New()
	deps.q.reports[token] = db.GetReportByAccessTokenRow{
		ID:     reportID,
		Status: db.ReportStatusReady,
	}
	deps.q.riskResults[reportID] = []db.RiskResult{
		{
			Rank:       1,
			QuestionID: "q_cash_runway",
			RiskName:   "Cash Runway Risk",
			Hedge:      "Static hedge",
			AiHedge:    sql.NullString{String: "AI-generated hedge", Valid: true},
			Tier:       db.RiskTierWatch,
		},
	}

	rr := doRequest(t, deps.handler, http.MethodGet, "/api/report/"+token, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp struct {
		Risks []struct {
			Hedge string `json:"hedge"`
		} `json:"risks"`
	}
	decodeJSON(t, rr, &resp)

	if len(resp.Risks) == 0 {
		t.Fatal("expected at least one risk")
	}
	if resp.Risks[0].Hedge != "AI-generated hedge" {
		t.Errorf("expected AI hedge, got %q", resp.Risks[0].Hedge)
	}
}

// ─── CORS ─────────────────────────────────────────────────────────────────────

func TestCORS_PreflightReturns204(t *testing.T) {
	deps := newTestServer(t)
	req := httptest.NewRequest(http.MethodOptions, "/api/session", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rr := httptest.NewRecorder()
	deps.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("missing Access-Control-Allow-Origin header")
	}
	if rr.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("missing Access-Control-Allow-Methods header")
	}
}

func TestCORS_NoOriginHeader_SkipsCORSHeaders(t *testing.T) {
	deps := newTestServer(t)
	rr := doRequest(t, deps.handler, http.MethodGet, "/healthz", nil, nil)
	if rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("should not set CORS headers when no Origin present")
	}
}

// ─── POST /api/session/:sessionID/checkout ────────────────────────────────────

func TestCreateCheckout_MissingEmailReturns400(t *testing.T) {
	deps := newTestServer(t)
	sessionID, token := sessionWithToken(deps)

	rr := doRequest(t, deps.handler,
		http.MethodPost, "/api/session/"+sessionID.String()+"/checkout",
		map[string]string{"email": ""},
		map[string]string{"X-Anon-Token": token})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateCheckout_StripeErrorReturns500(t *testing.T) {
	deps := newTestServer(t)
	sessionID, token := sessionWithToken(deps)
	deps.stripe.createErr = errors.New("stripe unavailable")

	rr := doRequest(t, deps.handler,
		http.MethodPost, "/api/session/"+sessionID.String()+"/checkout",
		map[string]string{"email": "test@example.com"},
		map[string]string{"X-Anon-Token": token})

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── POST /api/webhooks/stripe ────────────────────────────────────────────────

func TestStripeWebhook_InvalidSignatureReturns400(t *testing.T) {
	deps := newTestServer(t)
	deps.stripe.verifyErr = errors.New("invalid signature")

	rr := doRequest(t, deps.handler,
		http.MethodPost, "/api/webhooks/stripe",
		map[string]string{"type": "payment_intent.succeeded"}, nil)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestStripeWebhook_UnknownEventTypeReturns200(t *testing.T) {
	deps := newTestServer(t)
	deps.stripe.verifyErr = nil
	deps.stripe.verifyEvent = stripeinternal.Event{
		ID:   "evt_test_unknown",
		Type: "customer.created", // not handled
	}

	rr := doRequest(t, deps.handler,
		http.MethodPost, "/api/webhooks/stripe",
		[]byte(`{}`), nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown event type, got %d: %s", rr.Code, rr.Body.String())
	}
}