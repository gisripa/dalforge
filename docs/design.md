# DALForge design

Status: draft. The IDL options are sketched in
[`proto/dal/v1/options.proto`](../proto/dal/v1/options.proto) (backend-neutral)
and [`proto/dal/pg/v1/options.proto`](../proto/dal/pg/v1/options.proto)
(Postgres). Nothing is generated yet.

## 1. Problem and goals

ORMs such as Ent and Gorm map tables to typed objects but allow any query
shape. Query builders such as squirrel make it just as easy. Product-driven code
then accumulates queries that don't match the indexes: composite indexes in the
wrong equality/range order, `LIMIT/OFFSET` deep paging, and one index per API
shape until inserts slow down. sqlc gets the model right, with queries written
for known access patterns and typed code generated from them, but every query
is still written by hand.

DALForge is a **meta-generator**. Engineers declare *access patterns* in a
protobuf IDL. DALForge then derives the physical storage and a typed Go data
access layer. **Postgres is the reference backend**, built on sqlc and pgx v5.
It generates:

- the schema (`CREATE TABLE`) and the indexes those access patterns need;
- sqlc query files for CRUD-by-key, keyset-paginated List, upsert and soft
  delete;
- a `sqlc.yaml` with sqlc's safety features switched on;
- typed Go domain structs and `ReadRepository` / `WriteRepository` /
  `Repository` interfaces, with reads and writes routed to reader and writer
  pools;
- versioned migrations, linted for n+1 rolling-deploy compatibility.

Handwritten sqlc queries remain a first-class escape hatch alongside the
generated ones.

**The design is layered so a second backend (DynamoDB) can be added** without
changing the IDL model or the domain-facing interfaces. Only the Postgres
backend is built for now (§3).

**Non-goals**
- Complex queries (joins, aggregates, CTEs). Write those as custom sqlc
  queries.
- Foreign keys. They don't work well with sharded tables.
- Shard routing. A `shard_key` can be declared and is validated, but nothing
  routes or enforces by it yet.
- Batch operations (`CreateMany`, `GetMany`).
- Exposing the IDL as a service contract. The proto is only an IDL. The
  generated code is a library the service layer consumes.
- Postgres older than 16, drivers other than pgx v5, and Go older than 1.26.
  The target is Aurora PostgreSQL 16.x.

## 2. Architecture

```
*.proto ──protocompile──▶ descriptors ──▶ core IR (entities, fields, access paths)
                                               │
                                  core lint ◀──┤
                                               ▼
                               ┌──────── backend (pg today) ────────┐
                               │ physical model (tables, indexes)   │
                               │ backend lint                       │
                               │ emitters: schema.sql, queries,     │
                               │   sqlc.yaml, repo impl, migrations │
                               └────────────────────────────────────┘
                                               │
                        core emitter: domain structs + repository interfaces
```

- **A standalone CLI** (`dalforge generate | lint | migrate`) that embeds
  `github.com/bufbuild/protocompile`, so neither buf nor protoc is needed at
  generation time. Being a CLI rather than a protoc plugin lets it read and
  write state on disk, namely the schema snapshot that migration diffing
  depends on. A buf plugin mode can be added later over the same core.
- **The options protos ship inside the binary**, so users can
  `import "dal/v1/options.proto"` without vendoring it. They can also be
  published to the BSR for editor and `buf lint` support.
- **sqlc is pinned as a Go `tool` dependency** of the user's module and run as
  `go tool sqlc generate`. That keeps the version reproducible without a global
  install.
- **The core IR is the contract between stages.** Lint rules and emitters only
  see the IR, never the descriptors. That makes both testable with plain Go
  values.

## 3. Layering

Every concern sits in exactly one layer. A new backend implements the backend
layer. It never changes the IDL model or the core interfaces.

| Layer | IDL | Generator | Generated code / runtime |
|---|---|---|---|
| **Core** (backend-neutral) | `dal.v1`: entity, fields, format, roles, lifecycle, shard key, access patterns, consistency | IR, access-path resolution, core lint (DAL1xx), domain + interface emitter | domain structs, `<Entity>ReadRepository` / `WriteRepository` / `Repository`, runtime `dal` (`Page[T]`, `All`, page-token envelope, sentinel errors) |
| **Backend** (pg today) | `dal.pg.v1`: physical types, SQL defaults, explicit/partial/covering indexes, index budget | physical model, backend lint (DAL2xx, DAL3xx), schema/query/sqlc/migration emitters | `<pkg>pg` repository implementation, runtime `dal/dalpg` (`DB` pools, keyset codec, error mapping), backend extras such as `WithTx` |

**Rules that keep the core portable:**
- Nothing in `dal.v1` names a physical type, an index or a query language.
  `format: FORMAT_UUID` means "logical UUID". Postgres maps it to `uuid`; a
  DynamoDB backend would map it to `S`.
- Access patterns are **access paths**: equality prefix, sort, range, and
  primary-key tie-breaker. Each backend turns an access path into physical
  structures. For Postgres that's a B-tree index; for DynamoDB it would be a
  table key or a GSI.
- Read consistency is declared as intent (`CONSISTENCY_STRONG`), never as
  "use the writer pool".
- Page tokens are opaque strings in the interfaces. What's inside them belongs
  to the backend (§6).
- Anything with backend-specific semantics, such as transactions, lives on the
  backend's implementation type, not the core interface.

### How the concepts map to DynamoDB

This is a feasibility check only; the DynamoDB backend isn't built.

| Concept | Postgres | DynamoDB |
|---|---|---|
| entity | table | table (one per entity; single-table design out of scope) |
| primary key | PK constraint | partition key (+ sort key for composite keys) |
| List access path | composite B-tree index `(eq…, sort…, pk)` | GSI: partition key = `eq` (concatenated if more than one), sort key = `order_by` (+ pk) |
| `range` | predicate on the leading sort column | key condition on the sort key |
| `CONSISTENCY_STRONG` | writer pool | `ConsistentRead` (base table only, so a lint error on GSI paths) |
| unique non-key column | unique index | not supported natively, so a lint error |
| `ROLE_VERSION` CAS | `WHERE version = $n` | `ConditionExpression` |
| `ROLE_DELETE_TIME` | partial indexes + filter | sparse GSI or `FilterExpression` (lint warning: consumes read capacity) |
| upsert | `INSERT … ON CONFLICT` | `PutItem` / `UpdateItem` |
| page token | encoded keyset tuple | `LastEvaluatedKey`, passed through |
| lifecycle states | columns + migrations | attributes; no DDL, and GSI changes belong to IaC |
| transactions | `WithTx` on `pgx.Tx` | `TransactWriteItems`; a different model, not in the core interface |

The core model maps cleanly. The same ESR rule (range on the leading sort
column) holds in both backends. The DynamoDB-specific limits (single sort
attribute, no non-key uniqueness, no strongly consistent GSI reads) would be
enforced as that backend's own lint rules, DAL4xx.

## 4. IDL reference

Core options (`dal.v1`):

| Option | On | Purpose |
|---|---|---|
| `(dal.v1.table)` | message | entity name, shard key |
| `(dal.v1.field)` | field | format, primary key, unique, role, lifecycle state |
| `(dal.v1.store)` | service | binds the service to its entity message |
| `(dal.v1.query)` | rpc | exactly one of `get`, `list`, `create`, `update`, `delete`, `upsert` |

Postgres options (`dal.pg.v1`), all optional:

| Option | On | Purpose |
|---|---|---|
| `(dal.pg.v1.table)` | message | explicit (partial/covering) indexes, index budget |
| `(dal.pg.v1.column)` | field | physical type or `custom_type`, SQL default |

The defaults are meant to be the performant choice. Options only exist to
configure away from them.

```proto
message Order {
  option (dal.v1.table) = {name: "orders" shard_key: ["account_id"]};
  option (dal.pg.v1.table) = {index_budget: 4};

  string id = 1 [(dal.v1.field) = {format: FORMAT_UUID primary_key: true}];
  string account_id = 2 [(dal.v1.field).format = FORMAT_UUID];
  string status = 3 [(dal.pg.v1.column).default = "'pending'"];
  optional string note = 4;
  int64 version = 5 [(dal.v1.field).role = ROLE_VERSION];
  google.protobuf.Timestamp created_at = 6 [(dal.v1.field).role = ROLE_CREATE_TIME];
  google.protobuf.Timestamp updated_at = 7 [(dal.v1.field).role = ROLE_UPDATE_TIME];
  optional google.protobuf.Timestamp deleted_at = 8 [(dal.v1.field).role = ROLE_DELETE_TIME];
}

service OrderStore {
  option (dal.v1.store) = {entity: "Order"};

  rpc GetById(GetByIdRequest) returns (Order) {
    option (dal.v1.query) = {get: {}};
  }
  rpc ListByAccount(ListByAccountRequest) returns (ListOrdersResponse) {
    option (dal.v1.query) = {list: {eq: ["account_id"] order_by: ["created_at DESC"]}};
  }
  rpc Create(Order) returns (Order) { option (dal.v1.query) = {create: {}}; }
  rpc UpdateStatus(Order) returns (Order) {
    option (dal.v1.query) = {update: {columns: ["status", "note"]}};
  }
  rpc Delete(GetByIdRequest) returns (Order) { option (dal.v1.query) = {delete: {}}; }
}
```

The complete, compiling version is in
[`internal/idl/testdata/orders/v1/orders.proto`](../internal/idl/testdata/orders/v1/orders.proto).

**Request and response messages are checked, not trusted.** Filter fields in a
request (`account_id`) must match `eq`/`by` columns by name and type. List
requests carry `page_size` and `page_token`, and List responses carry one
repeated entity field plus `next_page_token`.

### Fields

- **Nullability comes from proto3 presence.** `optional` fields are nullable;
  every other field is required (`NOT NULL`).
- **Primary key:** set `primary_key: true` on one or more fields. A composite
  key follows declaration order.
- **Shard key:** `(dal.v1.table).shard_key` lists the columns the data is
  sharded by. v1 only checks that they exist and are active.
- **Roles** mark columns the generated code manages:

| Role | Behaviour |
|---|---|
| `ROLE_CREATE_TIME` | set on insert, never updated |
| `ROLE_UPDATE_TIME` | set on insert and on every update |
| `ROLE_DELETE_TIME` | makes `Delete` a soft delete; reads exclude deleted rows (pg: derived indexes become partial `WHERE deleted_at IS NULL`) |
| `ROLE_VERSION` | `Update` compare-and-swaps on it and increments it; a mismatch returns `dal.ErrVersionConflict` |

### Type mapping (Postgres)

The Postgres type comes from the proto type and `format`, unless
`(dal.pg.v1.column).type` or `custom_type` overrides it. The Go domain type
comes from the core layer and is the same for every backend.

| Proto (+ format) | Postgres default | Go domain type |
|---|---|---|
| `string` | `text` | `string` |
| `string` + `FORMAT_UUID` | `uuid` | `uuid.UUID` |
| `string` + `FORMAT_DECIMAL` | `numeric` | `string` (decimal library TBD) |
| `string` + `FORMAT_JSON` | `jsonb` | `json.RawMessage` |
| `bool` | `boolean` | `bool` |
| `int32`, `sint32`, `sfixed32` | `integer` | `int32` |
| `int64`, `sint64`, `sfixed64`, `uint32` | `bigint` | `int64` |
| `uint64` | lint error; requires `custom_type` | — |
| `float` / `double` | `real` / `double precision` | `float32` / `float64` |
| `bytes` | `bytea` | `[]byte` |
| `google.protobuf.Timestamp` | `timestamptz` | `time.Time` |
| enum | `text` (value name) | a generated string type |
| repeated scalar | `<type>[]` | `[]T` |
| message, map, `google.protobuf.Struct` | `jsonb` | `json.RawMessage` |

`optional` fields become pointers (`*T`) in the domain struct. Enums are stored
by name, which is readable and survives renumbering. There's no `CHECK`
constraint, because adding a value would then need a migration before the code
could use it. `custom_type` leaves room for extension types such as
`vector(1536)` or PostGIS types, without first-class support in v1.

## 5. Access paths and index derivation

Physical structures are **query-first**: they are derived from the access
patterns that need them, not declared up front. A List resolves to an access
path of three parts:

- **E**quality filters (`eq`)
- **S**ort (`order_by`)
- **R**ange (`range`, which must be the leading sort column)

The primary key is appended as a tie-breaker. That makes the order total,
which keyset pagination requires.

For Postgres, an access path becomes the B-tree index `(eq..., sort..., pk...)`:

```
list: {eq: ["account_id"] order_by: ["created_at DESC"]}
  → CREATE INDEX orders_account_id_created_at_id_idx
      ON orders (account_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;
```

### Where the sort order comes from

Sort order isn't inferred from field types. For example, there's no "first
timestamp wins" rule. It resolves in this order:

1. **Explicit `order_by` on the rpc.** It's used as written, and the physical
   structure is derived from it.
2. **A backend-declared index.** If the rpc gives `eq` but no `order_by`, and
   an explicit index has leading columns exactly equal to the `eq` set (in any
   order), that index's remaining columns become the sort. For Postgres that's
   a `(dal.pg.v1.table).indexes` entry. Use this when several List rpcs share
   one index.
3. **The primary key.** It's deterministic, and it produces lint `DAL103` if the
   key isn't time-ordered, e.g. a random UUIDv4 rather than UUIDv7 or a
   sequence. The results are stable but meaningless to users.

### Get

`get` looks up by primary key by default. `by: [...]` looks up by other
columns, which must be covered by a `unique` column or a unique index,
otherwise it's lint error `DAL104`. A non-unique lookup is a List.

### Postgres index merging and budget

- **Prefix merging:** derived indexes are deduplicated when one is a prefix of
  another with compatible directions. For example, `(account_id)` is served by
  `(account_id, created_at DESC, id DESC)`.
- **Equality order:** equality columns are reordered to maximise sharing, since
  their order inside the equality prefix doesn't affect correctness.
- **Budget:** each table has an index budget (project default 5, overridable
  with `(dal.pg.v1.table).index_budget`), counting the primary key and unique
  indexes. Going over it produces lint warning `DAL201`, because every index
  adds write amplification and insert latency.

### Lint rules (initial set)

Core (every backend):

| ID | Severity | Rule |
|---|---|---|
| DAL101 | error | `range` column isn't the leading `order_by` column |
| DAL103 | warn | List falls back to primary-key order and the key isn't time-ordered |
| DAL104 | error | `get.by` / `upsert.conflict_on` isn't covered by a unique constraint |
| DAL107 | error | request or response message doesn't match the declared pattern |
| DAL109 | warn | an `order_by` column can be changed by an `update`/`upsert`, so rows can move between pages mid-iteration |
| DAL110 | error | `shard_key` names a missing or deprecated column |
| DAL111–113 | error | field-number identity violations; see §8 |

Postgres:

| ID | Severity | Rule |
|---|---|---|
| DAL201 | warn | index budget exceeded |
| DAL202 | error | `order_by` contradicts an explicit index whose leading columns equal `eq` |
| DAL203 | warn | mixed sort directions; the keyset predicate can't use a row comparison |
| DAL204 | error | table or column name is a SQL reserved word; set `name` |
| DAL205 | error | name collision between a generated query and a custom sqlc query |
| DAL3xx | error | migration safety; see §8 |

DAL4xx is reserved for a future DynamoDB backend.

## 6. Pagination and page tokens

Every List is keyset-paginated. `OFFSET` is never generated.

### Contract (core, every backend)

- **Token lifecycle:** the page token is an opaque string. An empty
  `page_token` asks for the first page. An empty `next_page_token` means there
  are no more pages. The repository never returns a non-empty token for an
  empty page.
- **Clients:** hand `next_page_token` back unchanged. It's safe to send to API
  clients and receive back from them, so services can pass it straight through
  their own List endpoints.
- **Encoding:** tokens are base64url (no padding) of a small versioned JSON
  envelope. They're **not signed or encrypted**, so a client can read the sort
  values of the last row it received. Those are values from a row the client
  has already been shown.
- **Binding:** a token is bound to its query. The envelope carries a hash of
  the query shape (entity, access path, sort) and of the filter values (`eq`
  and `range` arguments). Reusing a token with a different query or different
  filters returns `dal.ErrInvalidPageToken` rather than wrong pages. This is
  about correctness, not security.
- **Page size:** `page_size` may change between calls. It's clamped to
  `max_page_size`, falling back to `default_page_size`.
- **Consistency under concurrent writes:** there are no duplicates and no
  skips for rows whose sort columns don't change. Rows inserted behind the
  cursor aren't seen; rows inserted ahead of it are. If a sort column is
  mutable, rows can move between pages, hence lint `DAL109`.

Envelope: `{"v":1,"q":"<shape hash>","f":"<filter hash>","k":<backend payload>}`.
The core `dal` package owns the envelope, the hashing and the base64 encoding.
Each backend only encodes and decodes `k`.

### Postgres payload

`k` is the last row's sort tuple plus its primary key. For
`order_by: ["created_at DESC"]`:

```sql
-- name: ListOrdersByAccount :many
SELECT ... FROM orders
WHERE account_id = @account_id
  AND deleted_at IS NULL
  AND (sqlc.narg(after_created_at)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(after_created_at), sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT @page_limit;
```

- The repository fetches `page_size + 1` rows. The extra row only signals
  that another page exists; the token is built from the last row actually
  returned.
- Timestamps are encoded at full microsecond precision, so the cursor
  comparison is exact.
- A row comparison (`(a, b) < (x, y)`) only works when every sort column has
  the same direction. Mixed directions fall back to the expanded `OR` form and
  produce lint warning `DAL203`.

A DynamoDB backend would put `LastEvaluatedKey` in `k` and pass it straight
through as `ExclusiveStartKey`.

### Iteration in Go

Pages are the primitive, because they're what crosses an API boundary.
`dal.All` adapts any List method into a Go 1.23 iterator for in-process use:

```go
for o, err := range dal.All(ctx, orders.ListByAccountParams{AccountID: id}, repo.ListByAccount) {
	if err != nil {
		return err
	}
	// ...
}
```

## 7. Generated Go API

The core layer generates one package per proto package. It holds plain domain
structs and backend-neutral repository interfaces. Each backend generates an
implementation package next to it (`orderspg` today). Proto messages are never
used at runtime.

```go
// package orders: core, backend-neutral
type Order struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	Status    string
	Note      *string
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type OrderReadRepository interface {
	GetByID(ctx context.Context, id uuid.UUID) (Order, error)
	ListByAccount(ctx context.Context, p ListByAccountParams) (dal.Page[Order], error)
}

type OrderWriteRepository interface {
	Create(ctx context.Context, o Order) (Order, error)
	UpdateStatus(ctx context.Context, o Order) (Order, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

type OrderRepository interface {
	OrderReadRepository
	OrderWriteRepository
}
```

```go
// package orderspg: Postgres implementation
type Repository struct{ /* ... */ }

var _ orders.OrderRepository = (*Repository)(nil)

func NewRepository(db dalpg.DB) *Repository

// WithTx runs fn in a transaction on the writer pool. It's Postgres-only, so it
// isn't on the core interface.
func (r *Repository) WithTx(ctx context.Context, fn func(tx *Tx) error) error
```

- **Services depend on the core interfaces** (`orders.OrderReadRepository`
  etc.), and only the composition root knows about `orderspg`. Switching
  backends means changing the constructor.
- **Postgres routing:** `dalpg.DB` holds a `Reader` and a `Writer`
  `*pgxpool.Pool`. Eventual reads go to the reader. `CONSISTENCY_STRONG` reads
  and all writes go to the writer. A single-instance setup passes the same pool
  twice.
- **Errors and retries:** see below.
- **Transactions:** `orderspg.Tx` exposes the entity's read and write methods
  bound to the transaction, plus `Queries()` for custom sqlc queries in the
  same transaction. They're in v1 if they stay this thin (sqlc's
  `Queries.WithTx(pgx.Tx)` does most of the work), and are cut otherwise.
- **Runtime libraries** live in this module and are imported, not generated,
  so bug fixes don't need a regeneration:
  - `dal`: `Page[T]`, `All`, the page-token envelope, sentinel errors,
    `Retrier`, `Op`, `Retryability`
  - `dal/dalpg`: `DB`, the keyset payload codec, the SQLSTATE classifier and
    error mapping

### Errors and retries

**Errors** are core sentinels, and each backend maps its own errors onto them:

| Sentinel | Postgres source |
|---|---|
| `dal.ErrNotFound` | `pgx.ErrNoRows` |
| `dal.ErrVersionConflict` | a CAS update that matched no row |
| `dal.ErrAlreadyExists` | `23505` unique violation |
| `dal.ErrInvalidPageToken` | a page token that fails to decode or doesn't match the query |

Every error the repository returns is a `*dal.Error`. It carries the `Op`, the
backend code (the SQLSTATE, for Postgres), a `Retryability`, and the
underlying cause. `errors.Is` still matches the sentinels.

**Retryability is decided in two steps.** First, the backend classifies the
error. Then the retry policy combines that class with whether the operation is
idempotent.

```go
// package dal
type Retryability int

const (
	NotRetryable          Retryability = iota
	RetryableIfIdempotent // transient, but a write may already have been applied
	Retryable             // the server did nothing or rolled back: safe for any op
)

type Op struct {
	Entity     string // "Order"
	Method     string // "ListByAccount"
	Kind       OpKind // Get, List, Create, Update, Delete, Upsert, Tx
	Idempotent bool
}

// ShouldRetry reports whether err is safe to retry for op.
func ShouldRetry(op Op, err error) bool

type Retrier interface {
	Do(ctx context.Context, op Op, fn func(ctx context.Context) error) error
}

// RetrierFunc lets a closure act as a Retrier.
type RetrierFunc func(ctx context.Context, op Op, fn func(ctx context.Context) error) error
```

**Postgres classification** (`dalpg.Classify`):

| Condition | SQLSTATE | Class | Why |
|---|---|---|---|
| `pgconn.SafeToRetry(err)` | — | Retryable | the request never reached the server |
| serialization failure, deadlock | `40001`, `40P01` | Retryable | the server rolled the transaction back |
| lock not available (`lock_timeout`) | `55P03` | Retryable | the statement failed, so nothing was applied |
| too many connections, cannot connect now | `53300`, `57P03` | Retryable | rejected before anything ran |
| read-only transaction | `25006` | Retryable | Aurora failover: the writer endpoint briefly points at a reader |
| admin/crash shutdown, connection lost mid-request | `57P01`, `57P02`, `08xxx` | RetryableIfIdempotent | a commit may or may not have happened |
| query canceled / statement timeout | `57014` | NotRetryable | the caller's deadline or the server's timeout governs |
| context canceled or deadline exceeded | — | NotRetryable | the caller gave up |
| everything else (e.g. `23xxx`, `22xxx`, `42xxx`) | — | NotRetryable | retrying won't change the outcome |

**Which generated operations count as idempotent** (the value is fixed at
generation time and passed in `Op`):

| Operation | Idempotent | Notes |
|---|---|---|
| Get, List | yes | |
| Upsert | yes | |
| Delete | yes | a retry after an ambiguous success returns `ErrNotFound` |
| Update without `ROLE_VERSION` | yes | it sets absolute values |
| Update with `ROLE_VERSION` | no | a retry after an ambiguous success would report the caller's own write as `ErrVersionConflict` |
| Create | no | the repository assigns a client-side UUIDv7 before the first attempt, so a custom policy that retries anyway gets `ErrAlreadyExists` rather than a duplicate row |
| `WithTx` | no by default | the transaction is retried as a whole, never statement by statement; opt in when `fn` has no side effects outside the transaction |

**Everything is pluggable, following the open-closed principle.** The
built-in SQLSTATE table is a default, not a contract. Deployment topology
changes which errors are transient. For example, RDS Proxy adds its own
connection-borrow timeouts and failover behaviour in front of Aurora. Callers
extend the classification or the policy for their topology without changing
DALForge:

- **`dalpg.WithRetrier(dal.Retrier)`** replaces the policy entirely. It can be
  a `dal.RetrierFunc` closure, or an adapter over a library such as
  [failsafe-go](https://github.com/failsafe-go/failsafe-go), a Go port of Java's
  Failsafe with retry policies, backoff, circuit breakers and bulkheads.
  `dal.ShouldRetry` works as its retry predicate. The adapter lives in
  `examples/`, so the runtime doesn't take on the dependency.
- **`dalpg.WithClassifier(func(err error, base dal.Retryability) dal.Retryability)`**
  adjusts the SQLSTATE mapping without replacing the policy. For example, you
  could treat `57014` as retryable.
- **The default is `dal.DefaultRetrier`:** at most 3 attempts, exponential
  backoff with full jitter (25 ms base, 1 s cap), following `ShouldRetry` and
  respecting the context. `dal.NoRetry` turns retries off.

The generated repositories wrap each method in `retrier.Do`. Inside `WithTx`,
individual statements are never retried: a `40001` aborts the whole
transaction, so only the outer `WithTx` can be retried.

## 8. Migrations and n+1 compatibility

In a rolling deploy, the migration runs first. Then release N and N+1
instances share the schema. So **every migration has to be compatible with the
code already running (N) and with the code about to run (N+1)**. That forces
expand/contract changes, which DALForge enforces.

**The lifecycle model is core; the migrations are backend-specific.** Postgres
emits SQL. A DynamoDB backend would emit none for attributes, since they're
schemaless, and would leave GSI changes to infrastructure-as-code.

### Snapshot and diff

- **Snapshot:** `dalforge.snapshot.json` is committed alongside the IDL. It
  records tables, columns (keyed by proto field number, with name, type,
  nullability, default, lifecycle state and the migration that set it),
  reserved numbers, and indexes.
- **Generating a migration:** `dalforge migrate -name <slug>` diffs the IDL
  against the snapshot. It writes files in
  [golang-migrate](https://github.com/golang-migrate/migrate) format, then
  updates the snapshot. golang-migrate was picked because it's the most widely
  used Go migration tool, it has a pgx v5 driver, and Atlas can emit the same
  format.
- **Linting:** `dalforge lint` runs the same diff read-only and fails if the IDL
  and snapshot disagree without a migration, which makes it suitable for CI.
- **Release model:** each migration is assumed to be one release. Rules that
  say "a later release" mean "a later migration file".

**golang-migrate caveat:** it runs a whole file in a single exec, and Postgres
treats a multi-statement exec as one implicit transaction. `CREATE/DROP INDEX
CONCURRENTLY` can't run inside a transaction, so:

- each concurrent index operation gets its own single-statement migration;
- every other migration file starts with `SET lock_timeout = '5s';`;
- for the concurrent-index files, set `lock_timeout` through the migration
  connection string (`options=-c lock_timeout=5s`).

### Field identity (core)

**A field's proto number is the column's identity**, and it's the first line of
defence against incompatible changes. The snapshot keys every column by entity
and field number, so the diff works on numbers rather than guessing from names:

| ID | Severity | Rule |
|---|---|---|
| DAL111 | error | a field number is reused for a different column, or one previously dropped (it should have been `reserved`) |
| DAL112 | error | a field is removed without its number *and* name being `reserved` |
| DAL113 | error | an existing column name moves to a different field number (renumbering) |

Because of this, renames are detected exactly rather than heuristically: the
same number with a new name is a rename (`DAL306`).

These rules are backend-neutral and come from the snapshot diff, so buf isn't
needed for them. DALForge's own extension numbers (`51000`, `51010`) are
protected by a unit test that pins them.

### Field lifecycle (core)

1. **Active.** This is the default.
2. **`state: STATE_DEPRECATED`.** The generated code stops reading and writing
   the column. It stays in storage. If the column is `NOT NULL` with no
   default, the migration drops `NOT NULL` so N+1 inserts succeed while N still
   reads the column.
3. **Delete the field and `reserved` its number.** This emits
   `ALTER TABLE ... DROP COLUMN`, but only if the snapshot shows the column was
   deprecated in an *earlier* migration. Otherwise it's error `DAL301`.

A rename is expressed as adding the new column under a new field number and
deprecating the old one. Backfill and dual-write are left to the application.
Renaming in place (the same number with a new name) is error `DAL306`.

### Migration safety rules (Postgres)

| ID | Rule |
|---|---|
| DAL301 | drop column without a prior DEPRECATED release |
| DAL302 | new `NOT NULL` column without a constant default. Old code's inserts would fail. Volatile defaults would rewrite the table |
| DAL303 | column type change. Needs a table rewrite and breaks N; use add + deprecate |
| DAL304 | tighten to `NOT NULL` on an existing column. Emitted as a `CHECK ... NOT VALID` + `VALIDATE` sequence, never a direct `SET NOT NULL` |
| DAL305 | adding a unique constraint. Emitted as `CREATE UNIQUE INDEX CONCURRENTLY`, then `ADD CONSTRAINT ... USING INDEX` |
| DAL306 | rename in place (same field number, new name). Old code still uses the old column name; use add + deprecate |

**Future option: Atlas.** Atlas could replace the diff engine, via declarative
`schema.sql` plus its lint analyzers, while still emitting golang-migrate
files. The snapshot and lifecycle states would still come from DALForge,
because Atlas has no notion of a field's release history.

## 9. Output layout

What a consuming repo looks like (paths are configurable in `dalforge.yaml`):

```
dalforge.yaml               # proto roots, backend, output dirs, index budget, page-size defaults
proto/orders/v1/orders.proto
dalforge.snapshot.json      # committed; source of truth for migration diffs
migrations/                 # generated golang-migrate files, append-only, reviewed
schema/schema.sql           # generated desired state (also the sqlc schema input)
queries/generated/*.sql     # generated; "Code generated by dalforge. DO NOT EDIT."
queries/custom/*.sql        # handwritten sqlc queries, never touched by dalforge
sqlc.yaml                   # generated
gen/sqlcdb/                 # sqlc output (both query sets)
gen/orders/v1/orders/       # core: domain structs + repository interfaces
gen/orders/v1/orderspg/     # pg backend: repository implementation
```

Generated and custom queries go through **one** sqlc config, so they share a
`Queries` type and custom queries can run inside the same transactions.

The generated `sqlc.yaml` turns on sqlc's safety features:

- `sql_package: pgx/v5`, `emit_interface`, `emit_pointers_for_null_types`
- `strict_function_checks`, `strict_order_by`
- type overrides: `uuid` → `github.com/google/uuid.UUID`, `timestamptz` →
  `time.Time`
- **with a database URI configured**, `sqlc vet` with `sqlc/db-prepare`, plus
  opt-in CEL rules: reject `OFFSET` in custom queries, and flag sequential
  scans with `postgresql.explain`.

## 10. Testing

| Suite | Where | Runs with | Needs |
|---|---|---|---|
| Unit | `*_test.go` next to the code | `mise run test` (part of `mise run check`) | nothing |
| Golden | unit tests comparing emitter output with `testdata/*.golden`; `-update` rewrites them | `mise run test` | nothing |
| Integration | `//go:build integration` files next to the code they cover, plus end-to-end suites in `internal/integration/` | `mise run test:integration` (runs `vm:up` and `db:up` first) | nothing beyond mise (Colima locally, native Docker in CI) |
| Examples | `examples/orders` regenerated and diffed | `mise run examples` (part of `mise run check`) | nothing |

**Local Postgres.** `compose.yaml` runs `postgres:16.9`, the community
release matching the Aurora PostgreSQL 16 LTS target, on port `55432` with
tmpfs storage and durability turned off:

- `mise run db:up` / `db:down` / `db:psql` manage it.
- **No Docker Desktop required.** mise pins `colima`, `lima`, `docker-cli`
  and `docker-compose`:
  - `vm:up` starts a dedicated Colima profile, `dalforge` (Apple
    Virtualization, 2 CPU / 2 GB). It's a no-op if the VM is already running.
    `vm:down` frees the memory.
  - `mise.toml` points `DOCKER_HOST` at that profile's socket only inside this
    repo, and Colima runs with `--activate=false`, so the global Docker
    context isn't touched.
  - In CI (`CI` set), `vm:up` is skipped and the runner's native Docker is
    used.
  - Measured: about 75 s for the first run (VM boot plus image pull), about
    40 s on a cold VM, about 4 s warm.
- `mise.toml` exports `DALFORGE_TEST_DATABASE_URL`.
- `internal/pgtest.New(t)` creates a throwaway database per test and drops it
  afterwards, so tests can run in parallel.
- `TestPostgresVersion` fails if the server isn't on major version 16.

**Why containers rather than embedded Postgres:** DALForge will go beyond
Postgres. DynamoDB Local, LocalStack, and images with extensions such as
pgvector are containers, and Toxiproxy (for injecting connection drops and
latency to test the retry classifier) fits the same compose file. Embedded
Postgres would be a Postgres-only path.

The same database backs the runnable example. Aurora-specific behaviour, such
as failover, isn't reproduced locally. The retry paths are tested by injecting
the conditions (below).

**What the end-to-end integration suites do:** compile IDL fixtures, run the
generator and sqlc, build the generated code, and run it against a real
Postgres 16. They cover:

- CRUD by key;
- keyset pagination under concurrent inserts (no duplicates or skips);
- CAS conflicts and soft delete;
- page-token rejection;
- retries: `40001` from conflicting serializable transactions, and a
  connection killed with `pg_terminate_backend` (`57P01`), checking the
  idempotent vs non-idempotent behaviour;
- the phase 3 rolling-deploy scenarios: schema N+1 run against the code for N
  and for N+1.

**Task split:** `mise run check` stays fast and Docker-free. `mise run
check:all` adds the integration suite, and it's what CI runs.

## 11. Examples

`examples/orders/` is a standalone Go module that consumes DALForge the way a
user would: IDL, `dalforge.yaml`, custom queries, and checked-in generated
output.

Its README is the entry point for newcomers. It walks through:

1. **The problem:** a free-form query next to the index it silently needs.
2. **The IDL:** the same access pattern declared once.
3. **What gets generated:** the schema and index, the SQL, and the Go API.
4. **Lint:** the linter rejecting bad patterns, using the fixtures in
   `examples/orders/bad/`.

A runnable demo exercises CRUD and pagination against Postgres 16.
`mise run examples` regenerates everything and fails on a diff, so the
checked-in output can't go stale. The example grows with each phase: CRUD in
phase 1, List and lint in phase 2, and a migration walkthrough in phase 3.

## 12. Roadmap

1. **CRUD by key:** IR loader, core/backend split, type mapping, `schema.sql`,
   Get/Create/Update/Delete queries, `sqlc.yaml`, domain structs, core
   interfaces and the pg implementation, the `dal` and `dal/dalpg` runtimes.
   Error classification and the pluggable retrier. Integration tests against
   the compose Postgres 16.9. First cut of
   `examples/orders`.
2. **List and indexes:** access paths, sort resolution, keyset List with page
   tokens and `dal.All`, index derivation and merging, the DAL1xx/DAL2xx lint
   rules. Upsert, soft delete, optimistic locking, and `WithTx` if it stays
   thin.
3. **Migrations:** snapshot, diff, lifecycle states, DAL3xx rules,
   golang-migrate file emission.
4. **Extensions:** `custom_type` ergonomics (pgvector, PostGIS), buf plugin
   mode, the Atlas backend, sqlc vet rule packs, shard-key enforcement, and a
   DynamoDB backend.

## Decision log

| Topic | Decision |
|---|---|
| Generator packaging | Standalone CLI embedding protocompile; buf plugin later |
| Domain types | Generated plain Go structs; proto only as IDL |
| Index model | Query-first; explicit indexes only for partial/covering/sort-pinning |
| Sort with no `order_by` | Inherit from matching explicit index, else PK + DAL103 |
| Migrations | Snapshot diff → golang-migrate SQL; Atlas as a future backend |
| Lifecycle | `STATE_DEPRECATED` for one release before a field can be dropped |
| Foreign keys | Non-goal (sharding) |
| Sharding | `shard_key` declared and validated; routing and enforcement out of scope |
| Page tokens | In scope. Base64url JSON envelope, unsigned, bound to query + filters |
| Batch operations | Out of scope |
| Backend layering | Core IDL/IR/interfaces are backend-neutral; Postgres is the reference backend; DynamoDB mapping validated on paper only |
| Platform | Aurora PostgreSQL 16.x, pgx v5, Go 1.26+ |
| Options Go bindings | protoc-gen-go driven by protoc, both pinned via mise; no buf. Field-number safety comes from DALForge's own snapshot rules (DAL111–113) |
| Testing | Unit + golden in `mise run check`; `integration` build tag against a docker compose `postgres:16.9` in `mise run test:integration`; `check:all` for CI |
| Container runtime | Colima via mise (dedicated `dalforge` profile, repo-scoped `DOCKER_HOST`); native Docker in CI. Chosen over embedded Postgres so that future backends and fault-injection proxies share one mechanism |
| Retries | SQLSTATE classification into Retryable / RetryableIfIdempotent / NotRetryable, plus per-op idempotency; pluggable `dal.Retrier` (closure-friendly) and classifier; built-in default; failsafe-go adapter shown in examples |
| Onboarding | `examples/orders` standalone consumer module with a README walkthrough, kept fresh by `mise run examples` |
