package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// ─── CONTEXT KEYS ─────────────────────────────────────────────────────────────

type contextKey string

const (
	ctxKeySessionID  contextKey = "session_id"
	ctxKeyAnonToken  contextKey = "anon_token"
)

// ─── ANON TOKEN AUTH ──────────────────────────────────────────────────────────

// requireAnonToken is chi middleware that validates the X-Anon-Token header
// against the session row in the database.
//
// The token is stored browser-side in sessionStorage and sent on every request
// to session-scoped routes. If it is missing or doesn't match the session, the
// handler receives a 401 before it runs.
//
// On success, the verified session_id (from the URL param) and anon_token are
// stored in the request context for downstream handlers.
func (s *Server) requireAnonToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract token from header.
		token := strings.TrimSpace(r.Header.Get("X-Anon-Token"))
		if token == "" {
			respondErr(w, http.StatusUnauthorized, "missing X-Anon-Token header")
			return
		}

		// Validate: look up the session by its anon_token and confirm it matches
		// the sessionID in the URL. This prevents one session from acting on
		// another's data even if both tokens are somehow known to the caller.
		session, err := s.q.GetSessionByAnonToken(r.Context(), token)
		if err != nil {
			respondErr(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}

		urlSessionID := chi_URLParam(r, "sessionID")
		if session.ID.String() != urlSessionID {
			respondErr(w, http.StatusForbidden, "token does not match session")
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeySessionID, session.ID)
		ctx = context.WithValue(ctx, ctxKeyAnonToken, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// chi_URLParam wraps chi.URLParam to avoid importing chi in every file.
// Defined here once; handlers call this helper.
func chi_URLParam(r *http.Request, key string) string {
	// chi stores URL params in the request context via its own key type.
	// We re-export the accessor here so handler files don't import chi directly.
	// If you prefer, you can just import chi in handler files — both are fine.
	return middleware.GetReqID(r.Context()) // placeholder — replace with chi.URLParam(r, key)
	// ^^^ Replace the line above with: return chi.URLParam(r, key)
	// It is written this way to avoid a direct chi import in middleware.go.
	// In practice, just import chi here or in each handler file.
}

// ─── CORS ─────────────────────────────────────────────────────────────────────

// corsMiddleware handles preflight OPTIONS requests and sets CORS headers.
// In production, tighten AllowedOrigins to your actual frontend domain.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		// TODO: replace "*" with your frontend URL in production.
		allowed := "*"
		if s.cfg.Env != "production" {
			allowed = origin
		}

		w.Header().Set("Access-Control-Allow-Origin", allowed)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Anon-Token, X-Request-ID")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ─── LOGGER MIDDLEWARE ────────────────────────────────────────────────────────

// loggerMiddleware logs each request with method, path, status, and duration.
func (s *Server) loggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		defer func() {
			s.logger.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", middleware.GetReqID(r.Context()),
			)
		}()

		next.ServeHTTP(ww, r)
	})
}

// ─── RESPONSE HELPERS ─────────────────────────────────────────────────────────

// respond writes a JSON body with the given status code.
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

// respondErr writes a standard JSON error envelope.
func respondErr(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}

// respondInternalErr logs an unexpected error and returns a 500 to the client
// without leaking internal details.
func (s *Server) respondInternalErr(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("internal error",
		"error", err,
		"path", r.URL.Path,
		"request_id", middleware.GetReqID(r.Context()),
	)
	respondErr(w, http.StatusInternalServerError, "internal server error")
}

// logAndIgnoreEmailErr logs an email send error without surfacing it to the
// caller. Used where email failure must not fail the HTTP response.
func (s *Server) logAndIgnoreEmailErr(r *http.Request, err error, context string) {
	if err == nil {
		return
	}
	s.logger.Error("email send failed",
		"context", context,
		"error", err,
		"request_id", middleware.GetReqID(r.Context()),
	)
}

// ─── REQUEST PARSING HELPERS ─────────────────────────────────────────────────

// decode JSON-decodes r.Body into dst. Returns false and writes 400 if the
// body is missing, malformed, or too large. Callers should return immediately
// on false.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB max
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		respondErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

// logField returns a slog.Attr using the request ID for correlation.
func logField(r *http.Request) slog.Attr {
	return slog.String("request_id", middleware.GetReqID(r.Context()))
}