# Access patterns

Each rpc in a store declares one access pattern in `(dal.v1.query)`. This
chapter covers the single-row patterns: `get`, `create`, `update`, `delete`
and `upsert`. Lists have [their own chapter](lists.md).

Two rules apply to all of them:

- **Soft-deleted rows don't exist.** On an entity with `ROLE_DELETE_TIME`,
  every generated statement filters `deleted_at IS NULL`. A soft-deleted row
  can't be read, updated, deleted again or revived by an upsert.
- **Errors are `dal` sentinels:** `dal.ErrNotFound`, `dal.ErrAlreadyExists`,
  `dal.ErrVersionConflict`, `dal.ErrMissingField`. See
  [The Go API](go-api.md#errors).

Method signatures: a pattern with a single parameter takes it directly
(`GetOrder(ctx, id)`). Otherwise it takes a params struct
(`CreateOrder(ctx, p)`).

## Get

```proto
rpc GetOrder(OrderKey) returns (Order) {
  option (dal.v1.query) = {get: {}};                    // by the primary key
}
rpc GetAccountByEmail(EmailKey) returns (Account) {
  option (dal.v1.query) = {get: {by: ["email"]}};       // by a unique field
}
```

```go
o, err := orders.GetOrder(ctx, id)             // dal.ErrNotFound if absent or soft-deleted
a, err := accounts.GetAccountByEmail(ctx, email)
```

- `by` defaults to the primary key. Any other set of fields must be backed by
  a unique field or a unique index, so the lookup can match at most one row
  ([DAL104](lint-rules.md#dal104)). A lookup that can match several rows is a
  [list](lists.md).
- `consistency: CONSISTENCY_STRONG` reads from the writer pool, for
  read-after-write. By default, reads go to the reader pool.

## Create

```proto
rpc CreateOrder(Order) returns (Order) {
  option (dal.v1.query) = {create: {}};
}
```

```go
o, err := orders.CreateOrder(ctx, shopdal.OrderCreateOrderParams{
	AccountID:   new(accountID),
	AmountCents: new(int64(1999)),
	// ID nil → a new UUIDv7. Status nil → the column default 'pending'.
	// Note nil → NULL.
})
```

**Every field in a Create's params is a pointer**, and nil has one defined
meaning per field:

| Field | nil means |
|---|---|
| required, no default | an error: `dal.ErrMissingField`, raised before any database call (a `*dal.MissingFieldError` names the field) |
| required, with a `default` | the column default |
| `optional`, no default | `NULL` |
| `optional`, with a `default` | the column default, so Create can't insert NULL here ([DAL212](lint-rules.md#dal212)); Update can |
| primary key with `FORMAT_UUID` | a new UUIDv7, assigned by the DAL |
| role fields | not in the params; the DAL manages them |

Why pointers? A generated INSERT lists every column, so with plain values a
forgotten field would silently insert `""`, `0` or the zero UUID, and a
column default would never apply. Go 1.26's `new(expr)` keeps call sites
short. The generated doc comment on each params type spells out what nil
means for each field.

- Returns the inserted row, including the defaults and roles the database
  filled in.
- A duplicate key or unique value returns `dal.ErrAlreadyExists`.
- Create isn't retried after an ambiguous failure, such as a connection lost
  mid-request, because it isn't idempotent. The ID is assigned before the
  first attempt, though, so a custom retry policy that retries anyway gets
  `ErrAlreadyExists` rather than a duplicate row.

## Update

```proto
rpc UpdateOrderStatus(Order) returns (Order) {
  option (dal.v1.query) = {
    update: {columns: ["status", "note"]}
  };
}
```

```go
o, err = orders.UpdateOrderStatus(ctx, shopdal.OrderUpdateOrderStatusParams{
	ID:      o.ID,
	Version: o.Version,          // the version you read
	Status:  new("shipped"),
	Note:    nil,                // optional: nil sets NULL
})
```

- **`columns`** lists the fields this update sets. It defaults to every field
  that's neither a key nor a role. Keys and role fields can't be listed
  ([DAL118](lint-rules.md#dal118)). Prefer narrow updates: one rpc per
  business operation, each naming its columns.
- **Every listed field is a pointer:** nil on a required field is
  `dal.ErrMissingField`, and nil on an `optional` field sets NULL. Beware:
  forgetting an optional field clears it.
- **The key** (and the version) are plain values: they identify the row.
- `updated_at` (`ROLE_UPDATE_TIME`) is set to `now()`.

### Optimistic locking

With a `ROLE_VERSION` field, every update is a compare-and-swap:

```sql
UPDATE orders SET …, version = version + 1
WHERE id = @id AND deleted_at IS NULL AND version = @version
```

If no row matched, the DAL checks whether the row exists and returns
`dal.ErrVersionConflict` (someone else changed it since you read it) or
`dal.ErrNotFound`. On a conflict, re-read the row and decide again. That
decision is your business logic, so dalforge doesn't retry it for you.

Without a version field, an update simply overwrites (last writer wins).

Updates that need more than this, such as "only if status is still pending"
or "increment a counter", are [custom queries](go-api.md#custom-queries).

## Delete

```proto
rpc DeleteOrder(OrderKey) returns (Order) {               // returns the deleted row
  option (dal.v1.query) = {delete: {}};
}
rpc DeleteSession(SessionKey) returns (google.protobuf.Empty) {
  option (dal.v1.query) = {delete: {}};
}
```

- Deletes by primary key. A missing (or already deleted) row returns
  `dal.ErrNotFound`.
- **With `ROLE_DELETE_TIME`, delete is soft:** it sets `deleted_at = now()`,
  bumps `updated_at` and the version, and keeps the row. Every generated read
  then ignores it. Unique fields on soft-delete tables are unique among live
  rows only, so a deleted row doesn't block re-creating its email.
- Without it, the row is deleted for real.
- With `google.protobuf.Empty` as the response, the Go method returns only
  `error`.

## Upsert

```proto
rpc UpsertAccountByEmail(Account) returns (Account) {
  option (dal.v1.query) = {
    upsert: {
      conflict_on: ["email"]   // default: the primary key
      columns: ["name"]        // updated on conflict; default: like update
    }
  };
}
```

```go
a, err := accounts.UpsertAccountByEmail(ctx, shopdal.AccountUpsertAccountByEmailParams{
	Email: new("ana@example.com"),
	Name:  new("Ana"),
})
```

Inserts the row, or, if the conflict target already exists, updates
`columns` on it:

- **The parameters follow Create's rules:** pointers, defaults, missing
  fields, and a UUIDv7 for a nil key. On conflict, the existing row keeps its
  key.
- **`conflict_on`** must be the primary key, a unique field or a unique
  index ([DAL104](lint-rules.md#dal104)).
- **The last writer wins:** the version is bumped, but there's no
  compare-and-swap. That makes upsert idempotent, so it's safe to retry.
- **Soft-deleted rows are never revived.** If `conflict_on` is the key and the
  existing row is soft-deleted, upsert returns `dal.ErrAlreadyExists`. If it's
  a unique field, the soft-deleted row doesn't count as a conflict (the
  unique index covers live rows only), so a new live row is inserted.

Anything richer, such as "insert or increment" or conditional upserts, is a
[custom query](go-api.md#custom-queries).
