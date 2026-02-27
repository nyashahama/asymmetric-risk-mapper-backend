// Package store wraps db.Querier with transaction support and groups the
// multi-step write operations that must execute atomically.
//
// Single-query reads (GetSessionByID, GetReportByAccessToken, etc.) should be
// called directly on db.Querier in handlers — there is no value in proxying
// them through this package.
//
// Dependency rule: store imports db only. It never imports api, worker,
// scoring, ai, or email.
package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/nyashahama/asymmetric-risk-mapper-backend/internal/db"
)

// Store holds a *sql.DB for starting transactions and a db.Querier for
// executing queries outside of transactions. The two operation files
// (sessions.go, reports.go) attach methods to this type.
type Store struct {
	// pool is the raw connection pool, used only to begin transactions.
	pool *sql.DB

	// q is the Querier used for non-transactional calls. Handlers that hold a
	// *Store can also access it directly via store.Q() for single-query reads.
	q db.Querier
}

// New creates a Store from a live connection pool. The pool must already be
// open and verified (e.g. via db.PingContext) before calling New.
func New(pool *sql.DB, q db.Querier) *Store {
	return &Store{pool: pool, q: q}
}

// Q exposes the underlying Querier so callers (handlers, worker) can run
// single-query reads without going through a store method.
//
//	session, err := s.Q().GetSessionByID(ctx, id)
func (s *Store) Q() db.Querier {
	return s.q
}

// txQuerier is a function that receives a transactional Querier and returns an
// error. Returning a non-nil error causes withTx to roll back automatically.
type txQuerier func(ctx context.Context, q db.Querier) error

// withTx begins a transaction, passes a Querier scoped to that transaction to
// fn, and commits on success or rolls back on any error (including panics).
//
// Serializable isolation is used by default because both multi-step write
// operations involve a read-then-write pattern (checking for existing rows
// before inserting). Callers that need a different isolation level should open
// their own transaction.
func (s *Store) withTx(ctx context.Context, fn txQuerier) error {
	tx, err := s.pool.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
	})
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}

	// Roll back on panic so the connection is never left in a broken state.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p) // re-panic after rollback
		}
	}()

	// db.Queries.WithTx re-uses prepared statements scoped to the transaction.
	txQ := s.q.(*db.Queries).WithTx(tx)

	if err := fn(ctx, txQ); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			// Wrap both errors so the caller sees both failure reasons.
			return fmt.Errorf("store: fn error: %w; rollback error: %v", err, rbErr)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}
	return nil
}