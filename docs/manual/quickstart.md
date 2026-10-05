# Quickstart

In about ten minutes, starting from a fresh clone, you'll:

1. run the example shop end to end against a real Postgres;
2. look at everything dalforge generated from one `.proto` file;
3. change an access pattern and watch the schema and the Go API follow;
4. make a few classic mistakes and see the linter stop them;
5. try any IDL you like in a local playground;
6. run dalforge's own test suites.

The `mise run` tasks work from the repository root or from inside
`demos/`.

## 0. Prerequisites

You need [mise](https://mise.jdx.dev). It installs every other tool at a
pinned version: Go 1.26, sqlc, protoc, golangci-lint and, on macOS, a
[Colima](https://github.com/abiosoft/colima) VM with the docker CLI.
You don't need Docker Desktop or a local Go install.

```sh
make            # first time only: installs mise and activates it in your shell
exec $SHELL
mise trust && mise install
```

If you already run Docker natively, for example on Linux or in CI, set `CI=1`:
mise then uses your Docker instead of starting a Colima VM.

## 1. Run the demo

```sh
mise run demo
```

That one command:

- starts the Colima VM and a throwaway `postgres:16.9` (`compose.yaml`; data
  lives in memory);
- builds `bin/dalforge`;
- in `demos/orders`, runs `dalforge generate` (schema, queries,
  `sqlc.yaml`, then sqlc and the Go DAL), builds the app and runs it.

No Docker handy? `mise run example` does the generate-and-build part only,
so you can read every generated file in your editor without a database.

The example's `go.mod` pins the released runtime, like a real project. The
tasks add a `go.work` (gitignored) so it builds against the runtime in your
checkout instead.

The app creates a fresh `orders_demo` database, applies the generated schema
and plays out a shop's day with fake data from
[gofakeit](https://github.com/brianvoe/gofakeit). Every step checks its
outcome, and the run exits non-zero if anything is off. Pass `-seed N` to
`go run .` for repeatable data. Abridged output:

```text
1. Create accounts with fake data (ID left nil → the DAL assigns a UUIDv7)
   ✓ created Rafael Marsh <mariascott82@considine.io>
   …
3. Upsert by email: an existing email updates the name, a new one inserts
   ✓ terrywatkins@graves.net kept its id and is now "Renamed Person"
4. Place orders in transactions that lock the account (there are no foreign keys)
   ✓ placed 25 orders for Rafael Marsh and 5 each for the others: map[paid:13 pending:11 shipped:11]
   ✓ an order for an unknown account is refused and its transaction rolled back
5. A required field left nil fails before touching the database
   ✓ dal: Order.CreateOrder (create): dal: invalid argument: missing required field: account_id
6. Page through an account's orders, newest first (keyset pagination, no OFFSET)
   ✓ page 1: 10 orders, next token eyJ2IjoxLCJxIjoi…
   ✓ page 2: 10 orders, next token eyJ2IjoxLCJxIjoi…
   ✓ page 3: 5 orders, last page
   ✓ 25 orders in all, no duplicates, newest first
7. dal.All iterates every page, for in-process use
8. A page token only works for the list and filters it was issued for
   ✓ token reused for another account → dal.ErrInvalidPageToken
9. A range list: pending orders created in a time window
10. Update with the version we read (compare-and-swap); a stale version loses
   ✓ update at the old version 1 → dal.ErrVersionConflict
11. Delete is soft: lists and reads stop seeing the order
12. A hand-written sqlc query (queries/custom/reports.sql)
13. The indexes dalforge derived from the list rpcs (schema/schema.sql)
   • CREATE INDEX orders_account_id_created_at_id_idx ON orders (account_id, created_at, id) WHERE deleted_at IS NULL
       serves ListOrdersByAccount
   • CREATE INDEX orders_status_created_at_id_idx ON orders (status, created_at, id) WHERE deleted_at IS NULL
       serves ListOrdersByStatus, ListOrdersByStatusAndCreatedAt
   …
All checks passed.
```

The app's code is [`demos/orders/main.go`](../../demos/orders/main.go).
It's an ordinary Go program that only sees the generated interfaces: no SQL,
no pools, and no pgx types.

## 2. What you wrote vs. what dalforge generated

A fresh clone of `demos/orders` contains only what a dalforge user writes.
Everything generated is gitignored, so after the demo, `git status` stays
clean and you can explore the output freely:

```text
demos/orders/
├── dalforge.yaml                  yours: project config
├── dalforge.lock                  pins dalforge, its options and sqlc (a real project commits it*)
├── proto/shop/v1/shop.proto       yours: entities and access patterns
├── queries/custom/                yours: hand-written sqlc queries (the escape hatch)
│   ├── accounts.sql                 LockAccountForKeyShare
│   └── reports.sql                  OrderTotalsByStatus
├── main.go                        yours: the app
│
├── schema/schema.sql              generated: tables and derived indexes
├── queries/generated/*.sql        generated: one sqlc query per rpc (two per List)
├── sqlc.yaml                      generated: sqlc config with every type override
└── gen/
    ├── sqlcdb/                    generated by sqlc: models, queries, Querier
    └── shop/v1/shopdal/           generated: the DAL your code imports
        ├── dal.go                   model aliases, params, Read/Write/Repository interfaces
        └── repository.go            the implementation, WithTx, Tx
```

\* The example gitignores its lock because it always runs the dalforge built
from this checkout. In your project, commit `dalforge.lock` so everyone
generates with the same toolchain ([details](project-setup.md#dalforgelock)).

### The input

One message per table, one service per entity, and one rpc per access
pattern ([full file](../../demos/orders/proto/shop/v1/shop.proto)):

```proto
message Order {
  option (dal.v1.table) = {name: "orders" shard_key: ["account_id"]};

  string id = 1 [(dal.v1.field) = {format: FORMAT_UUID primary_key: true}];
  string account_id = 2 [(dal.v1.field).format = FORMAT_UUID];
  string status = 3 [(dal.pg.v1.column).default = "'pending'"];
  int64 amount_cents = 4;
  optional string note = 5;
  int64 version = 6 [(dal.v1.field).role = ROLE_VERSION];
  google.protobuf.Timestamp created_at = 7 [(dal.v1.field).role = ROLE_CREATE_TIME];
  google.protobuf.Timestamp updated_at = 8 [(dal.v1.field).role = ROLE_UPDATE_TIME];
  optional google.protobuf.Timestamp deleted_at = 9 [(dal.v1.field).role = ROLE_DELETE_TIME];
}

service OrderStore {
  option (dal.v1.store) = {entity: "Order"};

  rpc ListOrdersByAccount(ByAccount) returns (OrderPage) {
    option (dal.v1.query) = {
      list: {eq: ["account_id"] order_by: ["created_at DESC"]}
    };
  }
  // … Get, Create, Update, Delete and three more lists
}
```

### The schema

`schema/schema.sql` is derived from the messages and the access patterns. The
roles became defaults, and every List rpc got an index. Lists that can share
one index do:

```sql
CREATE TABLE orders (
    id uuid NOT NULL,
    account_id uuid NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    amount_cents bigint NOT NULL,
    note text,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    PRIMARY KEY (id)
);

-- derived for: ListOrdersByStatus, ListOrdersByStatusAndCreatedAt
CREATE INDEX orders_status_created_at_id_idx ON orders (status, created_at, id) WHERE deleted_at IS NULL;
```

### The queries

`queries/generated/orders.sql` holds plain sqlc queries. Note what you didn't
have to remember: the soft-delete filter, the version compare-and-swap, the
column default, and keyset pagination instead of `OFFSET`:

```sql
-- name: OrderUpdateOrderStatus :one
UPDATE orders
SET status = sqlc.narg(status)::text,
    note = sqlc.narg(note)::text,
    updated_at = now(),
    version = version + 1
WHERE id = @id
  AND deleted_at IS NULL
  AND version = @version
RETURNING …;

-- name: OrderListOrdersByAccountAfter :many
SELECT … FROM orders
WHERE account_id = @account_id
  AND deleted_at IS NULL
  AND (created_at, id) < (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;
```

### The Go API

`gen/shop/v1/shopdal/dal.go` is what your services depend on: models (aliases
of sqlc's structs), params, and interfaces split into read and write sides.
The doc comment on each params type says what nil means for every field:

```go
// OrderCreateOrderParams are the parameters of Order.CreateOrder.
// Fields to write are pointers; nil means:
//   - ID: a new UUIDv7
//   - AccountID: an error (required)
//   - Status: the column default 'pending'
//   - AmountCents: an error (required)
//   - Note: NULL
type OrderCreateOrderParams = sqlcdb.OrderCreateOrderParams

type OrderReadRepository interface {
	GetOrder(ctx context.Context, id uuid.UUID) (Order, error)
	ListOrdersByAccount(ctx context.Context, p OrderListOrdersByAccountParams) (dal.Page[Order], error)
	// …
}
```

Wiring it up is one line at your composition root:

```go
runner := dalpg.New(dalpg.DB{Reader: readerPool, Writer: writerPool})
orders := shopdal.NewOrderRepository(runner) // a shopdal.OrderRepository
```

## 3. Change an access pattern

Say the product needs "an account's orders in one status, newest first". Add
the rpc and its request message to `demos/orders/proto/shop/v1/shop.proto`:

```proto
  rpc ListOrdersByAccountAndStatus(ByAccountStatus) returns (OrderPage) {
    option (dal.v1.query) = {
      list: {eq: ["account_id", "status"] order_by: ["created_at DESC"]}
    };
  }

message ByAccountStatus {
  string account_id = 1;
  string status = 2;
  int32 page_size = 3;
  string page_token = 4;
}
```

Then regenerate:

```sh
cd demos/orders
dalforge lint        # silent: nothing to complain about
dalforge generate
```

- `gen/shop/v1/shopdal/dal.go` now has `ListOrdersByAccountAndStatus` and
  `OrderListOrdersByAccountAndStatusParams{AccountID, Status, PageSize, PageToken}`.
- `schema/schema.sql` has **no new index**. The new list reads the existing
  `(account_id, status, created_at, id)` index backwards, so the two now share
  it:

  ```sql
  -- derived for: ListOrdersByAccountAndStatus, ListOrdersByAccountAndStatusAndCreatedAt
  ```

Compare that with a hand-written DAL, where each new query is a chance to
forget an index or to add a redundant one.

## 4. Make some mistakes

Now break it on purpose. Sort the new list by the optional `note` field, and
mistype a field name in a Get:

```proto
      list: {eq: ["account_id", "status"] order_by: ["note"]}
  …
      get: {by: ["emial"]}
```

```text
$ dalforge lint
shop/v1/shop.proto:59:3: error DAL117: rpc GetAccountByEmail get.by: shop.v1.Account has no field "emial"
shop/v1/shop.proto:131:3: warning DAL109: rpc ListOrdersByAccountAndStatus sorts by "note", which rpc UpdateOrderStatus can change; rows can move between pages while a client pages through them
shop/v1/shop.proto:131:3: error DAL115: rpc ListOrdersByAccountAndStatus: order_by "note" is optional (nullable); keyset paging would skip rows where it is NULL. Make it required, or make it the list's range
2 error(s), 1 warning(s), 0 info
```

Each finding names the rule, the place, the consequence and the fix.
`dalforge generate` refuses to write anything while there are errors.
Every rule is explained in the [lint rule catalog](lint-rules.md).

Undo your edits with `git checkout demos/orders/proto`, then run
`dalforge generate` again.

## 5. Try anything in the playground

```sh
mise run playground          # then open http://127.0.0.1:7070
```

A local page with a proto editor on the left and, on the right, the lint
findings and every file generated from it: dalforge's schema, queries,
`sqlc.yaml` and Go DAL, plus sqlc's own Go output. It regenerates as you
type. Presets cover the basics, soft delete and versions, Postgres types
(dates, `inet`, decimals, `jsonb`, `vector`), lists and indexes, and a file
full of mistakes to fix. Click a finding to jump to its line.

It runs the same pipeline as `dalforge generate`, in a throwaway directory
per request, and listens on the loopback address only.

## 6. Run the tests

```sh
mise run check              # lint + unit and golden tests + build; no Docker, about 15 s
mise run test:integration   # the runtime against Postgres 16.9, about 5 s
```

- **Golden tests** pin every generated file for a set of fixture IDLs
  (`internal/backend/pg/testdata/`), including the sqlc output. A change to
  the generator shows up as a reviewable diff. `mise run test:update` accepts
  new output.
- **Rule fixtures** trigger each lint rule exactly once, next to controls that
  must stay silent (`internal/check/testdata`, `internal/backend/pg/testdata`).
- **Integration tests** run the runtime (`dal/dalpg`) against real Postgres:
  error mapping, retries, reader/writer routing and transactions.
- **The demo** (`mise run demo`) is the end-to-end test: IDL → generate →
  sqlc → compile → the app against Postgres.

When you're done, `mise run vm:down` stops the VM and frees about 2 GB of
memory.

## Next

- [Concepts](concepts.md): the model behind all of this, and what dalforge
  deliberately doesn't do.
- [Setting up a project](project-setup.md): use dalforge in your own repository.
- [Lists, indexes and pagination](lists.md): the part that saves you the most
  trouble.
