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
- `ReadRepository` / `WriteRepository` / `Repository` interfaces per
  entity, with reads and writes routed to reader and writer pools. Their entity
  types are sqlc's model structs, re-exported as type aliases: no copy layer,
  and no driver (pgx) types anywhere in the API;
- basic footgun-free writes: version compare-and-swap on update, and upsert on
  the primary key or a unique field;
- versioned migrations, linted for n+1 rolling-deploy compatibility.

Handwritten sqlc queries remain a first-class escape hatch alongside the
generated ones. They live in a user-owned directory (`queries/custom/`) that
dalforge never touches; generated files say `DO NOT EDIT`.

**The IDL, IR and lint are backend-neutral; generated code isn't.** The backend
is chosen at declaration time (the IDL's backend options and `dalforge.yaml`),
and the generated code is written for that backend. A second backend
(DynamoDB) would reuse the IDL model and bring its own generated code. Only
Postgres is built for now (§3).

**Non-goals**
- Complex queries (joins, aggregates, CTEs). Write those as custom sqlc
  queries.
- Foreign keys, and generated referential-integrity checks. FKs don't work
  well with sharded tables. Application-level integrity is shown as a pattern
  in `examples/` (§11) rather than generated.
- Shard routing. A `shard_key` can be declared and is validated, but nothing
  routes or enforces by it yet.
- Batch operations (`CreateMany`, `GetMany`).
- Conditional writes beyond the version compare-and-swap (state-transition
  guards and the like), and upsert variants beyond PK/unique conflict targets.
  Those are custom sqlc queries.
- A domain model separate from sqlc's models. The DAL doesn't copy or map
  rows into a second set of structs; that would be an ORM-style translation
  layer to maintain.
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
                               │   sqlc.yaml, DAL package,          │
                               │   migrations                       │
                               └────────────────────────────────────┘
```

- **A standalone CLI** (`dalforge generate | lint | migrate | lock`) that embeds
  `github.com/bufbuild/protocompile`, so neither buf nor protoc is needed at
  generation time. Being a CLI rather than a protoc plugin lets it read and
  write state on disk, namely the schema snapshot that migration diffing
  depends on. A buf plugin mode can be added later over the same core.
- **The options ship inside the binary**, so users can
  `import "dal/v1/options.proto"` without vendoring it. The resolver
  (`internal/idl.NewResolver`) serves these imports from the compiled
  descriptors in the Go bindings rather than from embedded source, so there's
  no second copy to drift. Lookup order:
  1. the bundled options, which always win, so a stale vendored copy on an
     import path is never compiled in (DAL116 reports such copies);
  2. the user's import paths;
  3. the protobuf well-known types.

  protocompile decodes custom option values as `dynamicpb` messages, so the
  loader re-decodes options against the registered Go types to get typed
  `*dalv1.Table` and friends. If a command later needs to export the protos
  for editors or buf, that's when embedding the source becomes worthwhile. Their Go bindings sit
  next to them (`github.com/gisripa/dalforge/proto/dal/v1`, package `dalv1`;
  `…/proto/dal/pg/v1`, package `pgv1`). Users who run protoc-gen-go on their
  own IDL can import them, so they're public, not `internal/`. They can also be
  published to the BSR for editor and `buf lint` support.
- **sqlc is pinned in the consuming repo's `mise.toml`** (`sqlc = "1.30.0"`,
  a prebuilt binary from mise's registry), the same way dalforge itself is
  installed. dalforge runs `sqlc` from `PATH` and checks `sqlc version`
  against `dalforge.lock`. This replaces the earlier `go tool sqlc` plan:
  sqlc's Postgres parser (`pg_query_go`) uses cgo, so `go tool` would compile
  C code on first use, which is slow and needs a C compiler.
- **The core IR is the contract between stages.** Lint rules and emitters only
  see the IR, never the descriptors. That makes both testable with plain Go
  values.
- **Protobuf stops at the loader.** protocompile, the `dalv1`/`pgv1`
  bindings and option decoding are used only to read the IDL. The IR is plain
  Go, and so is everything generated from it. The generated code imports the
  standard library, pgx, uuid, the sqlc output and the `dal` runtime, never
  `google.golang.org/protobuf`. A `go list -deps` test enforces this.

### The IR

`internal/idl.Load` compiles the IDL and builds the IR. It's the only stage
that touches protobuf. Everything after it reads only:

- **`internal/ir`, the core:** `Schema` → `Entity` (table, fields, shard key,
  reserved numbers and names) and `Store` → `Query`. A query's `Spec` is
  sealed: `Get | List | Create | Update | Delete | Upsert`.
- **`internal/ir/pgir`, Postgres hints:** file `min_version`/`extensions`,
  explicit indexes, the index budget, and per-column type, default and Go
  type. They're keyed by entity full name and field number, so the core
  never references backend types. A DynamoDB backend would add `ddbir` the
  same way.

**Rules:**
- **Defaults are applied in the loader and marked.** Core defaults are filled
  in, and each defaulted value carries a `Source` (`declared`, `defaulted`,
  `inherited`) so lint can tell an explicit choice from a fallback:

  | Value | Default |
  |---|---|
  | table name | snake_case of the message name |
  | `Get.By`, `Upsert.ConflictOn` | the primary key |
  | `List.OrderBy` | the range column, otherwise the primary key minus the `eq` columns (the PK fallback is `defaulted` with an empty `range`) |
  | `Update.Columns`, `Upsert.Columns` | active columns that are neither key columns nor role columns |

  Defaults that need backend knowledge (a sort inherited from an explicit pg
  index) or config (page sizes) are filled in by later passes.
- **References are proto field names.** Every reference in the IDL (`eq`,
  `range`, `order_by`, `by`, `columns`, `conflict_on`, `shard_key`, pg index
  columns) uses the proto field name, the user's source of truth, which is
  also what request messages and generated Go code use. A field's `Column`
  (`(dal.v1.field).name`) is purely its physical SQL name. References are
  strings resolved with `Entity.Field`. Validation (`internal/check`)
  guarantees they resolve before any emitter runs, so a typo becomes a
  positioned lint error, not a load failure. Writing a column name by mistake
  gets a hint naming the right field.
- **Renames and rolling deploys.** Column identity is the field *number*.
  Changing a column name, whether via `name` or by renaming a field whose
  column defaults to its name, is a rename in place (DAL306). Renaming the
  proto field while pinning `name` to the old column is safe: only the Go and
  IDL name changes, with no migration. DAL306 therefore compares the
  snapshot's column names, not proto names. Until phase 3's snapshot exists
  this isn't guarded, but phase 1 has no migrations to break either.
- **Errors are structural and reported together.** The loader rejects:
  - non-proto3 files
  - an entity without a primary key
  - `oneof` fields in entities
  - unsupported well-known types (only `Timestamp`, `Struct`, `Value` and
    `ListValue` are accepted)
  - a store without an entity, or naming one that doesn't resolve
  - an rpc without a query, or with an empty one
  - an invalid sort
  - streaming rpcs

  Every error is reported in one run, each with `file:line:col`.
- **Nullability** comes from the proto3 `optional` keyword
  (`HasOptionalKeyword`), not field presence, which is always true for message
  fields.
- **Entities** are messages with `(dal.v1.table)` plus any message a store
  names, which may live in an imported file.
- **Golden IR fixtures** live in `internal/idl/testdata/load/*.json.golden`.



Two checked-in files hold state, each with one job:

| File | Like | Records |
|---|---|---|
| `dalforge.snapshot.json` | Terraform state | what the schema looks like (for migrations, §8) |
| `dalforge.lock` | `.terraform.lock.hcl` | which toolchain produced the generated code |

```toml
# dalforge.lock: maintained by dalforge, do not edit. Commit it.
lock_version = 1
dalforge     = "0.3.1"

[options]   # hashes of the bundled options protos used for generation
"dal/v1/options.proto"    = "sha256:9f2c…"
"dal/pg/v1/options.proto" = "sha256:41ab…"

[tools]
sqlc = "1.30.0"
```

**Behaviour:**
- **Creation:** `dalforge generate` writes the lock if it's missing.
- **Mismatch:** if the running binary's version or bundled options hashes
  differ from the lock, `generate`, `lint` and `migrate` fail. `dalforge lock
  -upgrade` is the explicit way forward, and the lock change shows up in
  review. A teammate or CI job with a different dalforge can't silently
  regenerate different code.
- **Vendored copies:** copies of the options protos kept for editor or buf
  tooling are checked against the lock's hashes (`DAL116`).
- **Distribution:** dalforge is meant to be installed with mise's `go:`
  backend, pinned in the consuming repo's `mise.toml`:

  ```toml
  [tools]
  "go:github.com/gisripa/dalforge/cmd/dalforge" = "0.3.1"
  ```

  The version lives in `mise.toml`, not the user's `go.mod`, so `go.sum`
  never pins it. That's why dalforge needs its own lock. The lock is still
  install-agnostic, so a release binary or Homebrew install is checked the
  same way.
- **Version detection:** `go install` stamps the module version into the
  binary, and dalforge reads it with `debug.ReadBuildInfo()` (no ldflags). A
  local build reports `(devel)`. The lock check then warns instead of failing,
  so work on dalforge itself isn't blocked.
- **No cgo:** dalforge must stay pure Go, because `go install` compiles on the
  user's machine and cgo would require a C toolchain and slow installs. Its
  current dependencies (protocompile, pgx) are pure Go, and any new dependency
  has to be too.

The lock deliberately holds no environment facts. `pg.version` stays in
`dalforge.yaml`.

## 3. Layering

Every concern sits in exactly one layer. A new backend implements the backend
layer, including all generated code. It never changes the IDL model.

| Layer | IDL | Generator | Generated code / runtime |
|---|---|---|---|
| **Core** (backend-neutral) | `dal.v1`: entity, fields, format, roles, shard key, access patterns, consistency | IR, access-path resolution, core lint (DAL1xx) | runtime `dal`: `Page[T]`, `All`, page-token envelope, sentinel errors, retrier |
| **Backend** (pg today) | `dal.pg.v1`: physical types, SQL defaults, explicit/partial/covering indexes, index budget | physical model, backend lint (DAL2xx, DAL3xx), schema/query/sqlc/migration emitters, DAL package emitter | the DAL package per proto package (repository interfaces + implementation + model aliases over sqlc's output), runtime `dal/dalpg` (`DB` pools, keyset codec, error mapping, `WithTx`) |

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
- No driver types in the generated API: no `pgx`, `pgtype` or `pgconn` in
  any exported signature or model field (§7).
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
| removed fields | retired columns (kept, `NOT NULL` relaxed) | attributes simply stop being written; GSI changes belong to IaC |
| transactions | `WithTx` on `pgx.Tx` | `TransactWriteItems`; a different model, not in the core interface |

The core model maps cleanly. The same ESR (Equality, Sort, Range; see §5)
rule holds in both backends: the range goes on the leading sort column. The DynamoDB-specific limits (single sort
attribute, no non-key uniqueness, no strongly consistent GSI reads) would be
enforced as that backend's own lint rules, DAL4xx.

## 4. IDL reference

Core options (`dal.v1`):

| Option | On | Purpose |
|---|---|---|
| `(dal.v1.table)` | message | entity name, shard key |
| `(dal.v1.field)` | field | format, primary key, unique, role |
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

`optional` fields become pointers (`*T`) in the model struct, via the
generated sqlc type overrides (no `pgtype` wrappers). The Go type follows the
*physical* column: nullable columns are pointers, except where nil already
means NULL (`[]byte`, slices, `json.RawMessage`). Role columns get schema defaults
(`now()` for create/update time, `1` for version), so adding them later stays
DAL302-safe. The physical model lives in `internal/backend/pg`
(`pg.Build`). Enums are stored
by name, which is readable and survives renumbering. There's no `CHECK`
constraint, because adding a value would then need a migration before the code
could use it. `custom_type` leaves room for extension types such as
`vector(1536)` or PostGIS types, without first-class support in v1. A
`custom_type` column must also set `go_type` (e.g.
`github.com/pgvector/pgvector-go.Vector`), which becomes a sqlc type override
and the domain field's type.

### Postgres version, extensions and type evolution

**Two declarations, two owners:**

| Where | What | Example | Default |
|---|---|---|---|
| IDL: `(dal.pg.v1.file)` | what the schema **needs**: a portable requirement | `{min_version: 16 extensions: ["vector"]}` | dalforge's floor (16), no extensions |
| `dalforge.yaml`: `pg.version` | what you **deploy to**: an environment fact | `pg: {version: "16.9"}` | 16 |

Lint checks every type and feature in use against both:

- `DAL207`: the target is older than `min_version`, or something in use needs
  a newer version.
- `DAL208`: a `custom_type` comes from an extension that isn't declared, or a
  `custom_type` has no `go_type`.

The deploy target also drives the local `compose.yaml` image tag and the
integration test's version check, instead of hard-coding them.

**How supported types evolve when more versions are supported:**

1. **The bundled `Type` enum is append-only.** It's the union of first-class
   types across every supported version. Values are never renumbered or
   removed. A breaking change would ship as `dal.pg.v2` next to v1.
2. **Version support is data in the generator, not the proto.** A capability
   table (`internal/backend/pg`) maps each type and feature to the version it
   first appeared in, and to the extension that provides it if any. Examples:
   - multirange types: 14
   - `NULLS NOT DISTINCT`: 15
   - `MERGE … RETURNING`, `JSON_TABLE`: 17
   - built-in `uuidv7()`, virtual generated columns: 18
   - `vector`: provided by the `vector` extension

   Adding version N means adding table rows and enum values. Existing IDL is
   untouched.
3. **The bundled options match the binary.** The options protos are embedded
   in `dalforge`, so the set of usable types is exactly what that release
   knows. Using a newer value with an older binary fails at compile time with
   an unknown enum name, never silently. Additive option changes ship in
   minor releases.
4. **"First-class" means the generator knows the whole pipeline for the
   type:** Go type, pgx codec, sqlc override, keyset encoding and comparison
   semantics. Everything else uses `custom_type` + `go_type`, and a type is
   promoted into the enum once it has proven itself.
5. **Extension types are checked against declared extensions**, not the PG
   version. Their availability depends on the extension version Aurora ships
   for that engine version.
6. **Dropping an old version** (e.g. when 16 reaches end of life) only raises
   dalforge's floor. No enum values are removed.
   Per-version option sets (`dal.pg16.v1`, `dal.pg17.v1`, …) were considered
   and rejected. They'd make version upgrades an IDL rewrite and duplicate
   every option, while the capability table already answers "valid on version
   N?" in one place.
7. **Testing a version range:** supporting more than one major version means
   running the integration suite as a matrix, with one compose image per
   supported version.

## 5. Access paths and index derivation

Physical structures are **query-first**: they are derived from the access
patterns that need them, not declared up front. A List resolves to an access
path of three parts, which give the composite index its column order. This is
the **ESR rule (Equality, Sort, Range)**, a common heuristic for composite
indexes (MongoDB's indexing guidance calls it that):

- **E**quality filters (`eq`) come first. They pin exact values, so the index
  narrows to one contiguous slice.
- **S**ort (`order_by`) comes next. Within that slice the index is already in
  order, so no separate sort step is needed.
- **R**ange (`range`) comes last. A B-tree scans only one contiguous range, so
  no column after a range can be used efficiently. In DALForge the range must
  also be the leading sort column, so Sort and Range share one column.

The primary key is appended as a tie-breaker. That makes the order total,
which keyset pagination requires.

### One shape per rpc

Every rpc is a single, fixed access path. Every `eq` column is a required
parameter, and the generated SQL never uses catch-all predicates such as
`(@x IS NULL OR col = @x)`. Those predicates let one query serve many filter
combinations, and in doing so they defeat the index and destabilise generic
plans.

The consequence is deliberate: **a different filter combination is a
different rpc, with its own index.** The shape is explicit at the call site,
and the index cost of each new shape is visible in the IDL and in lint
(`DAL201`).

### Range filters

`range` names one column:

- **Bounds:** it becomes a half-open interval with **both bounds required**:
  `col >= @<col>_from AND col < @<col>_to`. Required bounds keep a single plan
  shape, so prepared and generic plans always use the index. An open-ended
  range is either a separate rpc or a caller-supplied sentinel bound.
- **Sort:** a range implies sorting by that column (`ASC` unless `order_by`
  says `DESC`). If `order_by` is given, its leading column must be the range
  column (`DAL101`).
- **How many:** only one range per rpc. The proto shape enforces this, since
  `range` is a single string.

A B-tree can serve exactly one range after an equality prefix. That's why the
rules are: equality columns first and strict, then at most one range or sort
column, then the primary key.

### Nullable sort columns

Keyset row comparisons treat NULL as unknown, so rows with a NULL sort value
would be silently skipped. A nullable (`optional`) column may appear in
`order_by` only when it's also the `range` column, because the range predicate
already excludes NULLs. Otherwise it's error `DAL115`. In that case Postgres
also adds `col IS NOT NULL` to the derived partial index predicate.

### Worked example: the ESR ladder

`Order` has `state`, an `account_id`, and an optional `fulfilled_at`. Each
filter combination is its own rpc, and each derives the narrowest index that
serves it:

| rpc | eq | range | sort | derived Postgres index |
|---|---|---|---|---|
| `ListOrdersByState` | `state` | — | `id` (PK fallback) | `(state, id)` |
| `ListOrdersByStateAndFulfilledAt` | `state` | `fulfilled_at` | `fulfilled_at, id` | `(state, fulfilled_at, id) WHERE fulfilled_at IS NOT NULL` |
| `ListOrdersByAccountAndStateAndFulfilledAt` | `account_id, state` | `fulfilled_at` | `fulfilled_at, id` | `(account_id, state, fulfilled_at, id) WHERE fulfilled_at IS NOT NULL` |

(All three indexes also carry `deleted_at IS NULL` when the entity has soft
delete.)

**How the ladder works:**
- **Equality is strict:** equality columns form the index prefix.
- **One range, last:** the single range column comes after them.
- **Each rung adds a column:** a step up the ladder adds an equality column in
  front of the existing range. It never adds a second range.

**Why the first two rungs don't share an index:** `ListOrdersByState` sorts by
`id` and the second rpc sorts by `fulfilled_at`, so their indexes differ after
`state`. If `ListOrdersByState` declared `order_by: ["fulfilled_at"]`, the two
rpcs would share one index. Lint `DAL206` points out cases like this. Note
that the partial predicate would then exclude unfulfilled orders, which is
exactly the kind of trade-off the hint makes visible.

**Naming:** the expected name is
`List<Entities>By<Eq1>And<Eq2>…And<Range>`. A different name gets warning
`DAL114`, with the suggested name.

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
2. **The `range` column**, ascending.
3. **A backend-declared index.** If the rpc gives `eq` but no `order_by`, and
   an explicit index has leading columns exactly equal to the `eq` set (in any
   order), that index's remaining columns become the sort. For Postgres that's
   a `(dal.pg.v1.table).indexes` entry. Use this when several List rpcs share
   one index.
4. **The primary key.** It's deterministic, and it produces lint `DAL103` if the
   key isn't time-ordered, e.g. a random UUIDv4 rather than UUIDv7 or a
   sequence. The results are stable but meaningless to users.

### Get

`get` looks up by primary key by default. `by: [...]` looks up by other
columns, which must be covered by a `unique` column or a unique index,
otherwise it's lint error `DAL104`. A non-unique lookup is a List.

### Postgres index merging and budget

- **Prefix merging:** derived indexes are deduplicated when one index's full
  column list, sort included, is a prefix of another's with compatible
  directions. A pure-equality need such as `get.by` on `account_id`, or an
  existence check, is served by `(account_id, created_at DESC, id DESC)`. A
  List on `account_id` sorted by `id` is not: it needs `(account_id, id)`.
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
| DAL107 | error | request or response message doesn't match the declared pattern. Get: the entity, or exactly the `by` fields. List: exactly the `eq` fields, `<range>_from`/`<range>_to`, `page_size` (int32), `page_token` (string); the response has one repeated entity field plus `next_page_token`. Create/Update/Upsert: the entity. Delete: the entity or exactly the key fields; the response is the entity or `google.protobuf.Empty` |
| DAL109 | warn | an `order_by` column can be changed by an `update`/`upsert`, so rows can move between pages mid-iteration |
| DAL110 | error | `shard_key` names a missing or repeated field |
| DAL111–113 | error | field-number identity violations; see §8 |
| DAL114 | warn | rpc name doesn't match its shape (`List<Entities>By<Eq…>And<Range>`); suggests the expected name |
| DAL115 | error | a nullable column is in `order_by` without also being the `range` column |
| DAL117 | error | a query reference (`by`, `eq`, `range`, `order_by`, `columns`, `conflict_on`) names a missing or repeated field; a column name used by mistake gets a hint |
| DAL118 | error | `update`/`upsert` `columns` sets a key field or a role-managed field |
| DAL116 | error | a vendored copy of the options protos differs from the hashes in `dalforge.lock` |

Postgres:

| ID | Severity | Rule |
|---|---|---|
| DAL201 | warn | index budget exceeded |
| DAL202 | error | `order_by` contradicts an explicit index whose leading columns equal `eq` |
| DAL203 | warn | mixed sort directions; the keyset predicate can't use a row comparison |
| DAL204 | error | a table, column or index name isn't a valid unquoted identifier: lowercase letters, digits and `_`, starting with a letter or `_`. Also flagged: longer than 63 bytes (Postgres silently truncates), or a reserved SQL keyword. dalforge never quotes identifiers, so names must also work in custom queries |
| DAL205 | error | two rpcs generate the same sqlc query name (`<Store minus "Store"><Method>`, e.g. stores `Order` and `OrderStore` both give `OrderGet`), or a generated name collides with a custom sqlc query |
| DAL213 | error | two columns of the same SQL type need different Go types (e.g. `numeric` as `string` and as `decimal.Decimal`). sqlc's nullable insert-parameter overrides are per type, so give them the same `go_type` |
| DAL214 | error | a table needs a sqlc rename (its name differs from the message name) but some column has the same name; sqlc's global rename would rename that column's field too |
| DAL206 | info | two List rpcs share an equality prefix but sort differently; aligning `order_by` would let them share one index |
| DAL207 | error | the deploy target (`dalforge.yaml` `pg.version`) is older than `min_version`, or a type/feature in use needs a newer version |
| DAL208 | error | a `custom_type` has no (or an invalid) `go_type`, a `go_type` is set without `custom_type`, or a known extension type (`vector`, `geometry`, `citext`, …) is used without declaring its extension in the file's `(dal.pg.v1.file).extensions` |
| DAL209 | error | no or incompatible type mapping: `uint64` without `custom_type`, a `format` on a non-string field, or an explicit pg `type` that doesn't fit the field's kind (e.g. `uuid` on `int64`) |
| DAL210 | error | two entities map to the same table, or two fields to the same column |
| DAL211 | error | an explicit pg index references (`columns`, `include`) a missing or repeated field. A column name used by mistake gets a hint |
| DAL212 | info | an `optional` field has a column `default`: a nil insert parameter means "use the default", so Create can't insert NULL (Update can) |
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
for o, err := range dal.All(ctx, ordersdal.ListByAccountParams{AccountID: id}, repo.ListByAccount) {
	if err != nil {
		return err
	}
	// ...
}
```

## 7. Generated Go API

For each proto package, dalforge generates one **DAL package** (e.g.
`ordersdal`) next to sqlc's output. It contains:

- **model aliases:** `type Order = sqlcdb.Order`. sqlc's model structs are the
  entity types; there's no second domain model and no copy or mapping code;
- **per-store interfaces:** `OrderReadRepository`, `OrderWriteRepository` and
  `OrderRepository`, one method per rpc;
- **the implementation** over sqlc's `Queries`: pool routing, retries, page
  tokens, error mapping and the compare-and-swap follow-up;
- **parameter structs** named after the request messages.

Services import only the DAL package. They never see sqlc's `Queries`, a
pool, SQL or page-token internals.

**Naming** (step 1.8, checked by compiling against sqlc's output):
- **Package:** the last non-version segment of the proto package, plus `dal`
  (`orders.v1` → `gen/orders/v1/ordersdal`, `acme.billing.v2beta1` →
  `billingdal`). A store's package also aliases the models it returns.
- **Models are named after the proto message:** sqlc runs with
  `emit_exact_table_names`, plus a `rename` for each table whose name differs
  from its message (`orders: Order`), so sqlc's singularization never has to
  be imitated.
  - **DAL214:** sqlc's `rename` is global and would also rename a column's
    field with the same name, so a renamed table that collides with a column
    name is an error.
- **Interfaces:** `<Store minus "Store">ReadRepository` / `WriteRepository` /
  `Repository`. Methods are named after the rpc.
- **Signatures mirror sqlc's:** one query parameter is passed inline
  (`GetById(ctx, id uuid.UUID)`); more use sqlc's params struct, re-exported as
  an alias (`OrderUpdateStatusParams`). Get, Create and Update return the
  model. Delete returns the model when the rpc does, otherwise just `error`.
- **Create params document what nil means** for every field, in the alias's
  doc comment.
- **Module path:** emitting Go needs the consuming module's path
  (`Layout.Module`, from `dalforge.yaml` in 1.10) to import sqlc's output.

### From IDL to running code

The IR (§2) is the generator's internal model. It never ships. `dalforge
generate` reads it and writes SQL for sqlc plus the DAL package:

```
                 dalforge generate (reads IR)
                 │
     ┌───────────┴────────────────────────────┐
     ▼                                        ▼
SQL side (fed to sqlc)                 Go side (dalforge's own)
  schema/schema.sql                      gen/orders/v1/ordersdal/
  queries/generated/*.sql  (DO NOT EDIT)   model aliases, Read/Write/Repository
  sqlc.yaml (type overrides, vet)          interfaces, implementation
     │
     ▼ sqlc generate                    queries/custom/*.sql  (user-owned,
  gen/sqlcdb/   Queries, models,          never touched by dalforge)
                ListOrdersByAccount(...)
```

At runtime, a call goes down these layers:

```
your service
  → ordersdal.OrderReadRepository.ListByAccount(ctx, params)  // interface: models + plain Go types
  → ordersdal implementation:
        picks reader/writer pool, wraps the call in the retrier,
        decodes the page token, fetches page_size+1 rows,
        builds next_page_token, maps errors to dal sentinels
  → sqlcdb.Queries.ListOrdersByAccount(ctx, args)              // sqlc-generated SQL call
  → pgx v5 pool → Postgres
```

```go
// package ordersdal (generated)
type Order = sqlcdb.Order // sqlc's model; fields are uuid.UUID, time.Time, *string ...

type OrderReadRepository interface {
	GetByID(ctx context.Context, id uuid.UUID) (Order, error)
	ListByAccount(ctx context.Context, p ListByAccountParams) (dal.Page[Order], error)
}

type OrderWriteRepository interface {
	Create(ctx context.Context, p CreateOrderParams) (Order, error) // every field a pointer
	UpdateStatus(ctx context.Context, o Order) (Order, error) // version compare-and-swap
	Delete(ctx context.Context, id uuid.UUID) error
}

type OrderRepository interface {
	OrderReadRepository
	OrderWriteRepository
}

func NewOrderRepository(db dalpg.DB, opts ...dalpg.Option) OrderRepository

// WithTx runs fn in a transaction on the writer pool. Tx is the DAL's own
// type, never pgx.Tx.
func WithTx(ctx context.Context, db dalpg.DB, fn func(tx Tx) error) error
```

### No driver types in the API

pgx is the driver layer, so no `pgx`, `pgtype` or `pgconn` type appears in
any exported signature or model field of the DAL package:

| Leak | Prevention |
|---|---|
| sqlc model fields (`pgtype.UUID`, `pgtype.Timestamptz`, `pgtype.Text`, `pgtype.Numeric` …) | the generated `sqlc.yaml` has an override for **every** type dalforge maps: `uuid.UUID`, `time.Time`, pointers for nullable columns, `json.RawMessage`, `netip.Prefix`, …. A `custom_type` must declare its `go_type` |
| transactions (`pgx.Tx`) | `WithTx` hands out the DAL's own `Tx` |
| errors (`pgx.ErrNoRows`, `*pgconn.PgError`) | mapped to `dal` sentinels; the driver error is only the wrapped cause |
| pools (`*pgxpool.Pool`) | only at the composition root, when constructing `dalpg.DB{Reader, Writer}` |

A test (step 1.12) inspects the generated package's exported API with
`go/types` and fails on any driver type. A `go list -deps` check keeps the IR,
protobuf and the generator out of it.

### Insert parameters: every field is a pointer

A Postgres column default only applies when an INSERT leaves the column out.
One generated INSERT serves every call, so it lists every column. With plain
values, an unset Go field (`""`, `uuid.Nil`) would be written as-is: a
declared default would silently never apply, and a forgotten required field
would insert its zero value.

So `Create` (and Upsert's insert half) take a params struct, an alias of
sqlc's params type, not the read model, and **every field is a pointer**.
`nil` always has a defined meaning:

| Field | `nil` means | Enforced by |
|---|---|---|
| required, no default | **error**: the field is missing (`dal.ErrMissingField`, wraps `ErrInvalidArgument`, names the field) | the DAL, before any round trip |
| required, with default | the column default | SQL: `COALESCE(sqlc.narg(status), 'pending')` |
| `optional`, no default | `NULL` | SQL |
| `optional`, with default | the column default, so Create can't insert NULL (DAL212 info; Update can) | SQL |
| primary key with `FORMAT_UUID` | the DAL assigns a UUIDv7 | the DAL |
| role fields | not in the params | — |

**How the generated SQL gets sqlc to produce pointers** (found by running sqlc
on the output, step 1.7):
- **Why `narg` alone fails:** with plain `sqlc.narg(id)`, the column's type
  override wins and its nullability is dropped (`uuid.UUID`, not a pointer),
  and inside `COALESCE` sqlc can't infer a type at all (`interface{}`).
- **Every insert parameter is cast to its column type:**
  `sqlc.narg(id)::uuid`, `COALESCE(sqlc.narg(status)::text, 'pending')`. sqlc
  then types the parameter from the cast, as nullable.
- **`sqlc.yaml` adds a nullable `db_type` override** for each SQL type sqlc
  would otherwise map to a `pgtype` wrapper (`uuid`, `jsonb`, `numeric`,
  `timestamptz`, `date`, `inet`, custom types). Text, integers, booleans,
  floats, `bytea` and arrays are already clean pointers or nil-able slices.
- **sqlc's catalog naming is inconsistent:** `numeric` only matches as
  `pg_catalog.numeric`, while `timestamptz` only matches unqualified. The sqlc
  feedback test pins this.
- **A known limit:** these overrides are per SQL type, so two columns of one
  type that need different Go types (`numeric` as `string` here,
  `decimal.Decimal` there) are a conflict (DAL213).
- **The read model is unaffected:** its per-column overrides keep plain
  values for required columns.

Go 1.26's `new(expr)` keeps call sites short, with no helper needed:

```go
o, err := repo.Create(ctx, ordersdal.CreateOrderParams{
	AccountID: new(acct),
	Note:      new("rush"), // Status omitted → 'pending'
})
```

The read model (`Order`) keeps plain values for required columns. Pointer
semantics for *updates* (whether nil means "unchanged" or "error") is still
open and is decided with the DAL implementation.

### Writes: basic and safe, the rest is custom

Generated writes cover the common footguns and nothing more:

- **Update with a `ROLE_VERSION` field** is always a compare-and-swap:
  `SET …, version = version + 1 … WHERE id = @id AND version = @version`. If
  zero rows are updated, a follow-up existence check (only on that failure
  path) tells `ErrNotFound` from `ErrVersionConflict`.
- **Upsert** targets the primary key or a unique `conflict_on`, and the last
  writer wins: the version is bumped, with no compare-and-swap, so it stays
  idempotent for retries. It never revives a soft-deleted row
  (`DO UPDATE … WHERE deleted_at IS NULL`); hitting one returns
  `ErrAlreadyExists`.
- **Delete** is soft when the entity has `ROLE_DELETE_TIME`.

Anything richer goes in a custom sqlc query under `queries/custom/`. That
includes state-transition guards (`WHERE status = 'pending'`), unconditional
overwrites of a versioned row, multi-row updates and joins. Generated files
are marked `DO NOT EDIT` and live only under `queries/generated/`, so the two
never mix. Custom queries land on the same sqlc `Queries` and can join a
`WithTx` transaction.

- **Postgres routing:** `dalpg.DB` holds a `Reader` and a `Writer` pool.
  Eventual reads go to the reader. `CONSISTENCY_STRONG` reads and all writes
  go to the writer. A single-instance setup passes the same pool twice.
- **`dalpg.Runner`** is the one runtime entry point the generated
  implementation calls (step 1.9b):
  - `Read(ctx, op, strong, fn)` routes to the reader, or the writer when the
    read is strong or there's no reader. `Write(ctx, op, fn)` uses the
    writer.
  - `InTx(ctx, op, fn)` runs on the writer and hands `fn` a
    transaction-bound Runner, whose statements are never retried on their
    own; the whole transaction is retried by the policy. Nested `InTx` joins
    the outer transaction.
  - Every failure becomes a classified `*dal.Error` before the retrier sees
    it. `fn` receives a `dalpg.DBTX`, which has the same method set as sqlc's,
    so `sqlcdb.New(q)` works on a pool or a transaction alike.
  - `dalpg` depends only on pgx (plus `dal`). The generated DAL package's
    *exported* API still exposes no pgx types; `DBTX` is only used inside the
    generated implementation.
- **Runtime libraries** live in this module and are imported, not generated,
  so bug fixes don't need a regeneration:
  - `dal`: `Page[T]`, `All`, the page-token envelope, sentinel errors,
    `Retrier`, `Op`, `Retryability`
  - `dal/dalpg`: `DB`, `Option`, `WithTx`, the keyset payload codec, the
    SQLSTATE classifier and error mapping

### Errors and retries

**Errors** are core sentinels, and each backend maps its own errors onto them:

| Sentinel | Postgres source |
|---|---|
| `dal.ErrNotFound` | `pgx.ErrNoRows` |
| `dal.ErrVersionConflict` | a CAS update that matched no row while the row exists (follow-up check) |
| `dal.ErrAlreadyExists` | `23505` unique violation |
| `dal.ErrInvalidPageToken` | a page token that fails to decode or doesn't match the query |
| `dal.ErrInvalidArgument` | caller error, never retried; `dal.ErrMissingField` (with `*dal.MissingFieldError` naming the field) wraps it for nil required insert params |

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
- **The default is `dal.DefaultRetrier`,** a `dal.Backoff{Attempts: 3, Base:
  25ms, Max: 1s}`: exponential backoff with full jitter, following
  `ShouldRetry` and respecting the context. If the context ends while waiting,
  it returns the operation's error joined with the context error.
  `dal.NoRetry` turns retries off. `ShouldRetry` never retries a canceled or
  expired context.

The generated implementation wraps each method in `retrier.Do`. Inside `WithTx`,
individual statements are never retried: a `40001` aborts the whole
transaction, so only the outer `WithTx` can be retried.

## 8. Migrations and n+1 compatibility

In a rolling deploy, the migration runs first. Then release N and N+1
instances share the schema. DALForge's contract:

> **Every schema change for N+1 is additive and backward compatible with the
> code still running as N.**

DALForge never removes or rewrites existing schema in a release. The two
classic footguns, a new `NOT NULL` column without a default and a column type
change, are lint errors. Each has a safe, additive alternative.

**The contract is core; the migrations are backend-specific.** Postgres emits
SQL. A DynamoDB backend would emit none for attributes, since they're
schemaless, and would leave GSI changes to infrastructure-as-code.

### What a release may change

| Change | N+1 migration | Why N keeps working | Verdict |
|---|---|---|---|
| new entity | `CREATE TABLE` | N doesn't know it | allowed |
| new `optional` field | `ADD COLUMN … NULL` | N's inserts omit it | allowed |
| new required field **with** a constant default | `ADD COLUMN … NOT NULL DEFAULT <const>` | N's inserts get the default | allowed |
| new required field **without** a default | — | N's inserts would fail | **DAL302**: add a default (`(dal.pg.v1.column).default`) or make it `optional` |
| type change (kind, `format`, pg `type`) on an existing field | — | N reads and writes the old type | **DAL303**: add a new field with a new number. proto forbids number reuse, and DAL111–113 enforce it |
| rename a column in place | — | N uses the old name | **DAL306**: add a new field. Renaming only the proto field while pinning `(dal.v1.field).name` is allowed |
| tighten an existing column: `optional` → required, add `unique` | — | N may write values the new rule rejects, and existing rows may already violate it | **DAL301**: not backward compatible. Do it in a hand-written migration, outside the generated flow |
| new access pattern / index | `CREATE INDEX CONCURRENTLY` | N is unaffected | allowed |
| remove a field (delete + `reserved`) | relax `NOT NULL` if the column has no default | N still reads and writes the column; N+1 omits it | allowed: the column is **retired** |
| remove an entity or an access pattern | nothing | N still uses the table or index | allowed: the table or index is **retired** |

### Removing things: retire, never drop

A removed field disappears from the IDL, from sqlc's schema, from the models
and from every generated query in N+1. The database column stays. Its
`NOT NULL` is relaxed if needed, so N+1's inserts (which omit it) succeed while
N keeps using it. The snapshot records it as retired, along with the release
that retired it. The same goes for removed entities (tables) and indexes no
access pattern needs any more.

So sqlc always sees exactly what the IDL declares, and the physical schema may
carry retired columns. No special view is needed: retired columns simply
aren't in the IDL. A custom query that still names a retired column fails at
`sqlc generate`, which catches leftover uses at build time.

**Dropping retired columns, tables and indexes is out of scope for v1.** It's
not additive, so it can never be part of the generated flow. The snapshot lists
what's retired and since when, and the manual explains how to drop them with a
hand-written migration once no deployed code uses them. Retired indexes still
cost writes, so the index budget lint (DAL201) counts them until dropped.

**Changing a type or renaming a column is two additive steps:**
1. Add the new field (new number).
2. Backfill it, in a migration or in application code.
3. Switch readers to the new field.
4. Remove the old field. Its column retires.

### Snapshot and diff

- **Snapshot:** `dalforge.snapshot.json` is committed alongside the IDL. It
  records tables, columns (keyed by proto field number, with column name,
  type, nullability, default and whether and when it was retired), reserved
  numbers, and indexes.
- **Generating a migration:** `dalforge migrate -name <slug>` diffs the IDL
  against the snapshot. It writes files in
  [golang-migrate](https://github.com/golang-migrate/migrate) format, then
  updates the snapshot. golang-migrate was picked because it's the most widely
  used Go migration tool, it has a pgx v5 driver, and Atlas can emit the same
  format.
- **Linting:** `dalforge lint` runs the same diff read-only and fails if the IDL
  and snapshot disagree without a migration, which makes it suitable for CI.

**golang-migrate caveat:** it runs a whole file in a single exec, and Postgres
treats a multi-statement exec as one implicit transaction. `CREATE INDEX
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
| DAL111 | error | a field number is reused for a different column, or for a retired one (it should have been `reserved`) |
| DAL112 | error | a field is removed without its number *and* name being `reserved` |
| DAL113 | error | an existing column name moves to a different field number (renumbering) |

Because of this, renames are detected exactly rather than heuristically: the
same number with a new column name is a rename in place (`DAL306`).

These rules are backend-neutral and come from the snapshot diff, so buf isn't
needed for them. DALForge's own extension numbers (`51000`, `51010`) are
protected by a unit test that pins them.

### Migration safety rules (Postgres)

| ID | Rule |
|---|---|
| DAL301 | tightening an existing column: `optional` → required, or adding `unique`. N may write values the new rule rejects, and existing rows may already violate it; do it in a hand-written migration |
| DAL302 | new required column without a constant default. N's inserts would fail; a volatile default would also rewrite the table |
| DAL303 | type change on an existing column (kind, `format` or pg `type`). It breaks N and needs a table rewrite; add a new field instead |
| DAL306 | rename in place: same field number, new *column* name. N still uses the old column; add a new field instead. Renaming only the proto field while pinning `(dal.v1.field).name` is allowed |

**Future option: Atlas.** Atlas could replace the diff engine, via declarative
`schema.sql` plus its lint analyzers, while still emitting golang-migrate
files. The snapshot and the retired-column history would still come from
DALForge, because Atlas has no notion of a field's release history.

## 9. Output layout

What a consuming repo looks like (paths are configurable in `dalforge.yaml`):

```
dalforge.yaml               # proto roots, backend, pg.version (deploy target), output dirs, index budget, page-size defaults
proto/orders/v1/orders.proto
dalforge.lock               # committed; toolchain pin (dalforge version, options hashes, sqlc)
dalforge.snapshot.json      # committed; source of truth for migration diffs
migrations/                 # generated golang-migrate files, append-only, reviewed
schema/schema.sql           # generated desired state (also the sqlc schema input)
queries/generated/*.sql     # generated; "Code generated by dalforge. DO NOT EDIT."
queries/custom/*.sql        # handwritten sqlc queries, never touched by dalforge
sqlc.yaml                   # generated
gen/sqlcdb/                 # sqlc output (both query sets)
gen/orders/v1/ordersdal/    # DAL package: model aliases, Read/Write/Repository interfaces, implementation
```

Generated query files hold one file per table, named `<table>.sql`. Each query
is named `<Store minus "Store"><Method>` for sqlc, lists its columns
explicitly, and uses proto field names as parameters (`@account_id`). Unique
fields on soft-delete tables become partial unique indexes
(`WHERE deleted_at IS NULL`). `sqlc.yaml` carries one type override per
column, nullable per-type overrides for insert parameters, and Go initialisms
(`id`, `ip`, `url`, `uuid`, `http`, `json`, `api`, …) so fields read
`AccountID`, `ClientIP` (design §7).

**When there are no queries at all** (entities but no stores, and no custom
queries), sqlc refuses to run. `dalforge generate` then skips sqlc with a
note rather than failing.

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
| Golden | unit tests comparing generator output with `testdata/**/*.golden` via `internal/golden` (`Assert` for one file, `AssertDir` for a generated file set, including stale-file detection) | `mise run test`; `mise run test:update` rewrites them (`DALFORGE_UPDATE_GOLDEN=1`, refused when `CI` is set) | nothing |
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
output. It models two entities, **`Account`** and **`Order`**
(`Order.account_id` refers to `Account.id`), to cover two things.

**The ESR ladder from §5:** `ListOrdersByState`,
`ListOrdersByStateAndFulfilledAt` and
`ListOrdersByAccountAndStateAndFulfilledAt`. The README shows each rpc next to
the index it derives, why the first two don't share an index, and the `DAL206`
hint that would let them.

**Application-level referential integrity, without an FK:**

- **Why it matters:** without an FK, inserting an order for a missing account
  *succeeds*. The orphan only shows up later, as an empty lookup far from the
  cause. sqlc type-checks queries but doesn't enforce integrity; only the
  database (an FK) or the application can.
- **Create:** the service's `CreateOrder` runs in `WithTx`. It first runs a
  custom sqlc query `SELECT 1 FROM accounts WHERE id = $1 FOR KEY SHARE`,
  which fails fast if the account is missing and blocks a concurrent account
  delete until commit (the race that hand-written checks usually miss). Then
  it calls the generated `Create`.
- **Delete:** `DeleteAccount` checks `EXISTS (SELECT 1 FROM orders WHERE
  account_id = $1)` in the same transaction before the generated `Delete`. The
  ladder's `(account_id, …)` index serves that check.
- **Sharding:** if accounts and orders live on different shards, this can't
  be atomic. The README says so and points to async reconciliation as the
  usual answer.
- **Double duty:** these custom queries also demonstrate the sqlc escape hatch
  next to the generated repositories.

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

## 12. Documentation

Because DALForge introduces its own IDL, the **user manual** (`docs/manual/`)
is a first-class deliverable. This design doc records decisions and their
reasons; the manual teaches users every option, access pattern, combination,
limitation, type mapping and lint rule, without relying on `examples/`.

The manual is kept true by tests:
- every rule ID has a catalog entry, and every option field is documented;
- complete IDL snippets in the manual compile;
- diagnostics link to their rule entry.

The manual is written once the MVP exists (end of phase 2 plus a runnable demo), so it describes a stable surface. From then on, every increment updates the sections it affects.

## 13. Roadmap

1. **CRUD by key:** IR loader, core/backend split, type mapping, `schema.sql`,
   Get/Create/Update/Delete queries, `sqlc.yaml` with full type overrides,
   the DAL package (model aliases, interfaces, implementation), the `dal` and
   `dal/dalpg` runtimes, and the no-driver-types API guard.
   Error classification and the pluggable retrier. Integration tests against
   the compose Postgres 16.9. First cut of
   `examples/orders`.
2. **List and indexes:** access paths, sort resolution, keyset List with page
   tokens and `dal.All`, index derivation and merging, the DAL1xx/DAL2xx lint
   rules. Upsert, soft delete, optimistic locking, and `WithTx` if it stays
   thin.
3. **Migrations:** snapshot, diff, the additive contract with retired columns, DAL3xx rules,
   golang-migrate file emission.
4. **Extensions:** `custom_type` ergonomics (pgvector, PostGIS), buf plugin
   mode, the Atlas backend, sqlc vet rule packs, shard-key enforcement, and a
   DynamoDB backend.

## Decision log

| Topic | Decision |
|---|---|
| Generator packaging | Standalone CLI embedding protocompile; buf plugin later |
| Domain types | sqlc's model structs, re-exported as type aliases from the generated DAL package. No separate domain model and no copy layer (revised 2026-09-30; originally generated structs). Proto is only the IDL |
| Driver isolation | No `pgx`/`pgtype`/`pgconn` types in the DAL package's exported API: full sqlc type overrides, the DAL's own `Tx`, errors mapped to `dal` sentinels, pools only at the composition root. Enforced by a `go/types` test |
| Insert parameters | Every field is a pointer in Create/Upsert params (decided 2026-10-01). nil means: required without default → `ErrMissingField`; with default → the default (`COALESCE(sqlc.narg(..), default)`); optional → NULL; UUID PK → a UUIDv7. Call sites use Go 1.26 `new(expr)` |
| Generated writes | Basic and safe only: version compare-and-swap on update (follow-up existence check tells `ErrNotFound` from `ErrVersionConflict`), upsert on the PK or a unique field (last writer wins, never revives soft-deleted rows), soft delete. Everything else is a custom sqlc query in the user-owned `queries/custom/` |
| Index model | Query-first; explicit indexes only for partial/covering/sort-pinning |
| Sort with no `order_by` | Inherit from matching explicit index, else PK + DAL103 |
| Migrations | Snapshot diff → golang-migrate SQL; Atlas as a future backend |
| Lifecycle | Additive-only contract (revised 2026-10-01): every N+1 schema change is additive and backward compatible with N. Removing a field (delete + `reserved`) retires its column, which is kept with `NOT NULL` relaxed, never dropped. `STATE_DEPRECATED` was removed. Type changes and renames are a new field. Dropping retired columns is out of scope for v1 (hand-written migrations) |
| Foreign keys | Non-goal (sharding) |
| Sharding | `shard_key` declared and validated; routing and enforcement out of scope |
| Page tokens | In scope. Base64url JSON envelope, unsigned, bound to query + filters |
| Batch operations | Out of scope |
| Backend layering | The IDL, IR and lint are backend-neutral; generated code is backend-specific because the backend is chosen at declaration time. Postgres is the reference backend; DynamoDB mapping validated on paper only |
| Platform | Aurora PostgreSQL 16.x, pgx v5, Go 1.26+ |
| Options Go bindings | protoc (pinned in mise) + protoc-gen-go (a `go tool` dependency, so it always matches the protobuf runtime); no buf. Checked in next to the protos; `mise run proto:check` (part of `check`) fails on stale output; unit tests pin extension numbers and import paths. Field-number safety of user IDL comes from the snapshot rules (DAL111–113) |
| Testing | Unit + golden in `mise run check`; `integration` build tag against a docker compose `postgres:16.9` in `mise run test:integration`; `check:all` for CI |
| List shapes | One fixed shape per rpc: required equality params, at most one range (half-open, both bounds required) as the last column; each filter combination is its own rpc and index (ESR ladder); naming lint DAL114 (warn); nullable sort columns only as the range column (DAL115) |
| Referential integrity | Not generated. Demonstrated in `examples/orders` (Account ↔ Order) as an application pattern: `WithTx` + `FOR KEY SHARE` + custom sqlc queries. Generated opt-in checks are a possible later addition |
| PG version & types | The IDL declares requirements (`(dal.pg.v1.file)`: `min_version`, `extensions`); `dalforge.yaml` declares the deploy target (`pg.version`). The `Type` enum is append-only. Per-version and per-extension availability lives in a generator capability table, checked by DAL207/DAL208. `custom_type` requires `go_type` |
| Distribution | Consumers install dalforge with mise's `go:` backend (version in their `mise.toml`). The binary stays CGO-free, and its version comes from build info |
| sqlc pinning | A prebuilt binary through mise in the consuming repo, checked against `dalforge.lock`; replaces `go tool sqlc` (cgo) |
| Toolchain pinning | `dalforge.lock` (TOML: dalforge version, options hashes, sqlc version), committed. Mismatches fail until `dalforge lock -upgrade`. Separate from the schema snapshot. Per-PG-version option sets were rejected |
| sqlc's schema | Exactly what the IDL declares; retired columns exist only in the physical schema and snapshot. The earlier "schema view without deprecated columns" proposal was dropped together with `STATE_DEPRECATED` |
| Container runtime | Colima via mise (dedicated `dalforge` profile, repo-scoped `DOCKER_HOST`); native Docker in CI. Chosen over embedded Postgres so that future backends and fault-injection proxies share one mechanism |
| Retries | SQLSTATE classification into Retryable / RetryableIfIdempotent / NotRetryable, plus per-op idempotency; pluggable `dal.Retrier` (closure-friendly) and classifier; built-in default; failsafe-go adapter shown in examples |
| Onboarding | `examples/orders` standalone consumer module with a README walkthrough, kept fresh by `mise run examples` |
