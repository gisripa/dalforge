# DALForge design

This document records how DALForge works and why. The user manual
([`docs/manual/`](manual/README.md)) teaches how to use it; the options are
defined in [`proto/dal/v1/options.proto`](../proto/dal/v1/options.proto)
(backend-neutral) and [`proto/dal/pg/v1/options.proto`](../proto/dal/pg/v1/options.proto)
(Postgres).

**Status:** v0 is released. Everything below is implemented except where a
section says *planned*: generated migrations (§8, the snapshot, DAL111–113
and DAL3xx) are the main planned piece.

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
- *(planned)* versioned migrations, linted for n+1 rolling-deploy
  compatibility (§8).

Handwritten sqlc queries remain a first-class escape hatch alongside the
generated ones. They live in a user-owned directory (`queries/custom/`) that
dalforge never touches; generated files say `DO NOT EDIT`.

**The IDL, IR and lint are backend-neutral; generated code isn't.** The backend
is chosen at declaration time (the IDL's backend options and `dalforge.yaml`),
and the generated code is written for that backend. A second backend
(DynamoDB) would reuse the IDL model and bring its own generated code. Only
Postgres is built (§3).

**Non-goals**
- Complex queries (joins, aggregates, CTEs). Write those as custom sqlc
  queries.
- Foreign keys, and generated referential-integrity checks. FKs don't work
  well with sharded tables. Application-level integrity is shown as a pattern
  in `demos/orders` (§11) rather than generated.
- Shard routing. A `shard_key` can be declared and is validated, but nothing
  routes or enforces by it.
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
                               │   (planned) migrations             │
                               └────────────────────────────────────┘
```

- **A standalone CLI** (`dalforge generate | lint | lock | version`, with
  `migrate` reserved for migrations) that embeds
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
- **sqlc is pinned in the consuming repo's `mise.toml`** (`sqlc = "1.31"`,
  a prebuilt binary from mise's registry), the same way dalforge itself is
  installed. dalforge runs `sqlc` from `PATH` and checks `sqlc version`
  against `dalforge.lock`. sqlc isn't a `go tool` dependency because its
  Postgres parser (`pg_query_go`) uses cgo: `go tool` would compile C code on
  first use, which is slow and needs a C compiler.
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
  snapshot's column names, not proto names. *(Planned: DAL306 needs the
  schema snapshot, §8.)*
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

### Toolchain lock and distribution

Two checked-in files hold state, each with one job:

| File | Like | Records |
|---|---|---|
| `dalforge.snapshot.json` *(planned)* | Terraform state | what the deployed schema looks like (for migrations, §8) |
| `dalforge.lock` | `.terraform.lock.hcl` | which toolchain produced the generated code |

```toml
# dalforge.lock: maintained by dalforge, do not edit. Commit it.
# Update it with `dalforge lock -upgrade` after changing dalforge or sqlc.
lock_version = 1
dalforge = "v0.0.2"

[options]
"dal/pg/v1/options.proto" = "sha256:41ab…"
"dal/v1/options.proto" = "sha256:9f2c…"

[tools]
"sqlc" = "v1.31.1"
```

**Behaviour:**
- **Creation:** `dalforge generate` writes the lock if it's missing.
- **Mismatch:** if the running binary's version or bundled options hashes
  (or, for `generate`, the sqlc version) differ from the lock, `generate`
  and `lint` fail, naming each difference. `dalforge lock
  -upgrade` is the explicit way forward, and the lock change shows up in
  review. A teammate or CI job with a different dalforge can't silently
  regenerate different code.
- **Vendored copies:** copies of the options protos kept under a proto root
  for editor or buf tooling are compared with the options bundled in the
  binary (`DAL116`); dalforge always uses the bundled ones.
- **Distribution (decided 2026-10-02):** a `vX.Y.Z` tag runs CI, then
  GoReleaser publishes linux/darwin × amd64/arm64 binaries with checksums to
  GitHub Releases (`.goreleaser.yaml`, `.github/workflows/release.yml`).
  Consumers pin it in their `mise.toml`, either as a prebuilt binary or built
  from source:

  ```toml
  [tools]
  "github:gisripa/dalforge" = "X.Y.Z"
  # or: "go:github.com/gisripa/dalforge/cmd/dalforge" = "X.Y.Z"
  ```
- **The runtime is its own module (decided 2026-10-02):**
  `github.com/gisripa/dalforge/dal` (packages `dal`, `dal/dalpg`) has its own
  `go.mod`, so importing it doesn't pull the generator's dependencies
  (protocompile, yaml, protobuf) into a user's module graph. It's released in
  lockstep: the release workflow tags `dal/vX.Y.Z` on the same commit as
  `vX.Y.Z`, and users use the same version for both. The root module never
  imports the runtime (generated code names it only as a string), so it needs
  no `replace` directive, which `go install …@version` would reject.

  The version lives in `mise.toml`, not the user's `go.mod`, so `go.sum`
  never pins it. That's why dalforge needs its own lock. The lock is still
  install-agnostic: a release binary and a `go install` build are checked
  the same way. The manual explains the generator/runtime split to users
  (`docs/manual/project-setup.md`).
- **Version detection:** release binaries get the version through ldflags
  (`internal/pipeline._version`); `go install …@vX.Y.Z` stamps it into the
  build info, which dalforge reads with `debug.ReadBuildInfo()` otherwise.
  `dalforge version` prints it.
  - **Local builds** are recorded as `(devel)` and only warn on a mismatch, so
    work on dalforge itself isn't blocked. That includes the pseudo-versions
    Go stamps on builds from a git checkout
    (`v0.0.0-20261002040003-851063fe8fa3+dirty`), which change with every
    commit and would otherwise make the lock churn.
  - **`dalforge lock`** shows whether the lock matches; `dalforge lock
    -upgrade` rewrites it.
  - **Option hashes** cover each options file's descriptor without source
    info, so a vendored source copy and the embedded descriptor hash
    identically. A stale vendored copy is a DAL116 *warning*: generation
    continues with the bundled options.
- **No cgo:** dalforge must stay pure Go, so `go install` works without a C
  toolchain and release binaries cross-compile (`CGO_ENABLED=0`). The
  generator's dependencies (protocompile, protobuf, a YAML parser) and the
  runtime's (pgx) are pure Go, and any new dependency has to be too.

The lock deliberately holds no environment facts. `pg.version` stays in
`dalforge.yaml`.

## 3. Layering

Every concern sits in exactly one layer. A new backend implements the backend
layer, including all generated code. It never changes the IDL model.

| Layer | IDL | Generator | Generated code / runtime |
|---|---|---|---|
| **Core** (backend-neutral) | `dal.v1`: entity, fields, format, roles, shard key, access patterns, consistency | IR, core lint (DAL1xx) | runtime `dal`: `Page[T]`, `All`, `NewPage`, the page-token envelope and codec, sentinel errors, `Op`, retriers |
| **Backend** (pg today) | `dal.pg.v1`: physical types, SQL defaults, explicit/partial/covering indexes, index budget | access-path resolution and index derivation, physical model, backend lint (DAL2xx; DAL3xx planned), schema/query/sqlc emitters, DAL package emitter | the DAL package per proto package (repository interfaces, implementation, model aliases over sqlc's output, `WithTx`/`Tx`), runtime `dal/dalpg` (`DB` pools, `Runner`, SQLSTATE classification and error mapping) |

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
| transactions | generated `WithTx` over a pgx transaction | `TransactWriteItems`; a different model, not in the core interface |

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
  sharded by. dalforge checks that they exist and aren't repeated (DAL110).
- **Roles** mark columns the generated code manages:

| Role | Behaviour |
|---|---|
| `ROLE_CREATE_TIME` | on a Timestamp: set on insert, never updated |
| `ROLE_UPDATE_TIME` | on a Timestamp: set on insert and on every update |
| `ROLE_DELETE_TIME` | on an optional Timestamp: makes `Delete` a soft delete; reads exclude deleted rows (pg: derived indexes become partial `WHERE deleted_at IS NULL`) |
| `ROLE_VERSION` | on a required int64/int32: `Update` compare-and-swaps on it and increments it; a mismatch returns `dal.ErrVersionConflict` |

Each role appears at most once per entity and never on a key field (DAL119).

### Type mapping (Postgres)

The Postgres type comes from the proto type and `format`, unless
`(dal.pg.v1.column).type` or `custom_type` overrides it. The Go domain type
comes from the core layer and is the same for every backend.

| Proto (+ format) | Postgres default | Go domain type |
|---|---|---|
| `string` | `text` | `string` |
| `string` + `FORMAT_UUID` | `uuid` | `uuid.UUID` |
| `string` + `FORMAT_DECIMAL` | `numeric` | `string`: exact, verified round trip (a 30-digit value and NULL), no dependency |
| `string` + `FORMAT_JSON` | `jsonb` | `json.RawMessage` |
| `bool` | `boolean` | `bool` |
| `int32`, `sint32`, `sfixed32` | `integer` | `int32` |
| `int64`, `sint64`, `sfixed64`, `uint32`, `fixed32` | `bigint` | `int64` |
| `uint64` | lint error; requires `custom_type` | — |
| `float` / `double` | `real` / `double precision` | `float32` / `float64` |
| `bytes` | `bytea` | `[]byte` |
| `google.protobuf.Timestamp` | `timestamptz` | `time.Time` |
| enum | `text` (value name, by convention) | `string` (no generated constants or validation yet) |
| repeated scalar | `<type>[]` | `[]T` |
| message, map, `google.protobuf.Struct` | `jsonb` | `json.RawMessage` |

**Decimals (decided 2026-10-01)** default to `string`. A project that wants
a decimal library opts in per column with `custom_type: "numeric(12, 2)"` +
`go_type: "github.com/shopspring/decimal.Decimal"`. Insert parameters are
typed per SQL type, so all `numeric` columns then share that Go type (DAL213).
For money, the manual recommends `int64` minor units (cents), as the example
does.

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

The repository's own `compose.yaml` and `TestPostgresVersion` pin the same
target (`postgres:16.9`, major 16) for development; they're kept in step by
hand.

**How supported types evolve when more versions are supported:**

1. **The bundled `Type` enum is append-only.** It's the union of first-class
   types across every supported version. Values are never renumbered or
   removed. A breaking change would ship as `dal.pg.v2` next to v1.
2. **Version support is data in the generator, not the proto.** A capability
   table (`_minVersion` in `internal/backend/pg`) maps each type to the
   version it first appeared in. It's empty today, because every first-class
   type exists in Postgres 16; extension types are matched to their extension
   separately. Entries it would hold, for example:
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
      ON orders (account_id, created_at, id) WHERE deleted_at IS NULL;
    (stored ascending; the newest-first list reads it backwards)
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
   one index. The sort is marked `Source: inherited`, and that explicit index
   serves the list: no derived index is added next to it, even though it
   lacks the key tie-breakers (Postgres sorts the rare ties with an
   incremental sort, which is cheaper than a second near-identical index).
4. **The primary key.** It's deterministic, and it produces lint `DAL103` if the
   key isn't time-ordered. Time-ordered means a `FORMAT_UUID` key (dalforge
   assigns UUIDv7) or an integer; key fields pinned by `eq` are skipped, so
   `(tenant_id, id)` listed by `tenant_id` is judged by `id` alone. The
   results are stable but meaningless to users otherwise.

Whatever the source, the **keyset** is the sort keys followed by the key
fields not already sorted or pinned by `eq`, as tie-breakers in the
direction of the last sort key, so a single row comparison covers them.

### Get

`get` looks up by primary key by default. `by: [...]` looks up by other
columns, which must be covered by a `unique` column or a unique index,
otherwise it's lint error `DAL104`. A non-unique lookup is a List.

### Postgres index merging and budget

- **Derived index shape:** `(eq…, sort…, key tie-breakers)`, partial on
  `deleted_at IS NULL` for soft-delete tables, plus `range IS NOT NULL` for a
  nullable range column. The name spells out the columns and directions
  (`orders_status_created_at_id_idx`), and `schema.sql` notes the rpcs each
  derived index serves (`-- derived for: …`).
- **Canonical direction (decided 2026-10-02):** a derived index is stored with
  its first sort column ascending (the rest flipped with it); B-trees scan
  both ways. Lists sorting opposite ways then derive the identical index, so
  an index's shape and name depend only on its columns, never on rpc
  declaration order: adding or reordering other lists can't rename a
  deployed index. Pinned by `TestDerivedIndexesIgnoreDeclarationOrder`. The
  only remaining identity change is legitimate: a new, longer index
  absorbing a shorter one.
- **Prefix merging:** a derived index is dropped when another index (the
  primary key, an explicit index, or a longer derived one) starts with the
  same columns in the same directions or all reversed. B-trees scan both
  ways, so `ListOrdersByStatus` (`created_at DESC`) and
  `ListOrdersByStatusAndCreatedAt` (`created_at` ascending) share one index.
  The direction of equality columns never matters. A predicate must match, or
  the covering index must have none. A List on `account_id` sorted by `id`
  is not served by `(account_id, created_at, id)`: it needs
  `(account_id, id)`.
- **Equality order (planned):** reordering equality columns to maximise
  sharing, since their order inside the equality prefix doesn't affect
  correctness. Today they keep the `eq` order.
- **Budget:** each table has an index budget (default 5, overridable
  with `(dal.pg.v1.table).index_budget`), counting the primary key and unique
  indexes. Going over it produces lint warning `DAL201`, because every index
  adds write amplification and insert latency.

### Lint rules

The rules live in one place: `internal/rules/catalog/`, one Markdown file per
rule (severity, scope, title, explanation), embedded in the binary. The
manual's catalog ([`docs/manual/lint-rules.md`](manual/lint-rules.md)) is
generated from it, and a test keeps it and the rule constants in the code in
step, including each rule's severity. This section only explains how the IDs
are organised:

- **DAL1xx, core:** shapes and references that are wrong for any backend
  (range vs sort, nullable sort keys, request/response shapes, references,
  roles, list naming, mutable sort keys, shard keys).
- **DAL2xx, Postgres:** physical concerns (identifiers, type mapping, custom
  types and extensions, the deploy target, explicit indexes, the index
  budget, mixed sort directions, sqlc query-name and override conflicts).
- **DAL111–113 and DAL3xx, planned:** field-number identity and migration
  safety, which need the schema snapshot (§8). They're listed in the catalog
  as planned.
- **DAL4xx** is reserved for a DynamoDB backend.
- **DAL000** reports a generator bug (an emitter invariant), never a user
  mistake.

Cascades are suppressed: a query with a broken reference (DAL117) skips the
checks that would trip over the same typo, and a list that fails DAL202 gets
no DAL206 hint.

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
- **Consistency under concurrent writes: no snapshot.** Each page is its
  own statement (READ COMMITTED on Postgres), so an iteration doesn't see the
  table as of one moment. A snapshot would need a transaction held open
  across requests, or an exported snapshot / time-travel read, none of which
  fit stateless page tokens. What keyset pagination does guarantee:

  | Row during the iteration | Outcome |
  |---|---|
  | present throughout, sort columns unchanged | returned **exactly once**: no duplicates, no skips |
  | inserted, sorting behind the cursor | not returned (newest-first lists: new rows land here) |
  | inserted, sorting ahead of the cursor | returned |
  | deleted (or soft-deleted) | returned only if its page was already read |
  | sort column changed | may be returned twice or not at all (lint `DAL109`) |

  OFFSET pagination can't give even the first row of this table: an insert
  or delete before the offset shifts every later page, which is why dalforge
  never generates it. A caller that truly needs a consistent snapshot
  (exports, reconciliation) writes a custom query and reads in one
  `REPEATABLE READ` transaction.

Envelope: `{"v":1,"q":"<shape hash>","f":"<filter hash>","k":<backend payload>}`.
The core `dal` package owns the envelope, the hashing and the base64 encoding.
Each backend only encodes and decodes `k`.

### Postgres payload

`k` is the last row's keyset: the sort tuple plus the key tie-breakers. Each
List is two sqlc queries, the first page and the page after a cursor. For
`order_by: ["created_at DESC"]`:

```sql
-- name: OrderListOrdersByAccount :many
SELECT ... FROM orders
WHERE account_id = @account_id
  AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: OrderListOrdersByAccountAfter :many
SELECT ... FROM orders
WHERE account_id = @account_id
  AND deleted_at IS NULL
  AND (created_at, id) < (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;
```

- **Why two queries:** one query with `(@after IS NULL OR …)` is a shape
  Postgres plans poorly once a prepared statement switches to its generic
  plan. Two fixed shapes always use the index. The repository picks one by
  whether a page token was passed.
- **Range bounds** are `col >= sqlc.arg(<f>_from) AND col < sqlc.arg(<f>_to)`,
  both required.
- **Casts make cursor and bound parameters plain values:** they aren't traced
  to a column, so the generated `sqlc.yaml` adds a non-nullable `db_type`
  override next to each nullable one (which makes insert parameters
  pointers).
- **Tokens:** the repository decodes the page token before the retry loop
  (a bad token is the caller's error, `dal.ErrInvalidPageToken`) and checks it
  against the query shape (the sqlc query name plus the keyset) and a hash of
  the filter values, so a token can't be replayed against another list or
  other filters. Default page size 50, maximum 500, overridable per list.

- The repository fetches `page_size + 1` rows. The extra row only signals
  that another page exists; the token is built from the last row actually
  returned.
- Timestamps are encoded at full microsecond precision, so the cursor
  comparison is exact.
- A row comparison (`(a, b) < (x, y)`) only works when every sort column has
  the same direction. Mixed directions fall back to the expanded `OR` form
  (`(a < x) OR (a = x AND b > y)`) and produce lint warning `DAL203`.
- `dal.NewPage` builds the page from the `size + 1` rows and encodes the next
  token from the last returned row.

A DynamoDB backend would put `LastEvaluatedKey` in `k` and pass it straight
through as `ExclusiveStartKey`.

### Iteration in Go

Pages are the primitive, because they're what crosses an API boundary.
`dal.All` adapts any List method into a Go 1.23 iterator for in-process use:

```go
for o, err := range dal.All(ctx, shopdal.OrderListOrdersByAccountParams{AccountID: id}, orders.ListOrdersByAccount) {
	if err != nil {
		return err
	}
	// ...
}
```

### Planned: backward paging

Paging only goes forward: a page carries a `NextPageToken` built from its
last row, and there is no previous-page token. (Listing in the opposite
order is already a second rpc with the opposite `order_by`, sharing the same
index.) Backward paging is planned as an opt-in per list (for example
`list: {… bidirectional: true}`), because most API lists and every `dal.All`
iteration only go forward, and it costs a third query per list:

- **A `<Name>Before` query:** the cursor comparison flipped (`<` ↔ `>`) and the
  `ORDER BY` reversed, fetching `size + 1` rows that the repository reverses
  back into display order. It reads the same index the other way, so it adds
  no index.
- **Direction in the token:** the envelope gains a direction (its version
  field allows the change); pages gain a `PrevPageToken` built from their
  first row, empty on the first page.
- **Shapes:** the List response may carry `prev_page_token` (DAL107), and
  `dal.Page` gains the field.

**The consistency caveat matters more going backwards.** Postgres has no
time-travel reads (nothing like Oracle's `AS OF` or SQL Server's temporal
tables), and each page is its own statement, so every page sees the table as
of the moment it runs. Paging forward, the guarantees in "Contract" above
still hold for rows that don't change. Paging back and forth, the user can
see surprising pages:

- "next, then previous" can return a different page than the one already
  shown: rows inserted or deleted behind the cursor in between shift what
  the previous page holds;
- a previous page can come back shorter or fuller than before, or a page
  boundary can move, so the same row may appear at the end of one page and
  the start of the other;
- for newest-first lists, new rows land "before" the first page, so going
  back to the start shows rows that weren't there on the way forward.

Ways to narrow it, each with a cost, to decide when this is built:

- **Accept and document it**, as forward paging does today. This is usually
  fine for UIs that re-fetch.
- **An as-of bound in the token:** record the time of the first page and add
  `created_at <= @as_of` to every page, so later inserts stay out of the
  iteration. It needs a create-time column, ignores updates and deletes, and
  isn't exact: `now()` is the transaction start time, so a row created
  earlier but committed later can still slip in.
- **A real snapshot:** one `REPEATABLE READ` transaction, or
  `pg_export_snapshot()`, held open across requests. It's exact, but it ties
  a client session to a database connection and holds back vacuum, so it
  doesn't fit stateless page tokens. It's an option for exports, as a custom
  query, not for generated lists.

## 7. Generated Go API

For each proto package, dalforge generates one **DAL package** (e.g.
`shopdal` for `shop.v1`) next to sqlc's output. It contains:

- **model aliases:** `type Order = sqlcdb.Order`. sqlc's model structs are the
  entity types; there's no second domain model and no copy or mapping code;
- **per-store interfaces:** `OrderReadRepository`, `OrderWriteRepository` and
  `OrderRepository`, one method per rpc;
- **the implementation** over sqlc's `Queries`: pool routing, retries, page
  tokens, error mapping and the compare-and-swap follow-up;
- **parameter types:** for Create, Update and Upsert, aliases of sqlc's params
  structs (`OrderCreateOrderParams`); for List, dalforge's own struct with
  the filters, `PageSize`, `PageToken` and `WithPageToken`;
- **`WithTx` and `Tx`**, one per package, spanning all of its stores.

Services import only the DAL package. They never see sqlc's `Queries`, a
pool, SQL or page-token internals.

**Naming** (checked by compiling against sqlc's output):
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
  (`GetOrder(ctx, id uuid.UUID)`); more use sqlc's params struct, re-exported
  as an alias named `<Store minus "Store"><Method>Params`
  (`OrderUpdateOrderStatusParams`). Get, Create, Update and Upsert return the
  model; List returns `dal.Page[Model]`. Delete returns the model when the
  rpc does, otherwise just `error`.
- **Write params document what nil means** for every field, in the alias's
  doc comment.
- **Module path:** emitting Go needs the consuming module's path
  (`module` in `dalforge.yaml`) to import sqlc's output.

### From IDL to running code

The IR (§2) is the generator's internal model. It never ships. `dalforge
generate` reads it and writes SQL for sqlc plus the DAL package:

```
                 dalforge generate (reads IR)
                 │
     ┌───────────┴────────────────────────────┐
     ▼                                        ▼
SQL side (fed to sqlc)                 Go side (dalforge's own)
  schema/schema.sql                      gen/shop/v1/shopdal/
  queries/generated/*.sql  (DO NOT EDIT)   model aliases, Read/Write/Repository
  sqlc.yaml (type overrides, vet)          interfaces, implementation
     │
     ▼ sqlc generate                    queries/custom/*.sql  (user-owned,
  gen/sqlcdb/   Queries, models,          never touched by dalforge)
                OrderListOrdersByAccount(...)
```

At runtime, a call goes down these layers:

```
your service
  → shopdal.OrderReadRepository.ListOrdersByAccount(ctx, params)  // interface: models + plain Go types
  → shopdal implementation:
        picks reader/writer pool, wraps the call in the retrier,
        decodes the page token, fetches page_size+1 rows,
        builds next_page_token, maps errors to dal sentinels
  → sqlcdb.Queries.OrderListOrdersByAccount(ctx, args)              // sqlc-generated SQL call
  → pgx v5 pool → Postgres
```

```go
// package shopdal (generated from demos/orders)
type Order = sqlcdb.Order // sqlc's model; fields are uuid.UUID, time.Time, *string ...

type OrderReadRepository interface {
	GetOrder(ctx context.Context, id uuid.UUID) (Order, error)
	// The params struct is dalforge's own: the filters, PageSize, PageToken,
	// and WithPageToken for dal.All.
	ListOrdersByAccount(ctx context.Context, p OrderListOrdersByAccountParams) (dal.Page[Order], error)
}

type OrderWriteRepository interface {
	CreateOrder(ctx context.Context, p OrderCreateOrderParams) (Order, error)              // every field a pointer
	UpdateOrderStatus(ctx context.Context, p OrderUpdateOrderStatusParams) (Order, error) // version compare-and-swap
	DeleteOrder(ctx context.Context, id uuid.UUID) (Order, error)                         // soft delete
}

type OrderRepository interface {
	OrderReadRepository
	OrderWriteRepository
}

func NewOrderRepository(run *dalpg.Runner) OrderRepository // pools stay at the composition root

// WithTx runs fn in a transaction on the writer pool. Tx is the DAL's own
// type, never pgx.Tx: it hands out each store's repository bound to the
// transaction, and sqlc's Queries for custom queries.
func WithTx(ctx context.Context, run *dalpg.Runner, fn func(ctx context.Context, tx Tx) error) error

func (tx Tx) Order() OrderRepository
func (tx Tx) Queries() *sqlcdb.Queries
```

One `WithTx` and `Tx` per DAL package (proto package), covering all of its
stores, so a transaction can span them, as with Account and Order in the
example.

### No driver types in the API

pgx is the driver layer, so no `pgx`, `pgtype` or `pgconn` type appears in
any exported signature or model field of the DAL package:

| Leak | Prevention |
|---|---|
| sqlc model fields (`pgtype.UUID`, `pgtype.Timestamptz`, `pgtype.Text`, `pgtype.Numeric` …) | the generated `sqlc.yaml` has a per-column override for **every** column: `uuid.UUID`, `time.Time`, `string` for `numeric` and `inet`, pointers for nullable columns, `json.RawMessage`, …. A `custom_type` must declare its `go_type` |
| transactions (`pgx.Tx`) | `WithTx` hands out the DAL's own `Tx` |
| errors (`pgx.ErrNoRows`, `*pgconn.PgError`) | mapped to `dal` sentinels; the driver error is only the wrapped cause |
| pools (`*pgxpool.Pool`) | only at the composition root, when constructing `dalpg.DB{Reader, Writer}` |

Tests enforce this on the golden output: generated DAL files may not import
pgx, pgtype or pgconn in their exported API, sqlc's generated Go may not
mention `pgtype`, and a `go list -deps` check keeps protobuf out of the IR,
the checks and the backend. The runtime is a separate module whose `go.mod`
has no protobuf at all.

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
| required, with default | the column default | SQL: `COALESCE(sqlc.narg(status)::text, 'pending')` |
| `optional`, no default | `NULL` | SQL |
| `optional`, with default | the column default, so Create can't insert NULL (DAL212 info; Update can) | SQL |
| primary key with `FORMAT_UUID` | the DAL assigns a UUIDv7 | the DAL |
| role fields | not in the params | — |

**How the generated SQL gets sqlc to produce pointers** (established by
running sqlc on the generated SQL; `TestSQLCGenerate` pins it):
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
o, err := orders.CreateOrder(ctx, shopdal.OrderCreateOrderParams{
	AccountID:   new(acct),
	AmountCents: new(int64(1999)),
	Note:        new("rush"), // Status omitted → 'pending'
})
```

**Updates follow the same rule (decided 2026-10-01).** Every field an update
sets is a pointer (`SET status = sqlc.narg(status)::text`):
- nil on a required field is `ErrMissingField`, before any database call;
- nil on an optional field sets NULL;
- the key and version stay plain values, because they identify the row.

Rejected alternatives:
- **PATCH (nil = unchanged)** can't clear an optional field through the
  generated update. It also turns a forgotten field into a silent no-op, and
  gives nil a different meaning than in Create.
- **Plain values** silently overwrite a forgotten field with its zero value
  (`""`, `0`, `false`, the zero UUID), which `NOT NULL` can't catch.

The remaining caveat: forgetting an *optional* field clears it. Updates are
narrow by design (each rpc lists its columns), and the generated doc on each
params type lists what nil means per field.

### The generated implementation

`repository.go` in each DAL package implements the interfaces.
- **One Runner call per method, around one sqlc call.** The Runner owns
  routing, retries, error mapping and the per-attempt context.
- **A `dal.Op` per rpc is fixed at generation time**, with idempotency as in
  "Errors and retries" below: Get, List, Delete and Upsert are idempotent,
  Create isn't, and Update is unless it's a version compare-and-swap.
- **Create** returns `MissingFieldError` for a nil required field before any
  database call, and gives a nil UUID primary key a UUIDv7 *before* the retry
  loop, so retries reuse the same ID.
- **A versioned Update** that changes zero rows runs the generated
  `<Store>Exists` query on the same connection, only on that failure path,
  and returns `ErrVersionConflict` (row exists) or `ErrNotFound`.
- **Delete without an entity response** uses `:execrows`, and zero rows
  returns `ErrNotFound`.
- **No pgx imports in generated code:** `dalpg.NewError` and `dalpg.IsNoRows`
  cover what it needs.
- **List** decodes the token, runs the first-page or `…After` query for
  `size + 1` rows through `Runner.Read`, and builds the page with
  `dal.NewPage`.
- **`WithTx`** wraps `Runner.InTx`; `Tx.Queries()` uses the transaction's
  connection (`Runner.Conn`).

Proven end to end by the example project (`mise run demo`, §11): generate →
sqlc → compile → the app exercises every access pattern against Postgres.

### Writes: basic and safe, the rest is custom

Generated writes cover the common footguns and nothing more:

- **Update with a `ROLE_VERSION` field** is always a compare-and-swap:
  `SET …, version = version + 1 … WHERE id = @id AND version = @version`. If
  zero rows are updated, a follow-up existence check (only on that failure
  path) tells `ErrNotFound` from `ErrVersionConflict`.
- **Upsert** targets the primary key or a unique `conflict_on`, and the last
  writer wins: the version is bumped, with no compare-and-swap, so it stays
  idempotent for retries. Its parameters follow the Create rules (pointers,
  defaults, missing-field checks, UUIDv7). It never revives a soft-deleted
  row: with a key target, `DO UPDATE … WHERE deleted_at IS NULL` skips it and
  the DAL returns `ErrAlreadyExists`. With a unique-field target, the
  conflict names the partial unique index (`ON CONFLICT (email) WHERE
  deleted_at IS NULL`), so a soft-deleted row doesn't conflict and a new
  live row is inserted.
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
  implementation calls:
  - `Read(ctx, op, strong, fn)` routes to the reader, or the writer when the
    read is strong or there's no reader. `Write(ctx, op, fn)` uses the
    writer.
  - `InTx(ctx, op, fn)` runs on the writer and hands `fn` a
    transaction-bound Runner, whose statements are never retried on their
    own; the whole transaction is retried by the policy. `InTx` on a
    transaction-bound Runner joins that transaction. `Conn()` exposes the
    transaction's connection, which `Tx.Queries()` uses.
  - Every failure becomes a classified `*dal.Error` before the retrier sees
    it. `fn` receives a `dalpg.DBTX`, which has the same method set as sqlc's,
    so `sqlcdb.New(q)` works on a pool or a transaction alike.
  - **Pools are an interface you can wrap.** `dalpg.DB{Reader, Writer}`
    takes `dalpg.Pool` (the sqlc-compatible `DBTX` plus `Begin`), which
    `*pgxpool.Pool` satisfies, and resolves it on every operation. A wrapper
    can therefore swap the underlying pool at runtime without touching
    generated code: an Aurora blue/green switchover, tracing, or per-tenant
    pools. In-flight transactions finish on the connection they began on.
    Detecting the switchover is the wrapper's job, not dalforge's.
  - **Observability with no wrappers.** Every attempt runs with a context
    carrying the `dal.Op` and the attempt number (`dal.OpFromContext`). The
    Runner counts attempts itself, so this holds for any retrier.
    - Inside `InTx`, statements carry their own op and the transaction's
      attempt; BEGIN, COMMIT and ROLLBACK carry the transaction's op.
    - pgx tracers (`ConnConfig.Tracer`, set on the pool at the composition
      root) therefore label spans and metrics by repository method and tell
      retries apart. sqlc's `-- name:` comment in each statement also names
      the query.
    - Generated code does nothing for this, and no proxy closures around
      repository calls are needed.
  - `dalpg` depends only on pgx (plus `dal`). The generated DAL package's
    *exported* API still exposes no pgx types; `DBTX` is only used inside the
    generated implementation.
- **Runtime libraries** are imported, not generated, so bug fixes don't need
  a regeneration. They're their own Go module,
  `github.com/gisripa/dalforge/dal`, released in lockstep with the CLI (§2):
  - `dal`: `Page[T]`, `All`, `NewPage`, `PageSize`, the page-token codec
    (`EncodeToken`, `DecodeToken`), sentinel errors, `Op` and its context
    helpers, `Retrier`, `Backoff`, `Retryability`, `ShouldRetry`
  - `dal/dalpg`: `DB`, `Pool`, `Runner` (`Read`, `Write`, `InTx`, `Conn`),
    options (`WithRetrier`, `WithClassifier`), the SQLSTATE classifier and
    error mapping (`Classify`, `NewError`, `IsNoRows`)

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
| `WithTx` | no | the transaction is retried as a whole, never statement by statement, and only for errors Postgres rolled back (serialization failure, deadlock), which re-runs `fn`; keep side effects outside the database out of `fn` |

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
  `dal.ShouldRetry` works as its retry predicate. dalforge ships no adapter, so
  the runtime doesn't take on the dependency.
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

> **Planned.** The contract in this section is the design; the snapshot,
> `dalforge migrate`, DAL111–113 and DAL3xx aren't implemented. Until they
> are, users write migrations by hand following the same rules
> (`docs/manual/schema-changes.md`), and derived index names are stable by
> construction (§5) so migrations don't churn on them.

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
dalforge.yaml               # module, proto roots, backend, pg.version (deploy target), output dirs
proto/shop/v1/shop.proto
dalforge.lock               # committed; toolchain pin (dalforge version, options hashes, sqlc)
dalforge.snapshot.json      # (planned) committed; source of truth for migration diffs
migrations/                 # (planned) generated golang-migrate files, append-only, reviewed
schema/schema.sql           # generated desired state for a new database (also the sqlc schema input)
queries/generated/*.sql     # generated; "Code generated by dalforge. DO NOT EDIT."
queries/custom/*.sql        # handwritten sqlc queries, never touched by dalforge
sqlc.yaml                   # generated
gen/sqlcdb/                 # sqlc output (both query sets)
gen/shop/v1/shopdal/        # DAL package: model aliases, interfaces, implementation, WithTx
```

Generated query files hold one file per table, named `<table>.sql`. Each query
is named `<Store minus "Store"><Method>` for sqlc, lists its columns
explicitly, and uses proto field names as parameters (`@account_id`). Unique
fields on soft-delete tables become partial unique indexes
(`WHERE deleted_at IS NULL`). `sqlc.yaml` carries one type override per
column, per-type overrides for cast parameters (nullable ones for write
parameters, plain ones for cursor and range bounds), and Go initialisms
(`id`, `ip`, `url`, `uri`, `uuid`, `http`, `json`, `api`, `sql`) so fields read
`AccountID`, `ClientIP`.

**When there are no queries at all** (entities but no stores, and no custom
queries), sqlc refuses to run. `dalforge generate` then skips sqlc with a
note rather than failing.

Generated and custom queries go through **one** sqlc config, so they share a
`Queries` type and custom queries can run inside the same transactions.

The generated `sqlc.yaml` turns on sqlc's safety features:

- `sql_package: pgx/v5`, `emit_interface`, `emit_pointers_for_null_types`,
  `emit_exact_table_names` (plus `rename` so models are named after messages)
- `strict_function_checks`, `strict_order_by`
- per-column type overrides for every column, so no `pgtype` wrapper reaches
  the models or parameters

*Planned:* `sqlc vet` with a database URI (`sqlc/db-prepare`) and opt-in CEL
rules, such as rejecting `OFFSET` in custom queries.

### `dalforge.yaml` and the CLI

```yaml
version: 1
module: github.com/acme/shop   # Go module of this project (imports of generated code)
backend: pg
proto:
  roots: [proto]               # import paths; default [proto]
  files: ["orders/v1/*.proto"] # globs under the roots; default: every .proto
pg:
  version: "16.9"              # deploy target (DAL207); default 16
out:                           # defaults shown
  schema: schema/schema.sql
  queries: queries/generated
  custom: queries/custom       # user-owned; never written
  sqlc: gen/sqlcdb             # its last element is sqlc's Go package name
  dal: gen
```

**Validation:**
- Unknown keys are errors, so a typo can't silently do nothing.
- `module` is required.
- Every path must stay inside the project.
- `out.custom` may not overlap any generated directory.
- Vendored copies of the dal options are never treated as inputs (DAL116
  warns when one differs from the bundled options).

**Commands:**
- **`dalforge lint [-config dalforge.yaml]`** runs load → check → build →
  emit without writing anything. It prints findings to stderr and exits 1 on
  errors; it's silent on a clean IDL.
- **`-json`** (lint and generate) prints one report on stdout instead: the
  findings (load errors included, without a rule), what generate wrote, and
  any other failure. Its field names are a stable interface: the playground,
  CI annotations and editors read it instead of importing dalforge's
  internals. Human-facing text stays on stderr; results for tools (the
  report, `dalforge version`) go to stdout.
- **`dalforge generate`** runs the same pipeline, then:
  - writes only changed files, so mtimes stay stable;
  - removes stale generated files, but only files headed `Code generated by
    dalforge` or `by sqlc`, and never anything under `out.custom`;
  - runs sqlc, clearing sqlc's old output first, since sqlc never deletes
    files for removed queries;
  - skips sqlc with a note when there are no queries at all;
  - reports custom queries whose `-- name:` collides with a generated one
    (DAL205) before sqlc runs.
- **`dalforge lock [-upgrade]`** reports, or rewrites, `dalforge.lock` (§2).
- **`dalforge version`** prints the running version.
- **`dalforge migrate`** is reserved for generated migrations (§8, planned).
- **Cascades are suppressed:** a query with a broken reference (DAL117)
  doesn't also report a request-shape error (DAL107), so a single typo yields
  a single error.

## 10. Testing

| Suite | Where | Runs with |
|---|---|---|
| Unit | `*_test.go` next to the code, in both modules (generator and `dal/`) | `mise run test` (part of `mise run check`) |
| Golden | generator output compared with `testdata/**/*.golden` via `internal/golden`: the model, every emitted file, sqlc's own output for the fixtures (`TestSQLCGenerate`), and one fixture per lint rule. `mise run test:update` rewrites them (`DALFORGE_UPDATE_GOLDEN=1`, refused when `CI` is set) | `mise run test` |
| Generated docs | `internal/docgen` fails when a generated page of the manual is stale (`mise run docs` regenerates); `internal/rules` checks the rule catalog against the code; `internal/manualtest` checks links | `mise run test` |
| Integration | `//go:build integration` tests that need Postgres: `dal/dalpg` (real SQLSTATE mapping, a real `40001` from conflicting serializable transactions, a real admin shutdown `57P01`, reader/writer routing, transactions, the pgx tracer) and `internal/integration` (server version, lossless `numeric` round trip) | `mise run test:integration` (runs `vm:up` and `db:up` first) |
| End to end | `demos/orders`, run like a user's project: generate → sqlc → build → a demo app exercising every access pattern against Postgres | `mise run demo` |
| Playground | every preset generates cleanly (the mistakes preset reports exactly its rules), sqlc accepts every clean preset's output with no `pgtype`, load errors come back positioned, and the HTTP API works | `mise run test` |

`mise run check` stays fast and Docker-free. CI (`.github/workflows/ci.yml`)
runs `check` in one job and `test:integration` plus `demo` in another.

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
    used; the same `compose.yaml` and tasks run there.
- `mise.toml` exports `DALFORGE_TEST_DATABASE_URL`.
- `pgtest.New(t)` creates a throwaway database per test and drops it
  afterwards, so tests can run in parallel. Each module has its own copy
  (`internal/pgtest`, `dal/internal/pgtest`), since a module can't import
  another module's internal packages.
- `TestPostgresVersion` fails if the server isn't on major version 16.

**Why containers rather than embedded Postgres:** DALForge is meant to go
beyond Postgres. DynamoDB Local, LocalStack, and images with extensions such
as pgvector are containers, and Toxiproxy (for injecting connection drops and
latency) fits the same compose file. Embedded Postgres would be a
Postgres-only path. Aurora-specific behaviour, such as failover, isn't
reproduced locally; the retry paths are tested by provoking the real errors.

**Keeping the suite light:** generated code is compiled and run by the demo
rather than by tests that build throwaway Go modules, which were slow on a
laptop. *Planned:* a pagination-contract test under concurrent inserts, and
rolling-deploy tests (schema N+1 against the code for N and N+1) with
migrations.

## 11. Demos

Everything demo-only lives under `demos/`, and each demo is its own Go module
that uses dalforge exactly as a user does: through the `dalforge` CLI and the
released runtime module, never through `internal/` packages. That keeps the
generator's internals private, and lets the demos move to a repository of
their own without changes. For now they also serve as the end-to-end tests
(`mise run demo` in CI).

### Orders

`demos/orders/` is a standalone Go module that consumes DALForge the way a
user would: an IDL (`proto/shop/v1/shop.proto`), `dalforge.yaml`, custom
queries in `queries/custom/`, and an app (`main.go`). Everything dalforge and
sqlc generate is gitignored, including `dalforge.lock`, because the example
always runs the dalforge built from the same checkout; a real project
commits its lock. `mise run demo` generates and runs it, and it's the
end-to-end test in CI; `mise run example` only generates and builds it, with
no database.

**Its `go.mod` pins the released runtime** (`dal vX.Y.Z`), with no `replace`
directive, so the directory is a copyable, self-contained project. The
`example` and `demo` tasks add a gitignored `go.work` that points the runtime
at `../../dal`, so CI and local runs test the generator and the runtime from
the same checkout: a change spanning both never has to be released before it
can be tested. The pin is bumped after each release (RELEASING.md).

It models **`Account`** and **`Order`** (`Order.account_id` refers to
`Account.id`) and plays out a shop's day with gofakeit data, checking every
outcome and exiting non-zero on any surprise:

- **CRUD and writes:** UUIDv7 keys, a unique lookup, a duplicate, an upsert by
  email, missing required fields, compare-and-swap with a stale version, soft
  delete, and a custom report query.
- **The ESR ladder from §5:** `ListOrdersByAccount`, `ListOrdersByStatus`,
  `ListOrdersByStatusAndCreatedAt` and
  `ListOrdersByAccountAndStatusAndCreatedAt`, with paging through page
  tokens, `dal.All`, a token rejected for other filters, and a time-window
  range list. The four lists derive three indexes (two share one, read in
  opposite directions), and the app prints them with the rpcs each serves.
- **Application-level referential integrity, without an FK:** orders are
  placed in `WithTx`, which first runs the custom query
  `SELECT id FROM accounts WHERE id = @id FOR KEY SHARE`
  (`queries/custom/accounts.sql`). That fails fast for a missing account and
  blocks a concurrent account delete until commit, which is the race
  hand-written checks usually miss. Then it calls the generated create in the
  same transaction. Without an FK, an order for a missing account would
  otherwise *succeed*, and the orphan would surface later, far from the
  cause.

The quickstart (`docs/manual/quickstart.md`) is the guided tour of this
example: what's generated, adding a list, and tripping the linter.

### Playground

`mise run playground` serves a local page (`demos/playground`) on
`127.0.0.1:7070` for trying any IDL: a proto editor, the lint findings (click
one to jump to its line), and every generated file, dalforge's and sqlc's,
regenerated as you type. Presets cover basics, roles, Postgres types, lists
and indexes, and a file of deliberate mistakes.

- **The real CLI, server-side:** each request writes the source into a
  throwaway project and runs `dalforge generate -json` there, which also
  runs sqlc, then reads back the findings and every file written. Nothing is
  reimplemented for the page, so it can't drift from what users get, and it
  shows which dalforge version it's running.
- **Local only:** it listens on loopback and runs sqlc on whatever the page
  sends, with a size limit and a timeout per request. It's a tool for people
  with the repository checked out, not a hosted service.
- **Works offline:** one embedded HTML file with plain JavaScript, plus
  vendored, embedded highlight.js (BSD-3-Clause, with its protobuf grammar
  and GitHub light/dark themes) for syntax coloring. The editor colors its
  text by layering a transparent textarea over a highlighted copy of it.
- **Not in the CLI:** it's a separate module and command, so the released
  `dalforge` binary stays a generator. Its only dependency is the standard
  library.

## 12. Documentation

Because DALForge introduces its own IDL, the **user manual** (`docs/manual/`)
is a first-class deliverable. This design doc records decisions and their
reasons; the manual teaches users every option, access pattern, combination,
limitation, type mapping and lint rule, without relying on `demos/`.

**Generated where it must match the code.** The parts
that drift are generated, and `check` fails while they're stale
(`internal/docgen`; `mise run docs` regenerates):
- the **rule catalog** (`lint-rules.md`) from `internal/rules/catalog/*.md`,
  one Markdown file per rule with a header (severity, scope, title,
  planned). `internal/rules` tests that the catalog and the rule constants
  match both ways, and that the severity each `diags.Add` site uses is the
  documented one. The files are embedded in the binary, ready for a future
  `dalforge lint -explain`;
- the **option reference** (`options.md`) from the comments in the options
  protos, which are therefore the source of truth;
- the **type tables** in `types.md`, spliced between `<!-- generated:… -->`
  markers, by building a probe IDL through the real loader and pg backend.

The narrative chapters are hand-written; `internal/manualtest` checks that
every relative link and anchor in the manual resolves.

Planned: complete IDL snippets in the manual compile; diagnostics link to
their rule entry.

The manual has a quickstart (run the demo, tour the generated files, change
an access pattern, trip the linter, run the tests) and chapters on concepts,
project setup (including the generator/runtime split and upgrades), the IDL,
options, types, access patterns, lists, the Go API, schema changes, lint
rules and limitations. Every change updates the sections it affects.

## 13. Roadmap

1. **CRUD by key:** IR loader, core/backend split, type mapping, `schema.sql`,
   Get/Create/Update/Delete queries, `sqlc.yaml` with full type overrides,
   the DAL package (model aliases, interfaces, implementation), the `dal` and
   `dal/dalpg` runtimes, and the no-driver-types API guard.
   Error classification and the pluggable retrier. Integration tests against
   the compose Postgres 16.9. First cut of
   `demos/orders`.
2. **List and indexes:** access paths, sort resolution, keyset List with page
   tokens and `dal.All`, index derivation and merging, the DAL1xx/DAL2xx lint
   rules. Upsert, soft delete, optimistic locking, and `WithTx`. **Done
   (2026-10-01).**
   **v0 distribution (2026-10-02):** user manual with generated parts, the
   runtime as its own module, CI, GoReleaser releases, and stable derived
   index names, so v0 is usable for new databases before migrations exist.
3. **Migrations:** an adoption baseline for schemas deployed with v0,
   snapshot, diff, the additive contract with retired columns, DAL3xx rules,
   golang-migrate file emission.
4. **Extensions:** `custom_type` ergonomics (pgvector, PostGIS), buf plugin
   mode, the Atlas backend, sqlc vet rule packs, shard-key enforcement, and a
   DynamoDB backend.

## Decision log

| Topic | Decision |
|---|---|
| Generator packaging | Standalone CLI embedding protocompile; buf plugin later |
| Domain types | sqlc's model structs, re-exported as type aliases from the generated DAL package. No separate domain model and no copy layer (revised 2026-09-30; originally generated structs). Proto is only the IDL |
| Observability | Tracing and metrics come from pgx's own tracers, configured on the pool. The Runner puts `dal.Op` and the attempt number on every attempt's context (`dal.OpFromContext`), so tracers see the repository method and retries without wrappers. Generated code stays out of it |
| Pool injection | `dalpg.DB` takes the `dalpg.Pool` interface (not `*pgxpool.Pool`), resolved per operation; generated repositories take a `*dalpg.Runner`. Users can wrap pools (blue/green swap, tracing, tenancy) without editing generated code |
| Driver isolation | No `pgx`/`pgtype`/`pgconn` types in the DAL package's exported API: full sqlc type overrides, the DAL's own `Tx`, errors mapped to `dal` sentinels, pools only at the composition root. Enforced by a `go/types` test |
| Update parameters | Same rule as Create (decided 2026-10-01): SET fields are pointers, nil required → `ErrMissingField`, nil optional → NULL; key and version stay values. PATCH and plain values were rejected (see §7) |
| Decimal Go type | `string` by default (verified lossless); a decimal library is opt-in via `custom_type` + `go_type`; money as int64 cents is the recommendation |
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
| Testing | Unit + golden + generated-docs freshness in `mise run check`; `integration` build tag against a compose `postgres:16.9` in `mise run test:integration`; the demo as the end-to-end test; CI runs all three |
| List shapes | One fixed shape per rpc: required equality params, at most one range (half-open, both bounds required) as the last column; each filter combination is its own rpc and index (ESR ladder); naming lint DAL114 (warn); nullable sort columns only as the range column (DAL115) |
| List SQL | Two sqlc queries per List (first page, `…After` cursor) rather than one with `@cursor IS NULL OR …`, so generic plans keep using the index (2026-10-01) |
| Explicit indexes serve lists | A list whose eq + sort columns lead an explicit index is served by it, even without the key tie-breakers; no near-duplicate derived index (2026-10-01) |
| Transactions | One `WithTx`/`Tx` per DAL package: repositories of every store bound to the transaction, plus `Queries()` for custom queries. Not idempotent: retried as a whole (re-running `fn`) only when Postgres rolled it back, never after an ambiguous failure (2026-10-01) |
| Referential integrity | Not generated. Demonstrated in `demos/orders` (Account ↔ Order) as an application pattern: `WithTx` + `FOR KEY SHARE` + custom sqlc queries. Generated opt-in checks are a possible later addition |
| PG version & types | The IDL declares requirements (`(dal.pg.v1.file)`: `min_version`, `extensions`); `dalforge.yaml` declares the deploy target (`pg.version`). The `Type` enum is append-only. Per-version and per-extension availability lives in a generator capability table, checked by DAL207/DAL208. `custom_type` requires `go_type` |
| Distribution | GoReleaser binaries on GitHub Releases for each `vX.Y.Z` tag (linux/darwin × amd64/arm64), installed with mise's `github:` backend or `go install`; the version is stamped via ldflags or read from build info. CGO-free (2026-10-02) |
| Runtime module | `github.com/gisripa/dalforge/dal` is its own Go module (pgx only), released in lockstep as `dal/vX.Y.Z`, so users don't pull the generator's dependencies (2026-10-02) |
| Generated docs | The rule catalog, option reference and type tables in the manual are generated from code and checked for staleness; narrative chapters are hand-written (2026-10-02) |
| Derived index identity | Derived indexes are stored in a canonical direction (first sort column ascending), so their names depend only on their columns, never on rpc order (2026-10-02) |
| sqlc pinning | A prebuilt binary through mise in the consuming repo, checked against `dalforge.lock`; replaces `go tool sqlc` (cgo) |
| Toolchain pinning | `dalforge.lock` (TOML: dalforge version, options hashes, sqlc version), committed. Mismatches fail until `dalforge lock -upgrade`. Separate from the schema snapshot. Per-PG-version option sets were rejected |
| sqlc's schema | Exactly what the IDL declares; retired columns exist only in the physical schema and snapshot. The earlier "schema view without deprecated columns" proposal was dropped together with `STATE_DEPRECATED` |
| Container runtime | Colima via mise (dedicated `dalforge` profile, repo-scoped `DOCKER_HOST`); native Docker in CI. Chosen over embedded Postgres so that future backends and fault-injection proxies share one mechanism |
| Retries | SQLSTATE classification into Retryable / RetryableIfIdempotent / NotRetryable, plus per-op idempotency; pluggable `dal.Retrier` (closure-friendly) and classifier; built-in exponential backoff default; libraries such as failsafe-go plug in through `dal.RetrierFunc` |
| Onboarding | The quickstart plus `demos/orders`, a standalone consumer module run end to end by `mise run demo` (and in CI); its generated output is gitignored so a reader sees exactly what the current IDL produces |
| Example dependencies | The example's `go.mod` pins the released runtime (no `replace`); a gitignored `go.work`, written by the tasks, builds it against the in-repo runtime (2026-10-04) |
| Playground | A localhost page in its own module (`demos/playground`), running `dalforge generate -json` (and so sqlc) server-side per request; presets, no sharing or hosting (2026-10-04) |
| Demos layout | Demo-only code lives in `demos/`, each demo its own module that uses dalforge only through the CLI and the released runtime, never `internal/`, so the demos can move to their own repository unchanged. They double as the end-to-end tests for now (2026-10-04) |
| Machine-readable CLI output | `dalforge lint/generate -json` print one report on stdout (findings, what was written, other failures); text for people stays on stderr. Tools use it instead of a programmatic Go API, which isn't offered (2026-10-04) |
