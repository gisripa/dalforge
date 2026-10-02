package dal

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var (
	errDriver = errors.New("driver: boom")
	_readOp   = Op{Entity: "Order", Method: "GetById", Kind: OpGet, Idempotent: true}
	_createOp = Op{Entity: "Order", Method: "Create", Kind: OpCreate, Idempotent: false}
)

func classified(r Retryability) error {
	return &Error{Op: _readOp, Code: "40001", Retryability: r, Err: errDriver}
}

func TestErrorWrapping(t *testing.T) {
	mf := error(&MissingFieldError{Field: "account_id"})
	if !errors.Is(mf, ErrMissingField) || !errors.Is(mf, ErrInvalidArgument) {
		t.Errorf("MissingFieldError must match ErrMissingField and ErrInvalidArgument")
	}
	if got := mf.Error(); got != "dal: invalid argument: missing required field: account_id" {
		t.Errorf("MissingFieldError.Error() = %q", got)
	}

	// A backend maps a driver error to a sentinel while keeping the cause.
	err := error(&Error{Op: _readOp, Code: "P0002", Err: fmt.Errorf("%w: %w", ErrNotFound, errDriver)})
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, errDriver) {
		t.Errorf("Error must match both the sentinel and the driver cause")
	}
	if got := err.Error(); got != "dal: Order.GetById (get) (P0002): dal: not found: driver: boom" {
		t.Errorf("Error.Error() = %q", got)
	}
	if got := (&Error{Op: _createOp, Err: errDriver}).Error(); got != "dal: Order.Create (create): driver: boom" {
		t.Errorf("Error.Error() without code = %q", got)
	}
}

func TestShouldRetry(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		err  error
		want bool
	}{
		{name: "nil", op: _readOp, err: nil, want: false},
		{name: "unclassified", op: _readOp, err: errDriver, want: false},
		{name: "not retryable", op: _readOp, err: classified(NotRetryable), want: false},
		{name: "retryable, non-idempotent op", op: _createOp, err: classified(Retryable), want: true},
		{name: "if-idempotent, idempotent op", op: _readOp, err: classified(RetryableIfIdempotent), want: true},
		{name: "if-idempotent, non-idempotent op", op: _createOp, err: classified(RetryableIfIdempotent), want: false},
		{name: "wrapped classification", op: _readOp, err: fmt.Errorf("svc: %w", classified(Retryable)), want: true},
		{name: "canceled context", op: _readOp, err: errors.Join(classified(Retryable), context.Canceled), want: false},
		{name: "deadline", op: _readOp, err: context.DeadlineExceeded, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldRetry(tt.op, tt.err); got != tt.want {
				t.Errorf("ShouldRetry() = %v, want %v", got, tt.want)
			}
		})
	}
}

// failing returns fn that fails with errs[i] on call i (nil once exhausted),
// and a pointer to the call count.
func failing(errs ...error) (func(context.Context) error, *int) {
	calls := 0
	return func(context.Context) error {
		calls++
		if calls <= len(errs) {
			return errs[calls-1]
		}
		return nil
	}, &calls
}

func TestBackoff(t *testing.T) {
	var slept []time.Duration
	noSleep := func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	retryable, notRetryable := classified(Retryable), classified(NotRetryable)

	tests := []struct {
		name      string
		attempts  int
		op        Op
		errs      []error
		wantCalls int
		wantErr   error
	}{
		{name: "succeeds first time", attempts: 3, op: _readOp, wantCalls: 1},
		{name: "succeeds after retries", attempts: 3, op: _readOp, errs: []error{retryable, retryable}, wantCalls: 3},
		{name: "gives up after attempts", attempts: 3, op: _readOp, errs: []error{retryable, retryable, retryable, retryable}, wantCalls: 3, wantErr: retryable},
		{name: "stops on not retryable", attempts: 3, op: _readOp, errs: []error{notRetryable}, wantCalls: 1, wantErr: notRetryable},
		{name: "if-idempotent create is not retried", attempts: 3, op: _createOp, errs: []error{classified(RetryableIfIdempotent)}, wantCalls: 1, wantErr: classified(RetryableIfIdempotent)},
		{name: "attempts below 1 means 1", attempts: 0, op: _readOp, errs: []error{retryable}, wantCalls: 1, wantErr: retryable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slept = nil
			b := Backoff{Attempts: tt.attempts, Base: 10 * time.Millisecond, Max: 40 * time.Millisecond, sleep: noSleep}
			fn, calls := failing(tt.errs...)
			err := b.Do(context.Background(), tt.op, fn)
			if *calls != tt.wantCalls {
				t.Errorf("calls = %d, want %d", *calls, tt.wantCalls)
			}
			if (err == nil) != (tt.wantErr == nil) || (err != nil && err.Error() != tt.wantErr.Error()) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if len(slept) != max(tt.wantCalls-1, 0) {
				t.Errorf("slept %d times, want %d (once between attempts)", len(slept), tt.wantCalls-1)
			}
		})
	}
}

func TestBackoffStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := Backoff{Attempts: 5, Base: time.Hour, Max: time.Hour} // real sleep: must be cut short
	calls := 0
	err := b.Do(ctx, _readOp, func(context.Context) error {
		calls++
		cancel()
		return classified(Retryable)
	})
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if !errors.Is(err, context.Canceled) || RetryabilityOf(err) != Retryable {
		t.Errorf("err = %v, want the operation's error joined with context.Canceled", err)
	}
}

func TestBackoffDelay(t *testing.T) {
	b := Backoff{Base: 10 * time.Millisecond, Max: 40 * time.Millisecond}
	for n, ceiling := range map[int]time.Duration{1: 10 * time.Millisecond, 2: 20 * time.Millisecond, 3: 40 * time.Millisecond, 4: 40 * time.Millisecond, 80: 40 * time.Millisecond} {
		for range 50 {
			if d := b.delay(n); d < 0 || d >= ceiling {
				t.Fatalf("delay(%d) = %v, want in [0, %v)", n, d, ceiling)
			}
		}
	}
	if d := (Backoff{}).delay(1); d != 0 {
		t.Errorf("zero Backoff delay = %v, want 0", d)
	}
}

func TestRetrierFuncAndNoRetry(t *testing.T) {
	fn, calls := failing(classified(Retryable))
	if err := NoRetry.Do(context.Background(), _readOp, fn); err == nil || *calls != 1 {
		t.Errorf("NoRetry: calls = %d, err = %v; want 1 call and the error", *calls, err)
	}

	var seen Op
	r := RetrierFunc(func(ctx context.Context, op Op, fn func(context.Context) error) error {
		seen = op
		return fn(ctx)
	})
	if err := r.Do(context.Background(), _createOp, func(context.Context) error { return nil }); err != nil || seen != _createOp {
		t.Errorf("RetrierFunc: err = %v, op = %v", err, seen)
	}
}

func TestStrings(t *testing.T) {
	for got, want := range map[string]string{
		OpList.String():                "list",
		OpKind(99).String():            "opkind(99)",
		RetryableIfIdempotent.String(): "retryable if idempotent",
		Retryability(9).String():       "retryability(9)",
		_readOp.String():               "Order.GetById (get)",
		OpTx.String():                  "tx",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
