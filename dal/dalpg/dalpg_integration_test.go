//go:build integration

package dalpg

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gisripa/dalforge/dal"
	"github.com/gisripa/dalforge/internal/pgtest"
)

func exec(t *testing.T, q DBTX, sql string, args ...any) {
	t.Helper()
	if _, err := q.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// TestRealErrorsAreMapped triggers real Postgres errors and checks the
// sentinel, SQLSTATE and retryability the Runner reports.
func TestRealErrorsAreMapped(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	exec(t, pool, "CREATE TABLE items (id int PRIMARY KEY, n int NOT NULL)")
	exec(t, pool, "INSERT INTO items VALUES (1, 0)")
	r := New(DB{Writer: pool}, WithRetrier(dal.NoRetry))

	err := r.Write(ctx, _op, func(ctx context.Context, q DBTX) error {
		_, err := q.Exec(ctx, "INSERT INTO items VALUES (1, 0)")
		return err
	})
	var de *dal.Error
	if !errors.Is(err, dal.ErrAlreadyExists) || !errors.As(err, &de) || de.Code != "23505" || de.Retryability != dal.NotRetryable {
		t.Errorf("duplicate insert: %v, want ErrAlreadyExists / 23505 / not retryable", err)
	}

	err = r.Read(ctx, _op, false, func(ctx context.Context, q DBTX) error {
		var n int
		return q.QueryRow(ctx, "SELECT n FROM items WHERE id = 99").Scan(&n)
	})
	if !errors.Is(err, dal.ErrNotFound) {
		t.Errorf("missing row: %v, want ErrNotFound", err)
	}
}

// TestRealSerializationFailure provokes a genuine 40001 with two conflicting
// serializable transactions.
func TestRealSerializationFailure(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	exec(t, pool, "CREATE TABLE counter (id int PRIMARY KEY, n int NOT NULL)")
	exec(t, pool, "INSERT INTO counter VALUES (1, 0)")

	serializable := pgx.TxOptions{IsoLevel: pgx.Serializable}
	tx1, err := pool.BeginTx(ctx, serializable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }() // no-op after commit
	tx2, err := pool.BeginTx(ctx, serializable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(ctx) }() // aborted by the 40001

	var n int
	for _, tx := range []pgx.Tx{tx1, tx2} { // both read the same snapshot
		if err := tx.QueryRow(ctx, "SELECT n FROM counter WHERE id = 1").Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, tx1, "UPDATE counter SET n = n + 1 WHERE id = 1")
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = tx2.Exec(ctx, "UPDATE counter SET n = n + 1 WHERE id = 1")

	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "40001" {
		t.Fatalf("second update: %v, want SQLSTATE 40001", err)
	}
	if got := Classify(err); got != dal.Retryable {
		t.Errorf("Classify(40001) = %v, want Retryable", got)
	}
}

// TestRealAdminShutdown terminates a backend mid-query: the client sees a
// genuine 57P01, which is ambiguous for writes.
func TestRealAdminShutdown(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }() // already closed by the termination

	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.Exec(ctx, "SELECT pg_sleep(30)")
		done <- err
	}()
	time.Sleep(200 * time.Millisecond) // let the sleep start
	exec(t, pool, "SELECT pg_terminate_backend($1)", pid)

	select {
	case err := <-done:
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "57P01" {
			t.Fatalf("terminated query: %v, want SQLSTATE 57P01", err)
		}
		if got := Classify(err); got != dal.RetryableIfIdempotent {
			t.Errorf("Classify(57P01) = %v, want RetryableIfIdempotent", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("terminated query did not return")
	}
}

// TestRoutingByApplicationName proves reads and writes reach different pools.
func TestRoutingByApplicationName(t *testing.T) {
	base := pgtest.New(t)
	ctx := context.Background()
	pool := func(app string) *pgxpool.Pool {
		cfg := base.Config().Copy()
		cfg.ConnConfig.RuntimeParams["application_name"] = app
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	r := New(DB{Reader: pool("reader"), Writer: pool("writer")})
	app := func(run func(func(context.Context, DBTX) error) error) string {
		var name string
		if err := run(func(ctx context.Context, q DBTX) error {
			return q.QueryRow(ctx, "SELECT current_setting('application_name')").Scan(&name)
		}); err != nil {
			t.Fatal(err)
		}
		return name
	}
	tests := []struct {
		name string
		run  func(func(context.Context, DBTX) error) error
		want string
	}{
		{name: "eventual read", run: func(fn func(context.Context, DBTX) error) error { return r.Read(ctx, _op, false, fn) }, want: "reader"},
		{name: "strong read", run: func(fn func(context.Context, DBTX) error) error { return r.Read(ctx, _op, true, fn) }, want: "writer"},
		{name: "write", run: func(fn func(context.Context, DBTX) error) error { return r.Write(ctx, _op, fn) }, want: "writer"},
	}
	for _, tt := range tests {
		if got := app(tt.run); got != tt.want {
			t.Errorf("%s ran on %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestInTx(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	exec(t, pool, "CREATE TABLE ledger (id int PRIMARY KEY)")
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM ledger").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	insert := func(id int) func(context.Context, DBTX) error {
		return func(ctx context.Context, q DBTX) error {
			_, err := q.Exec(ctx, "INSERT INTO ledger VALUES ($1)", id)
			return err
		}
	}
	txOp := dal.Op{Entity: "Ledger", Method: "Batch", Kind: dal.OpTx, Idempotent: true}

	// Commit: both statements land together.
	r := New(DB{Writer: pool}, WithRetrier(dal.NoRetry))
	if err := r.InTx(ctx, txOp, func(ctx context.Context, tx *Runner) error {
		if err := tx.Write(ctx, txOp, insert(1)); err != nil {
			return err
		}
		return tx.Write(ctx, txOp, insert(2))
	}); err != nil || count() != 2 {
		t.Fatalf("commit: err = %v, rows = %d; want 2", err, count())
	}

	// Rollback: a failing statement undoes the whole transaction.
	err := r.InTx(ctx, txOp, func(ctx context.Context, tx *Runner) error {
		if err := tx.Write(ctx, txOp, insert(3)); err != nil {
			return err
		}
		return tx.Write(ctx, txOp, insert(1)) // duplicate
	})
	if !errors.Is(err, dal.ErrAlreadyExists) || count() != 2 {
		t.Errorf("rollback: err = %v, rows = %d; want ErrAlreadyExists and still 2", err, count())
	}

	// Retry: the whole transaction reruns after a retryable failure, and
	// statements inside it aren't retried on their own.
	retrying := New(DB{Writer: pool}, WithRetrier(dal.Backoff{Attempts: 3, Base: time.Millisecond, Max: time.Millisecond}))
	runs := 0
	err = retrying.InTx(ctx, txOp, func(ctx context.Context, tx *Runner) error {
		runs++
		if err := tx.Write(ctx, txOp, insert(10+runs)); err != nil {
			return err
		}
		if runs == 1 {
			return &pgconn.PgError{Code: "40001", Message: "simulated serialization failure"}
		}
		return nil
	})
	if err != nil || runs != 2 || count() != 3 {
		t.Errorf("retry: err = %v, runs = %d, rows = %d; want success on run 2 with only its insert kept (3 rows)", err, runs, count())
	}
}

// opTracer is a pgx.QueryTracer, as an observability library would provide,
// recording which DAL operation and attempt each query belonged to.
type opTracer struct{ seen chan string }

func (o opTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if op, attempt, ok := dal.OpFromContext(ctx); ok {
		o.seen <- fmt.Sprintf("%s#%d", op.Method, attempt)
	}
	return ctx
}
func (opTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestPgxTracerSeesOp: a tracer configured on the pool (the composition
// root) labels queries with the DAL op and attempt; nothing wraps the calls.
func TestPgxTracerSeesOp(t *testing.T) {
	base := pgtest.New(t)
	ctx := context.Background()
	exec(t, base, "CREATE TABLE ledger (id int PRIMARY KEY)")

	tracer := opTracer{seen: make(chan string, 16)}
	cfg := base.Config().Copy()
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r := New(DB{Writer: pool}, WithRetrier(dal.Backoff{Attempts: 2, Base: time.Millisecond, Max: time.Millisecond}))

	insertOp := dal.Op{Entity: "Ledger", Method: "Insert", Kind: dal.OpCreate}
	if err := r.Write(ctx, insertOp, func(ctx context.Context, q DBTX) error {
		_, err := q.Exec(ctx, "INSERT INTO ledger VALUES (1)")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	txOp := dal.Op{Entity: "Ledger", Method: "Batch", Kind: dal.OpTx, Idempotent: true}
	runs := 0
	if err := r.InTx(ctx, txOp, func(ctx context.Context, tx *Runner) error {
		runs++
		if err := tx.Write(ctx, insertOp, func(ctx context.Context, q DBTX) error {
			_, err := q.Exec(ctx, "INSERT INTO ledger VALUES ($1)", 10+runs)
			return err
		}); err != nil {
			return err
		}
		if runs == 1 {
			return &pgconn.PgError{Code: "40001", Message: "simulated"}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	close(tracer.seen)
	var got []string
	for s := range tracer.seen {
		got = append(got, s)
	}
	// BEGIN/ROLLBACK/COMMIT run under the tx op; statements under their own
	// op with the transaction's attempt.
	want := []string{
		"Insert#1",                       // plain write
		"Batch#1", "Insert#1", "Batch#1", // tx attempt 1: BEGIN, insert, ROLLBACK (40001)
		"Batch#2", "Insert#2", "Batch#2", // tx attempt 2: BEGIN, insert, COMMIT
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("tracer saw %v, want %v", got, want)
	}
}
