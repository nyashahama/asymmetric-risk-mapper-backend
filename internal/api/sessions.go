package api

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
)

// ─── POST /api/session ────────────────────────────────────────────────────────

type createSessionRequest struct {
	// Context fields are optional at creation — the user fills them in Step 1.
	BizName  string `json:"biz_name"`
	Industry string `json:"industry"`
	Stage    string `json:"stage"`
}

type createSessionResponse struct {
	SessionID  string `json:"session_id"`
	AnonToken  string `json:"anon_token"`
}

// handleCreateSession creates an anonymous session for a new visitor.
// Called once when the assessment page first loads.
//
// The anon_token is returned to the browser and stored in sessionStorage.
// It is sent as X-Anon-Token on all subsequent session-scoped requests.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if !decode(w, r, &req) {
		return
	}

	// Generate a cryptographically random token. 32 bytes → 64 hex chars.
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		s.respondInternalErr(w, r, fmt.Errorf("generate anon token: %w", err))
		return
	}
	anonToken := hex.EncodeToString(tokenBytes)

	// Hash the real IP for fraud logging — never store the raw IP.
	ipHash := hashIP(realIP(r))

	session, err := s.q.CreateSession(r.Context(), db.CreateSessionParams{
		AnonToken:   anonToken,
		UtmSource:   nullString(r.URL.Query().Get("utm_source")),
		UtmMedium:   nullString(r.URL.Query().Get("utm_medium")),
		UtmCampaign: nullString(r.URL.Query().Get("utm_campaign")),
		Referrer:    nullString(r.Referer()),
		IpHash:      nullString(ipHash),
		UserAgent:   nullString(r.UserAgent()),
	})
	if err != nil {
		s.respondInternalErr(w, r, fmt.Errorf("create session: %w", err))
		return
	}

	// If context was provided at creation time, persist it immediately.
	if req.BizName != "" || req.Industry != "" || req.Stage != "" {
		_, err = s.q.UpdateSessionContext(r.Context(), db.UpdateSessionContextParams{
			ID:       session.ID,
			BizName:  nullString(req.BizName),
			Industry: nullString(req.Industry),
			Stage:    nullString(req.Stage),
		})
		if err != nil {
			// Non-fatal — context can be set via PATCH later.
			s.logger.Warn("create session: failed to set initial context",
				"session_id", session.ID,
				"error", err,
				logField(r),
			)
		}
	}

	respond(w, http.StatusCreated, createSessionResponse{
		SessionID: session.ID.String(),
		AnonToken: anonToken,
	})
}

// ─── PATCH /api/session/:sessionID/context ────────────────────────────────────

type updateContextRequest struct {
	BizName  string `json:"biz_name"`
	Industry string `json:"industry"`
	Stage    string `json:"stage"`
}

type updateContextResponse struct {
	SessionID string `json:"session_id"`
	BizName   string `json:"biz_name"`
	Industry  string `json:"industry"`
	Stage     string `json:"stage"`
}

// handleUpdateContext persists the business context from Step 1 (ContextStep).
// The route is protected by requireAnonToken middleware, so session_id in the
// URL is already verified to belong to the token sender.
func (s *Server) handleUpdateContext(w http.ResponseWriter, r *http.Request) {
	sessionID, err := parseUUID(chi.URLParam(r, "sessionID"))
	if err != nil {
		respondErr(w, http.StatusBadRequest, "invalid session_id")
		return
	}

	var req updateContextRequest
	if !decode(w, r, &req) {
		return
	}

	session, err := s.q.UpdateSessionContext(r.Context(), db.UpdateSessionContextParams{
		ID:       sessionID,
		BizName:  nullString(req.BizName),
		Industry: nullString(req.Industry),
		Stage:    nullString(req.Stage),
	})
	if err != nil {
		s.respondInternalErr(w, r, fmt.Errorf("update context: %w", err))
		return
	}

	respond(w, http.StatusOK, updateContextResponse{
		SessionID: session.ID.String(),
		BizName:   session.BizName.String,
		Industry:  session.Industry.String,
		Stage:     session.Stage.String,
	})
}

// ─── HELPERS ─────────────────────────────────────────────────────────────────

// nullString converts a Go string to sql.NullString. Empty string → NULL.
func nullString(s string) sql.NullString {
	s = strings.TrimSpace(s)
	return sql.NullString{String: s, Valid: s != ""}
}

// hashIP returns the hex-encoded SHA-256 of the IP string.
func hashIP(ip string) string {
	h := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(h[:])
}

// realIP extracts the client IP, honouring X-Real-IP set by a reverse proxy.
func realIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	// RemoteAddr is "ip:port".
	if idx := strings.LastIndex(r.RemoteAddr, ":"); idx >= 0 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}

// parseUUID wraps uuid.Parse with a cleaner error.
func parseUUID(s string) (uuidType, error) {
	return uuidParse(s)
}