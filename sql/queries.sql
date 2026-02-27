-- =============================================================================
-- sqlc QUERIES — Asymmetric Risk Mapper
-- Run: sqlc generate  (sqlc.yaml points here)
-- =============================================================================

-- ---------------------------------------------------------------------------
-- SESSIONS
-- ---------------------------------------------------------------------------

-- name: CreateSession :one
INSERT INTO sessions (anon_token, utm_source, utm_medium, utm_campaign, referrer, ip_hash, user_agent)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetSessionByAnonToken :one
SELECT * FROM sessions WHERE anon_token = $1 LIMIT 1;

-- name: GetSessionByID :one
SELECT * FROM sessions WHERE id = $1 LIMIT 1;

-- name: GetSessionByStripePI :one
SELECT * FROM sessions WHERE stripe_payment_intent = $1 LIMIT 1;

-- name: UpdateSessionContext :one
UPDATE sessions
SET biz_name = $2,
    industry = $3,
    stage    = $4
WHERE id = $1
RETURNING *;

-- name: AttachStripeCustomer :one
UPDATE sessions
SET stripe_customer_id    = $2,
    stripe_payment_intent = $3,
    email                 = $4
WHERE id = $1
RETURNING *;

-- name: MarkSessionPaid :one
UPDATE sessions
SET payment_status = 'paid',
    paid_at        = now()
WHERE stripe_payment_intent = $1
RETURNING *;

-- name: MarkSessionPaymentFailed :one
UPDATE sessions
SET payment_status = 'failed'
WHERE stripe_payment_intent = $1
RETURNING *;

-- ---------------------------------------------------------------------------
-- ANSWERS
-- ---------------------------------------------------------------------------

-- name: UpsertAnswer :one
INSERT INTO answers (session_id, question_id, answer_text, client_p, client_i)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (session_id, question_id)
DO UPDATE SET
    answer_text = EXCLUDED.answer_text,
    client_p    = EXCLUDED.client_p,
    client_i    = EXCLUDED.client_i,
    updated_at  = now()
RETURNING *;

-- name: GetAnswersBySession :many
SELECT a.*, qd.section_id, qd.risk_name, qd.risk_desc, qd.hedge, qd.scoring_config, qd.is_scoring
FROM answers a
JOIN question_definitions qd ON qd.id = a.question_id
WHERE a.session_id = $1
ORDER BY qd.display_order;

-- name: CountAnsweredBySession :one
SELECT COUNT(*) FROM answers WHERE session_id = $1 AND answer_text != '';

-- ---------------------------------------------------------------------------
-- QUESTION DEFINITIONS
-- ---------------------------------------------------------------------------

-- name: GetAllQuestionDefinitions :many
SELECT * FROM question_definitions
ORDER BY section_id, display_order;

-- name: GetScoringQuestions :many
SELECT * FROM question_definitions
WHERE is_scoring = TRUE
ORDER BY section_id, display_order;

-- name: GetQuestionByID :one
SELECT * FROM question_definitions WHERE id = $1 LIMIT 1;

-- ---------------------------------------------------------------------------
-- REPORTS
-- ---------------------------------------------------------------------------

-- name: CreateReport :one
INSERT INTO reports (session_id)
VALUES ($1)
RETURNING *;

-- name: GetReportBySessionID :one
SELECT * FROM reports WHERE session_id = $1 LIMIT 1;

-- name: GetReportByAccessToken :one
SELECT r.*, s.biz_name, s.industry, s.stage, s.email
FROM reports r
JOIN sessions s ON s.id = r.session_id
WHERE r.access_token = $1
LIMIT 1;

-- name: GetReportByID :one
SELECT * FROM reports WHERE id = $1 LIMIT 1;

-- name: SetReportProcessing :one
UPDATE reports
SET status = 'processing'
WHERE id = $1
RETURNING *;

-- name: FinalizeReport :one
UPDATE reports
SET status          = 'ready',
    overall_score   = $2,
    critical_count  = $3,
    risks_json      = $4,
    executive_summary = $5,
    top_priority_html = $6,
    generated_at    = now()
WHERE id = $1
RETURNING *;

-- name: SetReportError :one
UPDATE reports
SET status        = 'error',
    error_message = $2
WHERE id = $1
RETURNING *;

-- name: ListPendingReports :many
-- Used by the background worker to pick up unprocessed reports.
SELECT * FROM reports
WHERE status IN ('draft', 'processing')
  AND created_at > now() - INTERVAL '1 day'
ORDER BY created_at;

-- ---------------------------------------------------------------------------
-- RISK RESULTS
-- ---------------------------------------------------------------------------

-- name: InsertRiskResult :one
INSERT INTO risk_results (
    report_id, question_id, rank, risk_name, risk_desc,
    probability, impact, score, tier, hedge, section
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: SetAIHedge :one
UPDATE risk_results
SET ai_hedge = $2
WHERE id = $1
RETURNING *;

-- name: GetRiskResultsByReport :many
SELECT * FROM risk_results
WHERE report_id = $1
ORDER BY rank;

-- name: GetWatchAndRedRisks :many
SELECT * FROM risk_results
WHERE report_id = $1 AND tier IN ('watch', 'red')
ORDER BY score DESC;

-- ---------------------------------------------------------------------------
-- STRIPE EVENTS
-- ---------------------------------------------------------------------------

-- name: UpsertStripeEvent :one
INSERT INTO stripe_events (stripe_event_id, type, payload)
VALUES ($1, $2, $3)
ON CONFLICT (stripe_event_id) DO NOTHING
RETURNING *;

-- name: MarkStripeEventProcessed :one
UPDATE stripe_events
SET processed    = TRUE,
    processed_at = now()
WHERE stripe_event_id = $1
RETURNING *;

-- name: MarkStripeEventFailed :one
UPDATE stripe_events
SET processed    = FALSE,
    error        = $2
WHERE stripe_event_id = $1
RETURNING *;

-- name: GetUnprocessedStripeEvents :many
SELECT * FROM stripe_events
WHERE processed = FALSE
  AND received_at > now() - INTERVAL '24 hours'
ORDER BY received_at;

-- ---------------------------------------------------------------------------
-- EMAIL LOG
-- ---------------------------------------------------------------------------

-- name: LogEmail :one
INSERT INTO email_log (session_id, report_id, to_address, subject, template, provider_id, sent_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
RETURNING *;

-- name: MarkEmailOpened :one
UPDATE email_log SET opened_at = now() WHERE provider_id = $1 RETURNING *;

-- ---------------------------------------------------------------------------
-- ANALYTICS
-- ---------------------------------------------------------------------------

-- name: GetRiskStats :many
SELECT * FROM public_risk_stats;

-- name: GetDailyRevenue :many
SELECT
    DATE(paid_at)       AS day,
    COUNT(*)            AS sales,
    COUNT(*) * 59       AS revenue_usd   -- $59 fixed price
FROM sessions
WHERE payment_status = 'paid'
  AND paid_at >= now() - INTERVAL '30 days'
GROUP BY DATE(paid_at)
ORDER BY day DESC;

-- name: GetCompletionFunnelStats :one
SELECT
    COUNT(*)                                                        AS total_sessions,
    COUNT(*) FILTER (WHERE (SELECT COUNT(*) FROM answers a WHERE a.session_id = s.id) > 0) AS started,
    COUNT(*) FILTER (WHERE payment_status = 'paid')                AS paid,
    COUNT(*) FILTER (WHERE payment_status = 'paid' AND EXISTS (
        SELECT 1 FROM reports r WHERE r.session_id = s.id AND r.status = 'ready'
    ))                                                              AS report_delivered
FROM sessions s;