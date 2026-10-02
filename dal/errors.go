// Package dal is the backend-neutral runtime that dalforge-generated data
// access layers import: sentinel errors, the Error type carrying an
// operation's retryability, and the pluggable Retrier. It depends only on the
// standard library; backend runtimes (dal/dalpg) build on it.
package dal

import (
	"errors"
	"fmt"
)

// Sentinel errors. Backends map their own errors onto these, so callers can
// use errors.Is without knowing the database driver.
var (
	// ErrNotFound means no row matched: a Get or Delete by key, or an Update
	// whose row doesn't exist (or is soft-deleted).
	ErrNotFound = errors.New("dal: not found")

	// ErrAlreadyExists means a write violated a unique constraint.
	ErrAlreadyExists = errors.New("dal: already exists")

	// ErrVersionConflict means an optimistic-locking update found the row at
	// a different version than the caller read.
	ErrVersionConflict = errors.New("dal: version conflict")

	// ErrInvalidArgument means the caller passed parameters the operation
	// can't run with. It is never retried.
	ErrInvalidArgument = errors.New("dal: invalid argument")

	// ErrMissingField means a required insert parameter was nil. It wraps
	// ErrInvalidArgument; MissingFieldError names the field.
	ErrMissingField = fmt.Errorf("%w: missing required field", ErrInvalidArgument)
)

// MissingFieldError reports which required field was nil. errors.Is matches
// both ErrMissingField and ErrInvalidArgument.
type MissingFieldError struct {
	Field string // proto field name, e.g. "account_id"
}

func (e *MissingFieldError) Error() string {
	return fmt.Sprintf("%v: %s", ErrMissingField, e.Field)
}

// Unwrap returns ErrMissingField.
func (e *MissingFieldError) Unwrap() error { return ErrMissingField }

// Error is the error every generated repository method returns. It records
// the operation, the backend's code (the SQLSTATE, for Postgres) and how safe
// a retry is. Err is the cause; it wraps the sentinel the backend mapped the
// failure to, if any, so errors.Is(err, ErrNotFound) still works.
type Error struct {
	Op           Op
	Code         string
	Retryability Retryability
	Err          error
}

func (e *Error) Error() string {
	var b []byte
	b = fmt.Appendf(b, "dal: %s", e.Op)
	if e.Code != "" {
		b = fmt.Appendf(b, " (%s)", e.Code)
	}
	b = fmt.Appendf(b, ": %v", e.Err)
	return string(b)
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }
