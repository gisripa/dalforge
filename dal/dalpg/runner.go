package dalpg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gisripa/dalforge/dal"
)

// DBTX is what a sqlc-generated Queries runs on: a pool or a transaction. It
// has the same method set as sqlc's DBTX, so sqlcdb.New accepts it.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Pool is what DB routes operations to. *pgxpool.Pool satisfies it; so does
// any wrapper you write around one, e.g. to swap the underlying pool during
// an Aurora blue/green switchover, add tracing, or pick a pool per tenant.
// Generated code never sees pools, so such wrappers need no edits to it.
type Pool interface {
	DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

var _ Pool = (*pgxpool.Pool)(nil)

// DB holds the reader and writer pools. Eventual reads use Reader; strong
// reads, writes and transactions use Writer. A single-instance setup passes
// the same pool twice, or leaves Reader nil. The pools are resolved on every
// operation, so a Pool wrapper may change what it delegates to at any time;
// a transaction stays on the connection it began on.
type DB struct {
	Reader Pool
	Writer Pool
}

// Option configures a Runner.
type Option func(*Runner)

// WithRetrier replaces the retry policy (default dal.DefaultRetrier). Any
// dal.Retrier works, including a dal.RetrierFunc closure.
func WithRetrier(r dal.Retrier) Option {
	return func(run *Runner) { run.retrier = r }
}

// WithClassifier adjusts retry classification on top of Classify, e.g. to
// treat an RDS Proxy-specific error as retryable.
func WithClassifier(c Classifier) Option {
	return func(run *Runner) { run.classify = c }
}

// Runner executes repository operations: it picks the pool, applies the
// retry policy, and turns every failure into a classified *dal.Error.
// Generated repositories hold one; it is safe for concurrent use.
//
// Every attempt runs with a context carrying the operation and attempt
// number (dal.OpFromContext), so tracers and metrics configured on the pgx
// pool see which repository method ran and whether it was a retry.
type Runner struct {
	db       DB
	retrier  dal.Retrier
	classify Classifier
	tx       pgx.Tx // set on a transaction-bound Runner
}

// New returns a Runner over db.
func New(db DB, opts ...Option) *Runner {
	r := &Runner{db: db, retrier: dal.DefaultRetrier}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Read runs a read. strong selects the writer pool for read-after-write
// consistency (CONSISTENCY_STRONG); otherwise the reader pool is used, or
// the writer when there is no reader.
func (r *Runner) Read(ctx context.Context, op dal.Op, strong bool, fn func(ctx context.Context, q DBTX) error) error {
	pool := r.db.Reader
	if strong || pool == nil {
		pool = r.db.Writer
	}
	return r.run(ctx, op, pool, fn)
}

// Write runs a write on the writer pool.
func (r *Runner) Write(ctx context.Context, op dal.Op, fn func(ctx context.Context, q DBTX) error) error {
	return r.run(ctx, op, r.db.Writer, fn)
}

func (r *Runner) run(ctx context.Context, op dal.Op, pool Pool, fn func(context.Context, DBTX) error) error {
	if r.tx != nil {
		// Inside a transaction: statements run on it and are never retried
		// on their own (a 40001 aborts the whole transaction). They carry
		// their own op and the transaction's attempt number.
		_, attempt, _ := dal.OpFromContext(ctx)
		return wrap(op, fn(dal.WithOp(ctx, op, max(attempt, 1)), r.tx), r.classify)
	}
	if pool == nil {
		return wrap(op, errors.New("dalpg: no pool configured"), r.classify)
	}
	attempt := 0 // counted here, so it is right for any Retrier
	return r.retrier.Do(ctx, op, func(ctx context.Context) error {
		attempt++
		return wrap(op, fn(dal.WithOp(ctx, op, attempt), pool), r.classify)
	})
}

// InTx runs fn in a transaction on the writer pool and commits if it returns
// nil. fn gets a Runner bound to the transaction, which generated
// repositories use for their statements. The whole transaction is retried by
// the retry policy (only when op.Idempotent allows it for ambiguous
// failures), so fn must not have side effects outside the database.
func (r *Runner) InTx(ctx context.Context, op dal.Op, fn func(ctx context.Context, tx *Runner) error) error {
	if r.tx != nil {
		return fn(ctx, r) // already in a transaction: join it
	}
	if r.db.Writer == nil {
		return wrap(op, errors.New("dalpg: no writer pool configured"), r.classify)
	}
	attempt := 0
	return r.retrier.Do(ctx, op, func(ctx context.Context) error {
		attempt++
		ctx = dal.WithOp(ctx, op, attempt)
		return wrap(op, pgx.BeginFunc(ctx, r.db.Writer, func(tx pgx.Tx) error {
			return fn(ctx, &Runner{db: r.db, retrier: dal.NoRetry, classify: r.classify, tx: tx})
		}), r.classify)
	})
}
