# The Go API

## The generated package

Each proto package gets one DAL package. `shop.v1` becomes
`gen/shop/v1/shopdal`, containing:

| File | Contents |
|---|---|
| `dal.go` | model types, params types, and the three interfaces per store |
| `repository.go` | the implementation, its constructors, `WithTx` and `Tx` |

- **Models** are named after the proto messages (`shopdal.Order`). They're
  type aliases of sqlc's structs, so there's no copying, and a custom query
  that returns the same columns returns the same type.
- **Interfaces:** per store, `<Name>ReadRepository` (Get and List),
  `<Name>WriteRepository` (Create, Update, Delete, Upsert) and
  `<Name>Repository` (both). `<Name>` is the store's name minus `Store`.
  Depend on the narrowest one that fits.
- **No driver types.** No `pgx`, `pgtype` or `pgconn` type appears in any
  exported signature, and errors are `dal` types.

## Wiring

Pools exist only at your composition root:

```go
writer, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
reader, err := pgxpool.New(ctx, os.Getenv("DATABASE_READER_URL"))

runner := dalpg.New(dalpg.DB{Reader: reader, Writer: writer})
orders := shopdal.NewOrderRepository(runner)
accounts := shopdal.NewAccountRepository(runner)

svc := NewCheckoutService(orders, accounts) // takes the interfaces
```

- **Routing:** eventual reads go to `Reader`; strong reads (`CONSISTENCY_STRONG`),
  writes and transactions go to `Writer`. With a single database, pass the
  same pool twice or leave `Reader` nil.
- **Pool wrappers:** `dalpg.Pool` is an interface that `*pgxpool.Pool`
  satisfies. Wrap it to add tracing, choose a pool per tenant, or swap the
  underlying pool at runtime (for example, during an Aurora blue/green
  switchover). The runner resolves the pool on every operation, and generated
  code never sees pools, so wrappers need no changes there.
- **Options:** `dalpg.New(db, dalpg.WithRetrier(r), dalpg.WithClassifier(c))`.
  See [Retries](#retries).

## Errors

Every error a repository returns is a `*dal.Error`:

```go
type Error struct {
	Op           dal.Op           // e.g. Order.CreateOrder (create)
	Code         string           // the SQLSTATE, when there is one
	Retryability dal.Retryability
	Err          error            // the cause; wraps the sentinel below
}
```

Match the sentinels with `errors.Is`:

| Sentinel | When |
|---|---|
| `dal.ErrNotFound` | Get, Update or Delete found no (live) row |
| `dal.ErrAlreadyExists` | a unique or primary-key violation; an upsert hit a soft-deleted row by key |
| `dal.ErrVersionConflict` | an optimistic-locking update lost: the row changed since you read it |
| `dal.ErrMissingField` | a required write parameter was nil; `errors.As` a `*dal.MissingFieldError` to get `.Field` |
| `dal.ErrInvalidPageToken` | a page token is malformed, or belongs to another list or other filters |
| `dal.ErrInvalidArgument` | the caller's mistake; `ErrMissingField` and `ErrInvalidPageToken` wrap it |

```go
o, err := orders.GetOrder(ctx, id)
switch {
case errors.Is(err, dal.ErrNotFound):
	return nil, status.Error(codes.NotFound, "no such order")
case errors.Is(err, dal.ErrInvalidArgument):
	return nil, status.Error(codes.InvalidArgument, err.Error())
case err != nil:
	return nil, err
}
```

## Retries

Repository calls retry transient failures automatically: three attempts in
total, with exponential backoff and jitter (25 ms base, capped at 1 s),
respecting the context. A retry happens only when it's safe, which depends on
both the error and the operation.

**The error's class** comes from its SQLSTATE:

| Class | Errors | Retried for |
|---|---|---|
| `Retryable` | the request never reached the server; serialization failure (`40001`), deadlock (`40P01`), lock timeout (`55P03`), too many connections (`53300`, `57P03`), read-only transaction (`25006`, an Aurora failover) | any operation |
| `RetryableIfIdempotent` | the connection was lost mid-request, or the server shut down (`08xxx`, `57P01`, `57P02`): a write may or may not have committed | idempotent operations only |
| `NotRetryable` | everything else: constraint violations, bad input, query canceled or statement timeout (`57014`), a canceled context | never |

**Which operations are idempotent:**

| Operation | Idempotent | Why |
|---|---|---|
| Get, List, Upsert, Delete | yes | repeating them leaves the same state (a repeated Delete may report `ErrNotFound`) |
| Update without a version | yes | it sets absolute values |
| Update with a version | no | a retry after an ambiguous success would report your own write as `ErrVersionConflict` |
| Create | no | it could insert twice; the DAL assigns the ID before the first attempt, so a forced retry gets `ErrAlreadyExists` instead |
| `WithTx` | no | see [Transactions](#transactions) |

**Make it yours.** The defaults are a starting point, because the right
policy depends on your topology: RDS Proxy, for example, adds its own
transient errors.

```go
// Change the policy: anything implementing dal.Retrier, or a closure.
runner := dalpg.New(db, dalpg.WithRetrier(dal.Backoff{Attempts: 5, Base: 50 * time.Millisecond, Max: 2 * time.Second}))
runner = dalpg.New(db, dalpg.WithRetrier(dal.NoRetry))
runner = dalpg.New(db, dalpg.WithRetrier(dal.RetrierFunc(
	func(ctx context.Context, op dal.Op, fn func(context.Context) error) error {
		// your loop; dal.ShouldRetry(op, err) says whether a retry is safe
		return fn(ctx)
	})))

// Adjust the classification, keeping the policy.
runner = dalpg.New(db, dalpg.WithClassifier(func(err error, base dal.Retryability) dal.Retryability {
	if isProxyBorrowTimeout(err) {
		return dal.Retryable
	}
	return base
}))
```

A retry library such as [failsafe-go](https://github.com/failsafe-go/failsafe-go)
plugs in through `dal.RetrierFunc`, with `dal.ShouldRetry` as its retry
predicate.

## Tracing and metrics

Every statement runs with a context that carries the repository operation
and the attempt number. A pgx `QueryTracer` can label spans and metrics
without wrapping any repository call:

```go
func (t *tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if op, attempt, ok := dal.OpFromContext(ctx); ok {
		// op.Entity "Order", op.Method "ListOrdersByAccount", op.Kind list; attempt 1, 2, …
		ctx, _ = otel.Tracer("db").Start(ctx, op.String(), trace.WithAttributes(attribute.Int("db.attempt", attempt)))
	}
	return ctx
}
```

Set it on the pool config (`cfg.ConnConfig.Tracer = &tracer{}`) before
creating the pool.

## Custom queries

For anything dalforge doesn't generate, such as joins, aggregates, guarded
updates or bulk operations, write a sqlc query in `queries/custom/`:

```sql
-- queries/custom/reports.sql
-- name: OrderTotalsByStatus :many
SELECT status, count(*) AS orders, sum(amount_cents)::bigint AS amount_cents
FROM orders
WHERE account_id = @account_id
  AND deleted_at IS NULL
GROUP BY status
ORDER BY status;
```

`dalforge generate` runs sqlc over generated and custom queries together, so
yours land on the same `sqlcdb.Queries` type, with the same type overrides:

```go
totals, err := sqlcdb.New(pool).OrderTotalsByStatus(ctx, accountID)
```

- **You own them.** dalforge never writes to `queries/custom/`. Remember the
  rules the generated queries follow for you: filter `deleted_at IS NULL`,
  bump `version` and `updated_at` in updates, and use a keyset, not `OFFSET`.
- **Names** mustn't collide with generated ones, which are
  `<Store minus "Store"><Method>` ([DAL205](lint-rules.md#dal205)). A plain
  descriptive name like `OrderTotalsByStatus` never does.
- **Errors** from sqlc calls are raw pgx errors. Use `dalpg.IsNoRows(err)`
  for "no row", or `dalpg.NewError(op, err)` to map one to a `*dal.Error`.
- **Wrap them** behind your own interface if your services should not see
  `sqlcdb`, just as the generated repositories do.

## Transactions

```go
err := shopdal.WithTx(ctx, runner, func(ctx context.Context, tx shopdal.Tx) error {
	if _, err := tx.Queries().LockAccountForKeyShare(ctx, accountID); err != nil {
		return err
	}
	_, err := tx.Order().CreateOrder(ctx, params)
	return err
})
```

- `WithTx` runs `fn` in one transaction on the writer pool. It commits if
  `fn` returns nil, and rolls back otherwise.
- `tx.<Store>()` gives each store's repository bound to the transaction, and
  `tx.Queries()` runs custom queries in it. There's one `WithTx` per DAL
  package, so a transaction can span all of the package's stores.
- **Use only `tx`'s repositories inside `fn`.** A repository created outside
  (`orders`, not `tx.Order()`) runs on a different connection and commits on
  its own, outside the transaction.
- **Don't swallow errors inside `fn`.** After any failed statement, Postgres
  rejects the rest of the transaction. Return the error, and handle it outside
  `WithTx`.
- **Retries re-run `fn`.** When Postgres rolls a transaction back (a
  serialization failure, a deadlock, a lock timeout), the whole transaction
  is retried, `fn` included. It's never retried after an ambiguous failure.
  Keep side effects outside the database (sending email, publishing events,
  appending to captured slices) out of `fn`.
- Transactions run at Postgres's default isolation, `READ COMMITTED`.

### Referential integrity

dalforge doesn't create foreign keys. Where one row must refer to an
existing row, check it in the same transaction, and lock the parent so it
can't be deleted before you commit:

```sql
-- queries/custom/accounts.sql
-- name: LockAccountForKeyShare :one
SELECT id FROM accounts WHERE id = @id FOR KEY SHARE;
```

`FOR KEY SHARE` is the lock a foreign key itself would take. It blocks
deleting the account and changing its key, but not ordinary updates to it.
If the query returns no row (`dalpg.IsNoRows`), the parent doesn't exist:
return an error and the transaction rolls back. The
[example](../../demos/orders/main.go) (`placeOrder`) does exactly this.

## Testing your code

Services depend on interfaces, so unit tests can use fakes:

```go
type fakeOrders struct{ shopdal.OrderReadRepository } // embed: only implement what the test calls

func (fakeOrders) GetOrder(ctx context.Context, id uuid.UUID) (shopdal.Order, error) {
	return shopdal.Order{ID: id, Status: "paid"}, nil
}
```

For the repositories themselves, test against a real Postgres. A container
per test run is cheap, and the generated SQL is the part worth testing for
real.
