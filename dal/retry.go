package dal

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Retryability is how safe it is to retry a failed operation, as classified
// by the backend.
type Retryability int

// Retryability classes.
const (
	// NotRetryable: retrying won't change the outcome, or the caller gave up.
	NotRetryable Retryability = iota
	// RetryableIfIdempotent: transient, but a write may already have been
	// applied (e.g. the connection dropped mid-commit).
	RetryableIfIdempotent
	// Retryable: the server did nothing or rolled back; safe for any
	// operation.
	Retryable
)

func (r Retryability) String() string {
	switch r {
	case NotRetryable:
		return "not retryable"
	case RetryableIfIdempotent:
		return "retryable if idempotent"
	case Retryable:
		return "retryable"
	}
	return fmt.Sprintf("retryability(%d)", int(r))
}

// RetryabilityOf returns the retryability recorded in err's *Error, or
// NotRetryable when err carries none.
func RetryabilityOf(err error) Retryability {
	var e *Error
	if errors.As(err, &e) {
		return e.Retryability
	}
	return NotRetryable
}

// ShouldRetry reports whether err is safe to retry for op: always for
// Retryable errors, and for RetryableIfIdempotent ones only when op is
// idempotent. A canceled or expired context is never retried.
func ShouldRetry(op Op, err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch RetryabilityOf(err) {
	case Retryable:
		return true
	case RetryableIfIdempotent:
		return op.Idempotent
	}
	return false
}

// Retrier runs fn, retrying it as its policy allows. Implementations decide
// what to retry, usually with ShouldRetry, and must respect ctx.
//
// Bring your own: any type with this method works, including an adapter over
// a library such as failsafe-go, or a RetrierFunc closure. dalforge takes no
// dependency on a retry library.
type Retrier interface {
	Do(ctx context.Context, op Op, fn func(ctx context.Context) error) error
}

// RetrierFunc lets a closure act as a Retrier.
type RetrierFunc func(ctx context.Context, op Op, fn func(ctx context.Context) error) error

// Do calls f.
func (f RetrierFunc) Do(ctx context.Context, op Op, fn func(ctx context.Context) error) error {
	return f(ctx, op, fn)
}

// NoRetry runs every operation exactly once.
var NoRetry Retrier = RetrierFunc(func(ctx context.Context, _ Op, fn func(ctx context.Context) error) error {
	return fn(ctx)
})

// Backoff retries with exponential backoff and full jitter: before retry n
// (1-based) it sleeps a random duration in [0, min(Max, Base·2^(n-1))). It
// retries only what ShouldRetry allows and stops when ctx is done.
type Backoff struct {
	Attempts int           // total attempts, including the first; < 1 means 1
	Base     time.Duration // first backoff ceiling
	Max      time.Duration // cap on any single backoff

	// sleep waits for d or until ctx is done. Tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

// DefaultRetrier is the retrier generated repositories use unless configured
// otherwise: at most 3 attempts, 25 ms base, 1 s cap.
var DefaultRetrier Retrier = Backoff{Attempts: 3, Base: 25 * time.Millisecond, Max: time.Second}

// Do implements Retrier. It returns the last error.
func (b Backoff) Do(ctx context.Context, op Op, fn func(ctx context.Context) error) error {
	sleep := b.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	attempts := max(b.Attempts, 1)

	var err error
	for attempt := 1; ; attempt++ {
		if err = fn(ctx); err == nil || attempt >= attempts || !ShouldRetry(op, err) {
			return err
		}
		if serr := sleep(ctx, b.delay(attempt)); serr != nil {
			return errors.Join(err, serr) // the context ended while waiting
		}
	}
}

// delay is the full-jitter backoff before retry n.
func (b Backoff) delay(n int) time.Duration {
	ceiling := b.Base << (n - 1)
	if ceiling <= 0 || (b.Max > 0 && ceiling > b.Max) { // <= 0 catches overflow
		ceiling = b.Max
	}
	if ceiling <= 0 {
		return 0
	}
	return rand.N(ceiling)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
