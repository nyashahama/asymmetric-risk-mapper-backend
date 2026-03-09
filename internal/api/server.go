// Package api implements the gRPC service layer for the Asymmetric Risk Mapper.
//
// Transport: pure gRPC (HTTP/2) for all RPCs.
// Stripe webhooks are served on a separate HTTP/1.1 mux in main.go because
// Stripe cannot call gRPC endpoints — see the StripeWebhookHandler() method
// which returns an http.Handler to mount alongside the gRPC server via cmux.
//
// Auth: anonymous session token sent as gRPC metadata key "x-anon-token".
// The anonTokenInterceptor validates it before any session-scoped RPC runs.
package api

import (
	"context"
	"log/slog"
	"time"

	apiv1 "github.com/nyashahama/asymmetric-risk-mapper-backend/gen/api/v1"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/email"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/store"
	stripeinternal "github.com/nyashahama/asymmetric-risk-mapper-backend/internal/stripe"
	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// Config holds values read from environment variables at startup.
// Identical to the old HTTP Config — no changes needed here.
type Config struct {
	BaseURL             string // e.g. "https://app.asymmetricrisk.com"
	StripeWebhookSecret string
	Env                 string // "production" | "staging" | "development"
}

// Server implements apiv1.RiskMapperServer. Each handler file attaches methods
// to this type and uses only the fields it needs.
type Server struct {
	// Embed the unimplemented server so new proto RPCs don't break compilation.
	apiv1.UnimplementedRiskMapperServer

	q      db.Querier
	store  *store.Store
	stripe stripeinternal.Client
	worker worker.Enqueuer
	mailer email.Sender
	cfg    Config
	logger *slog.Logger
}

// authRequiredMethods is the set of fully-qualified RPC method paths that
// require a valid X-Anon-Token. All session-scoped RPCs are listed here;
// CreateSession, GetReport, and StripeWebhook are public.
var authRequiredMethods = map[string]bool{
	"/api.v1.RiskMapper/UpdateContext":  true,
	"/api.v1.RiskMapper/GetQuestions":   true,
	"/api.v1.RiskMapper/UpsertAnswers":  true,
	"/api.v1.RiskMapper/CreateCheckout": true,
}

// NewServer constructs the gRPC server with all interceptors and registers the
// RiskMapper service. The returned *grpc.Server is ready for Serve().
func NewServer(
	q db.Querier,
	st *store.Store,
	stripeClient stripeinternal.Client,
	enqueuer worker.Enqueuer,
	mailer email.Sender,
	cfg Config,
	logger *slog.Logger,
) (*grpc.Server, *Server) {
	s := &Server{
		q:      q,
		store:  st,
		stripe: stripeClient,
		worker: enqueuer,
		mailer: mailer,
		cfg:    cfg,
		logger: logger,
	}

	grpcSrv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			s.recoveryInterceptor,
			s.loggingInterceptor,
			s.anonTokenInterceptor,
		),
		// Keep-alive: drop idle connections after 2 min, ping every 30 s.
		// Prevents silent connection failures behind load balancers.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 2 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		// Limit individual message size to 4 MB (generous — largest payload
		// is the full questions+answers list which is ~50 KB in practice).
		grpc.MaxRecvMsgSize(4*1024*1024),
		grpc.MaxSendMsgSize(4*1024*1024),
	)

	apiv1.RegisterRiskMapperServer(grpcSrv, s)
	if cfg.Env != "production" { // optional: only enable in non-prod
		reflection.Register(grpcSrv)
	}

	return grpcSrv, s
}

// ─── INTERCEPTORS ─────────────────────────────────────────────────────────────

// anonTokenInterceptor validates the "x-anon-token" metadata value for every
// RPC listed in authRequiredMethods. It also validates that the session_id
// field inside the request matches the token's owner, preventing cross-session
// access.
//
// The session ID is then stored in the context (ctxKeySessionID) so handlers
// can retrieve it without re-querying the database.
func (s *Server) anonTokenInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	if !authRequiredMethods[info.FullMethod] {
		return handler(ctx, req)
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing gRPC metadata")
	}

	tokens := md.Get("x-anon-token")
	if len(tokens) == 0 || tokens[0] == "" {
		return nil, status.Error(codes.Unauthenticated, "missing x-anon-token metadata")
	}
	token := tokens[0]

	session, err := s.q.GetSessionByAnonToken(ctx, token)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired token")
	}

	// Extract session_id from the request message and confirm it matches the
	// token's owner — prevents one session from reading another's data.
	if sid := extractSessionID(req); sid != "" && sid != session.ID.String() {
		return nil, status.Error(codes.PermissionDenied, "token does not match session_id")
	}

	ctx = context.WithValue(ctx, ctxKeySessionID, session.ID)
	ctx = context.WithValue(ctx, ctxKeyAnonToken, token)
	return handler(ctx, req)
}

// loggingInterceptor logs every RPC with method, status code, and duration.
func (s *Server) loggingInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	code := codes.OK
	if err != nil {
		code = status.Code(err)
	}
	s.logger.Info("grpc",
		"method", info.FullMethod,
		"code", code.String(),
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return resp, err
}

// recoveryInterceptor catches panics in any handler and converts them to
// gRPC Internal errors, preventing a single handler crash from killing
// the whole server goroutine.
func (s *Server) recoveryInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp any, err error) {
	defer func() {
		if p := recover(); p != nil {
			s.logger.Error("grpc: handler panic",
				"method", info.FullMethod,
				"panic", p,
			)
			err = status.Errorf(codes.Internal, "internal server error")
		}
	}()
	return handler(ctx, req)
}

// ─── CONTEXT KEYS ─────────────────────────────────────────────────────────────

type contextKey string

const (
	ctxKeySessionID contextKey = "session_id"
	ctxKeyAnonToken contextKey = "anon_token"
)

// ─── SESSION ID EXTRACTOR ─────────────────────────────────────────────────────

// sessionIDer is implemented by every request message that carries a
// session_id field. The interceptor uses this to validate cross-session access.
type sessionIDer interface {
	GetSessionId() string
}

// extractSessionID returns the session_id field from a request message, or ""
// if the message doesn't implement sessionIDer.
func extractSessionID(req any) string {
	if r, ok := req.(sessionIDer); ok {
		return r.GetSessionId()
	}
	return ""
}

// ─── INTERNAL ERROR HELPER ────────────────────────────────────────────────────

// internalErr logs the real error and returns a generic gRPC Internal status,
// matching the old respondInternalErr behaviour of not leaking internal details.
func (s *Server) internalErr(ctx context.Context, method string, err error) error {
	s.logger.Error("internal error",
		"method", method,
		"error", err,
	)
	return status.Error(codes.Internal, "internal server error")
}

// logAndIgnoreEmailErr logs an email send error without surfacing it to the
// caller. Identical in purpose to the old HTTP version.
func (s *Server) logAndIgnoreEmailErr(method string, err error, context string) {
	if err == nil {
		return
	}
	s.logger.Error("email send failed",
		"context", context,
		"method", method,
		"error", err,
	)
}
