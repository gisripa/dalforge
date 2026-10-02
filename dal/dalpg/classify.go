// Package dalpg is the Postgres runtime of dalforge-generated data access
// layers: reader/writer pool routing, SQLSTATE-based retry classification,
// mapping driver errors onto the dal sentinels, and transactions.
//
// It sits at the composition root. Services depend on the generated DAL
// interfaces, which expose no pgx types; only wiring code touches DB and the
// pools here.
package dalpg

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gisripa/dalforge/dal"
)

// SQLSTATE codes the classifier and error mapping use.
const (
	codeUniqueViolation      = "23505"
	codeSerializationFailure = "40001"
	codeDeadlockDetected     = "40P01"
	codeLockNotAvailable     = "55P03"
	codeTooManyConnections   = "53300"
	codeCannotConnectNow     = "57P03"
	codeReadOnlyTransaction  = "25006"
	codeAdminShutdown        = "57P01"
	codeCrashShutdown        = "57P02"
)

// Classifier adjusts the retryability Classify assigned to err, e.g. for an
// RDS Proxy error code. It receives Classify's result as base.
type Classifier func(err error, base dal.Retryability) dal.Retryability

// Classify is the default SQLSTATE classification (design §7):
//
//	request never reached the server (pgconn.SafeToRetry)   Retryable
//	40001 serialization failure, 40P01 deadlock               Retryable (rolled back)
//	55P03 lock not available                                 Retryable (statement failed)
//	53300 too many connections, 57P03 cannot connect now     Retryable (rejected before running)
//	25006 read-only transaction                              Retryable (Aurora failover window)
//	57P01/57P02 shutdown, 08xxx or a connection lost mid-request
//	                                                         RetryableIfIdempotent (a commit may have happened)
//	context canceled/expired, 57014 query canceled, anything else
//	                                                         NotRetryable
func Classify(err error) dal.Retryability {
	switch {
	case err == nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return dal.NotRetryable
	case pgconn.SafeToRetry(err):
		return dal.Retryable
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case codeSerializationFailure, codeDeadlockDetected, codeLockNotAvailable,
			codeTooManyConnections, codeCannotConnectNow, codeReadOnlyTransaction:
			return dal.Retryable
		case codeAdminShutdown, codeCrashShutdown:
			return dal.RetryableIfIdempotent
		}
		if strings.HasPrefix(pgErr.Code, "08") { // connection exception class
			return dal.RetryableIfIdempotent
		}
		return dal.NotRetryable
	}

	// A connection that broke mid-request: the statement may or may not have
	// run.
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return dal.RetryableIfIdempotent
	}
	return dal.NotRetryable
}

// wrap turns any error from an operation into a *dal.Error: it maps driver
// errors onto the dal sentinels, records the SQLSTATE, and classifies
// retryability (Classify, then the optional classifier). A *dal.Error is
// returned unchanged.
func wrap(op dal.Op, err error, classify Classifier) error {
	if err == nil {
		return nil
	}
	var de *dal.Error
	if errors.As(err, &de) {
		return err
	}

	out := &dal.Error{Op: op, Err: err}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		out.Code = pgErr.Code
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		out.Err = errors.Join(dal.ErrNotFound, err)
	case out.Code == codeUniqueViolation:
		out.Err = errors.Join(dal.ErrAlreadyExists, err)
	}

	if errors.Is(err, dal.ErrInvalidArgument) {
		out.Retryability = dal.NotRetryable // caller errors are never retried
		return out
	}
	out.Retryability = Classify(err)
	if classify != nil {
		out.Retryability = classify(err, out.Retryability)
	}
	return out
}
