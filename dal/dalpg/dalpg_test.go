package dalpg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gisripa/dalforge/dal"
)

var _op = dal.Op{Entity: "Order", Method: "GetById", Kind: dal.OpGet, Idempotent: true}

func pgErr(code string) error { return &pgconn.PgError{Code: code, Message: "test " + code} }

// safeErr reports SafeToRetry like pgconn's errors for requests never sent.
type safeErr struct{}

func (safeErr) Error() string     { return "not sent" }
func (safeErr) SafeToRetry() bool { return true }

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want dal.Retryability
	}{
		{name: "nil", err: nil, want: dal.NotRetryable},
		{name: "never sent", err: safeErr{}, want: dal.Retryable},
		{name: "serialization failure", err: pgErr("40001"), want: dal.Retryable},
		{name: "deadlock", err: pgErr("40P01"), want: dal.Retryable},
		{name: "lock timeout", err: pgErr("55P03"), want: dal.Retryable},
		{name: "too many connections", err: pgErr("53300"), want: dal.Retryable},
		{name: "cannot connect now", err: pgErr("57P03"), want: dal.Retryable},
		{name: "read-only (failover)", err: pgErr("25006"), want: dal.Retryable},
		{name: "admin shutdown", err: pgErr("57P01"), want: dal.RetryableIfIdempotent},
		{name: "crash shutdown", err: pgErr("57P02"), want: dal.RetryableIfIdempotent},
		{name: "connection exception class", err: pgErr("08006"), want: dal.RetryableIfIdempotent},
		{name: "query canceled", err: pgErr("57014"), want: dal.NotRetryable},
		{name: "unique violation", err: pgErr("23505"), want: dal.NotRetryable},
		{name: "wrapped code", err: fmt.Errorf("q: %w", pgErr("40001")), want: dal.Retryable},
		{name: "network error", err: &net.OpError{Op: "read", Err: errors.New("reset")}, want: dal.RetryableIfIdempotent},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF, want: dal.RetryableIfIdempotent},
		{name: "canceled", err: context.Canceled, want: dal.NotRetryable},
		{name: "other", err: errors.New("x"), want: dal.NotRetryable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Errorf("Classify() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWrap(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		classify  Classifier
		sentinel  error // errors.Is target; nil for none
		code      string
		retryable dal.Retryability
	}{
		{name: "no rows", err: pgx.ErrNoRows, sentinel: dal.ErrNotFound},
		{name: "unique violation", err: pgErr("23505"), sentinel: dal.ErrAlreadyExists, code: "23505"},
		{name: "serialization", err: pgErr("40001"), code: "40001", retryable: dal.Retryable},
		{name: "missing field", err: &dal.MissingFieldError{Field: "account_id"}, sentinel: dal.ErrMissingField},
		{name: "classifier override", err: pgErr("57014"), code: "57014", retryable: dal.Retryable,
			classify: func(err error, base dal.Retryability) dal.Retryability {
				var pe *pgconn.PgError
				if errors.As(err, &pe) && pe.Code == "57014" {
					return dal.Retryable
				}
				return base
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := wrap(_op, tt.err, tt.classify)
			var de *dal.Error
			if !errors.As(err, &de) {
				t.Fatalf("wrap() = %T, want *dal.Error", err)
			}
			if !errors.Is(err, tt.err) {
				t.Error("the original error must stay reachable")
			}
			if tt.sentinel != nil && !errors.Is(err, tt.sentinel) {
				t.Errorf("errors.Is(%v) = false", tt.sentinel)
			}
			if de.Code != tt.code || de.Retryability != tt.retryable || de.Op != _op {
				t.Errorf("got code %q, %v, op %v; want %q, %v", de.Code, de.Retryability, de.Op, tt.code, tt.retryable)
			}
		})
	}
	if wrap(_op, nil, nil) != nil {
		t.Error("wrap(nil) must be nil")
	}
	already := &dal.Error{Op: _op, Err: errors.New("x")}
	if wrap(_op, already, nil) != error(already) {
		t.Error("a *dal.Error must pass through unchanged")
	}
}

// lazyPool makes a pool that never connects as long as it isn't used.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestRouting(t *testing.T) {
	reader, writer := lazyPool(t), lazyPool(t)
	ctx := context.Background()
	got := func(run func(func(context.Context, DBTX) error) error) DBTX {
		var q DBTX
		if err := run(func(_ context.Context, db DBTX) error { q = db; return nil }); err != nil {
			t.Fatal(err)
		}
		return q
	}

	r := New(DB{Reader: reader, Writer: writer})
	if q := got(func(fn func(context.Context, DBTX) error) error { return r.Read(ctx, _op, false, fn) }); q != DBTX(reader) {
		t.Error("eventual read must use the reader")
	}
	if q := got(func(fn func(context.Context, DBTX) error) error { return r.Read(ctx, _op, true, fn) }); q != DBTX(writer) {
		t.Error("strong read must use the writer")
	}
	if q := got(func(fn func(context.Context, DBTX) error) error { return r.Write(ctx, _op, fn) }); q != DBTX(writer) {
		t.Error("write must use the writer")
	}
	single := New(DB{Writer: writer})
	if q := got(func(fn func(context.Context, DBTX) error) error { return single.Read(ctx, _op, false, fn) }); q != DBTX(writer) {
		t.Error("without a reader, reads must use the writer")
	}
	if err := New(DB{}).Write(ctx, _op, func(context.Context, DBTX) error { return nil }); err == nil {
		t.Error("no pool configured must be an error")
	}
}

func TestRetriesClassifiedErrors(t *testing.T) {
	writer := lazyPool(t)
	attempts := 0
	counting := dal.RetrierFunc(func(ctx context.Context, op dal.Op, fn func(context.Context) error) error {
		var err error
		for range 3 {
			attempts++
			if err = fn(ctx); err == nil || !dal.ShouldRetry(op, err) {
				return err
			}
		}
		return err
	})
	r := New(DB{Writer: writer}, WithRetrier(counting))

	calls := 0
	err := r.Write(context.Background(), _op, func(context.Context, DBTX) error {
		calls++
		if calls == 1 {
			return pgErr("40001") // the retrier sees it already classified Retryable
		}
		return nil
	})
	if err != nil || attempts != 2 {
		t.Errorf("err = %v, attempts = %d; want success on attempt 2", err, attempts)
	}

	attempts = 0
	err = r.Write(context.Background(), _op, func(context.Context, DBTX) error { return pgErr("23505") })
	if !errors.Is(err, dal.ErrAlreadyExists) || attempts != 1 {
		t.Errorf("unique violation: err = %v, attempts = %d; want ErrAlreadyExists after 1 attempt", err, attempts)
	}
}
