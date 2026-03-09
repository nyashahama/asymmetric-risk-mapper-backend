package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	apiv1 "github.com/nyashahama/asymmetric-risk-mapper-backend/gen/api/v1"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ─── CreateSession ────────────────────────────────────────────────────────────
//
// Public RPC — no auth required. Creates an anonymous session and returns the
// session ID and a cryptographically random anon token.
//
// UTM params and browser metadata previously extracted from HTTP query params
// and headers are now fields in the request message — the client is responsible
// for reading them from window.location and document.referrer and including
// them in the request.

func (s *Server) CreateSession(
	ctx context.Context,
	req *apiv1.CreateSessionRequest,
) (*apiv1.CreateSessionResponse, error) {
	// Generate a cryptographically random 32-byte token → 64 hex chars.
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, s.internalErr(ctx, "CreateSession", fmt.Errorf("generate anon token: %w", err))
	}
	anonToken := hex.EncodeToString(tokenBytes)

	// Hash the peer IP for fraud logging. In gRPC the remote address comes
	// from peer.FromContext. Behind a proxy, the real IP may be in the
	// "x-real-ip" metadata key — we check both.
	ipHash := hashPeerIP(ctx)

	// Pull user-agent from gRPC metadata (set by the client SDK).
	userAgent := firstMeta(ctx, "user-agent")

	params := db.CreateSessionParams{
		AnonToken:   anonToken,
		UtmSource:   nullString(req.UtmSource),
		UtmMedium:   nullString(req.UtmMedium),
		UtmCampaign: nullString(req.UtmCampaign),
		Referrer:    nullString(req.Referrer),
		IpHash:      nullString(ipHash),
		UserAgent:   nullString(userAgent),
	}

	session, err := s.q.CreateSession(ctx, params)
	if err != nil {
		return nil, s.internalErr(ctx, "CreateSession", fmt.Errorf("create session: %w", err))
	}

	// If context was provided at creation time, persist it immediately.
	// Non-fatal — context can be set via UpdateContext later.
	if req.BizName != "" || req.Industry != "" || req.Stage != "" {
		_, err = s.q.UpdateSessionContext(ctx, db.UpdateSessionContextParams{
			ID:       session.ID,
			BizName:  nullString(req.BizName),
			Industry: nullString(req.Industry),
			Stage:    nullString(req.Stage),
		})
		if err != nil {
			s.logger.Warn("CreateSession: failed to set initial context",
				"session_id", session.ID,
				"error", err,
			)
		}
	}

	return &apiv1.CreateSessionResponse{
		SessionId: session.ID.String(),
		AnonToken: anonToken,
	}, nil
}

// ─── UpdateContext ────────────────────────────────────────────────────────────
//
// Protected by anonTokenInterceptor. Persists the business context collected
// in Step 1 of the assessment (ContextStep).

func (s *Server) UpdateContext(
	ctx context.Context,
	req *apiv1.UpdateContextRequest,
) (*apiv1.UpdateContextResponse, error) {
	sessionID, err := parseUUID(req.SessionId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid session_id")
	}

	if req.BizName == "" && req.Industry == "" && req.Stage == "" {
		return nil, status.Error(codes.InvalidArgument, "at least one context field must be non-empty")
	}

	session, err := s.q.UpdateSessionContext(ctx, db.UpdateSessionContextParams{
		ID:       sessionID,
		BizName:  nullString(req.BizName),
		Industry: nullString(req.Industry),
		Stage:    nullString(req.Stage),
	})
	if err != nil {
		return nil, s.internalErr(ctx, "UpdateContext", fmt.Errorf("update context: %w", err))
	}

	return &apiv1.UpdateContextResponse{
		SessionId: session.ID.String(),
		BizName:   session.BizName.String,
		Industry:  session.Industry.String,
		Stage:     session.Stage.String,
	}, nil
}

// ─── HELPERS ─────────────────────────────────────────────────────────────────

// hashPeerIP extracts the client IP from the gRPC peer info or the x-real-ip
// metadata key (set by a reverse proxy), then returns its SHA-256 hex hash.
func hashPeerIP(ctx context.Context) string {
	// Check x-real-ip metadata first (set by Nginx/Envoy in front of the service).
	if ip := firstMeta(ctx, "x-real-ip"); ip != "" {
		return hashIP(ip)
	}
	// Fall back to the transport-level peer address.
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr := p.Addr.String()
		// addr is "ip:port" — strip port.
		for i := len(addr) - 1; i >= 0; i-- {
			if addr[i] == ':' {
				addr = addr[:i]
				break
			}
		}
		return hashIP(addr)
	}
	return ""
}

// hashIP returns the hex-encoded SHA-256 of the IP string.
func hashIP(ip string) string {
	h := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(h[:])
}

// firstMeta returns the first value of a metadata key, or "".
func firstMeta(ctx context.Context, key string) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(key)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}