package dal

import "context"

type opKey struct{}

type opInfo struct {
	op      Op
	attempt int
}

// WithOp returns a context carrying the repository operation and its attempt
// number (1 for the first try). Backend runtimes set it on every attempt, so
// anything downstream that receives the context, such as a pgx QueryTracer,
// can label spans and metrics with the operation and tell retries apart,
// without wrapping repository calls.
func WithOp(ctx context.Context, op Op, attempt int) context.Context {
	return context.WithValue(ctx, opKey{}, opInfo{op: op, attempt: attempt})
}

// OpFromContext returns the operation and attempt number ctx carries, and
// false if it carries none.
func OpFromContext(ctx context.Context) (Op, int, bool) {
	info, ok := ctx.Value(opKey{}).(opInfo)
	return info.op, info.attempt, ok
}
