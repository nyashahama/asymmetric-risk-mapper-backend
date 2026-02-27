-- =============================================================================
-- ASYMMETRIC RISK MAPPER — PostgreSQL Schema
-- Stack: Go + sqlc + Stripe
-- =============================================================================

-- ---------------------------------------------------------------------------
-- EXTENSIONS
-- ---------------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS "pgcrypto";   -- gen_random_uuid()
CREATE EXTENSION IF NOT EXISTS "citext";     -- case-insensitive email

-- ---------------------------------------------------------------------------
-- ENUMS
-- ---------------------------------------------------------------------------

CREATE TYPE question_type   AS ENUM ('radio', 'text', 'select');
CREATE TYPE risk_tier       AS ENUM ('watch', 'red', 'manage', 'ignore');
CREATE TYPE payment_status  AS ENUM ('pending', 'paid', 'failed', 'refunded');
CREATE TYPE report_status   AS ENUM ('draft', 'processing', 'ready', 'error');
CREATE TYPE section_id      AS ENUM (
    'snapshot', 'dependency', 'market', 'operational', 'legal', 'blindspots'
);

-- ---------------------------------------------------------------------------
-- 1. SESSIONS
--    Created the moment a visitor starts the questionnaire.
--    Anonymous until payment — then linked to an email.
-- ---------------------------------------------------------------------------

CREATE TABLE sessions (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- browser-side anonymous token stored in sessionStorage
    anon_token      TEXT        NOT NULL UNIQUE,

    -- filled in after payment
    email           CITEXT,

    -- business context (Step 1 — ContextStep)
    biz_name        TEXT,
    industry        TEXT,
    stage           TEXT,

    -- Stripe identifiers
    stripe_customer_id      TEXT    UNIQUE,
    stripe_payment_intent   TEXT    UNIQUE,

    payment_status  payment_status  NOT NULL DEFAULT 'pending',
    paid_at         TIMESTAMPTZ,

    -- UTM / attribution
    utm_source      TEXT,
    utm_medium      TEXT,
    utm_campaign    TEXT,
    referrer        TEXT,

    ip_hash         TEXT,   -- SHA-256 of IP, for fraud/abuse only
    user_agent      TEXT,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sessions_anon_token       ON sessions (anon_token);
CREATE INDEX idx_sessions_email            ON sessions (email);
CREATE INDEX idx_sessions_payment_status   ON sessions (payment_status);
CREATE INDEX idx_sessions_stripe_pi        ON sessions (stripe_payment_intent);

-- ---------------------------------------------------------------------------
-- 2. QUESTION DEFINITIONS  (source of truth, seeded from risks.ts)
--    Immutable after release — new versions get a new question_version.
-- ---------------------------------------------------------------------------

CREATE TABLE question_definitions (
    id              TEXT        PRIMARY KEY,     -- matches Question.id in risks.ts, e.g. "s2_supplier"
    question_version SMALLINT   NOT NULL DEFAULT 1,
    section_id      section_id  NOT NULL,
    section_title   TEXT        NOT NULL,
    display_order   SMALLINT    NOT NULL,

    text            TEXT        NOT NULL,
    subtext         TEXT,
    type            question_type NOT NULL,
    opts            TEXT[],                      -- radio options array
    placeholder     TEXT,
    required        BOOLEAN     NOT NULL DEFAULT TRUE,

    -- risk metadata (mirrors risks.ts Question fields)
    risk_name       TEXT        NOT NULL,
    risk_desc       TEXT        NOT NULL,
    hedge           TEXT        NOT NULL,

    -- scoring coefficients stored as JSONB so Go can evaluate pCalc/iCalc
    -- server-side without re-implementing TS.
    -- Format: {"type":"radio","opts":["<opt>",...],"p_scores":[1,3,5,7,9],"i_scores":[2,4,5,7,8]}
    -- For text questions: {"type":"text","p_short":2,"p_long":6,"i_short":2,"i_long":8,"threshold":10}
    scoring_config  JSONB       NOT NULL,

    is_scoring      BOOLEAN     NOT NULL DEFAULT TRUE,  -- false for snapshot/context questions

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_qdef_section_order ON question_definitions (section_id, display_order, question_version);

-- ---------------------------------------------------------------------------
-- 3. ANSWERS
--    One row per (session, question). Written as the user steps through.
--    Allows the user to go back and change answers before payment.
-- ---------------------------------------------------------------------------

CREATE TABLE answers (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id      UUID        NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    question_id     TEXT        NOT NULL REFERENCES question_definitions (id),

    -- raw answer as the user typed/selected it
    answer_text     TEXT        NOT NULL DEFAULT '',

    -- client-side preview scores (computed in browser from risks.ts)
    client_p        SMALLINT    CHECK (client_p BETWEEN 1 AND 10),
    client_i        SMALLINT    CHECK (client_i BETWEEN 1 AND 10),

    answered_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (session_id, question_id)
);

CREATE INDEX idx_answers_session ON answers (session_id);

-- ---------------------------------------------------------------------------
-- 4. REPORTS
--    Created after payment is confirmed. One report per session.
--    The Go backend scores the answers with its own implementation of
--    pCalc/iCalc, then calls the AI endpoint to augment each risk with
--    a narrative hedge recommendation.
-- ---------------------------------------------------------------------------

CREATE TABLE reports (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id      UUID        NOT NULL UNIQUE REFERENCES sessions (id),

    status          report_status NOT NULL DEFAULT 'draft',
    error_message   TEXT,

    -- Overall composite
    overall_score   SMALLINT    CHECK (overall_score BETWEEN 0 AND 100),
    critical_count  SMALLINT    DEFAULT 0,

    -- The full serialised risk list (JSON array of ComputedRisk).
    -- Stored for fast retrieval; individual rows also in risk_results.
    risks_json      JSONB,

    -- AI narrative fields
    executive_summary   TEXT,
    top_priority_html   TEXT,   -- formatted HTML for the report view

    -- Report access token (sent in email link — opaque, no session auth needed)
    access_token    TEXT        NOT NULL UNIQUE DEFAULT encode(gen_random_bytes(24), 'base64url'),

    generated_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_reports_access_token ON reports (access_token);
CREATE INDEX idx_reports_status       ON reports (status);

-- ---------------------------------------------------------------------------
-- 5. RISK RESULTS
--    One row per risk in the scored report. Enables querying/aggregation
--    across all reports (e.g. "what are the most common watch-list risks?").
-- ---------------------------------------------------------------------------

CREATE TABLE risk_results (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    report_id       UUID        NOT NULL REFERENCES reports (id) ON DELETE CASCADE,
    question_id     TEXT        NOT NULL REFERENCES question_definitions (id),

    rank            SMALLINT    NOT NULL,   -- 1-indexed, sorted by score DESC
    risk_name       TEXT        NOT NULL,
    risk_desc       TEXT        NOT NULL,

    probability     SMALLINT    NOT NULL CHECK (probability BETWEEN 1 AND 10),
    impact          SMALLINT    NOT NULL CHECK (impact BETWEEN 1 AND 10),
    score           SMALLINT    NOT NULL,   -- p × i, max 100
    tier            risk_tier   NOT NULL,

    -- AI-generated hedge narrative (may differ from static risks.ts hedge)
    hedge           TEXT        NOT NULL,
    ai_hedge        TEXT,       -- AI-augmented, set after generation

    section         TEXT        NOT NULL,

    UNIQUE (report_id, question_id)
);

CREATE INDEX idx_risk_results_report    ON risk_results (report_id, rank);
CREATE INDEX idx_risk_results_tier      ON risk_results (tier);
CREATE INDEX idx_risk_results_score     ON risk_results (score DESC);

-- ---------------------------------------------------------------------------
-- 6. STRIPE EVENTS  (webhook idempotency log)
--    Store every relevant Stripe event to handle retries safely.
-- ---------------------------------------------------------------------------

CREATE TABLE stripe_events (
    stripe_event_id TEXT        PRIMARY KEY,    -- e.g. "evt_..."
    type            TEXT        NOT NULL,       -- e.g. "payment_intent.succeeded"
    payload         JSONB       NOT NULL,
    processed       BOOLEAN     NOT NULL DEFAULT FALSE,
    processed_at    TIMESTAMPTZ,
    error           TEXT,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_stripe_events_type      ON stripe_events (type);
CREATE INDEX idx_stripe_events_processed ON stripe_events (processed);

-- ---------------------------------------------------------------------------
-- 7. EMAIL LOG
--    Record every outbound email (report delivery, receipt, etc.)
-- ---------------------------------------------------------------------------

CREATE TABLE email_log (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id      UUID        REFERENCES sessions (id),
    report_id       UUID        REFERENCES reports (id),

    to_address      CITEXT      NOT NULL,
    subject         TEXT        NOT NULL,
    template        TEXT        NOT NULL,   -- e.g. "report_ready", "receipt"

    provider_id     TEXT,       -- e.g. Resend / Postmark message ID
    sent_at         TIMESTAMPTZ,
    opened_at       TIMESTAMPTZ,
    error           TEXT,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_email_log_session ON email_log (session_id);
CREATE INDEX idx_email_log_report  ON email_log (report_id);

-- ---------------------------------------------------------------------------
-- 8. AGGREGATE STATS VIEW  (no personal data — safe for dashboards)
-- ---------------------------------------------------------------------------

CREATE VIEW public_risk_stats AS
SELECT
    rr.risk_name,
    rr.tier,
    rr.section,
    COUNT(*)                            AS occurrences,
    ROUND(AVG(rr.probability), 2)       AS avg_probability,
    ROUND(AVG(rr.impact), 2)            AS avg_impact,
    ROUND(AVG(rr.score), 2)             AS avg_score
FROM risk_results rr
JOIN reports r ON r.id = rr.report_id
WHERE r.status = 'ready'
GROUP BY rr.risk_name, rr.tier, rr.section
ORDER BY avg_score DESC;

-- ---------------------------------------------------------------------------
-- TRIGGERS — auto-update updated_at
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_sessions_updated_at
    BEFORE UPDATE ON sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER trg_answers_updated_at
    BEFORE UPDATE ON answers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER trg_reports_updated_at
    BEFORE UPDATE ON reports
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();