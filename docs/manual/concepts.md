# Concepts

## The idea

Most data-access bugs aren't about SQL syntax. They're about the things a
query should have done and didn't:

- a list without an index, which is fast in development and slow in
  production;
- `OFFSET` pagination that skips or repeats rows;
- a soft-deleted row that leaks back into a read;
- a lost update, because nothing checked the version;
- a column default that never applies, because the insert always sends a
  value;
- a new `NOT NULL` column that breaks the release still running during a
  rolling deploy.

dalforge moves those decisions out of individual queries and into the
generator. You declare **what** each access pattern is: "orders of an
account, newest first". dalforge decides **how**: the index, the keyset
query, the filters and the checks. It also refuses patterns it can't make
safe.

## The pieces

| Concept | In the IDL | Becomes |
|---|---|---|
| **Entity** | a message with `(dal.v1.table)` | a table, and a Go model type |
| **Field** | a message field, optionally with `(dal.v1.field)` / `(dal.pg.v1.column)` | a column, and a model field |
| **Store** | a service with `(dal.v1.store) = {entity: "…"}` | a set of repository interfaces |
| **Access pattern** | an rpc with `(dal.v1.query)` | a sqlc query (two for a list), a repository method, and possibly an index |

Each rpc declares exactly one pattern: `get`, `list`, `create`, `update`,
`delete` or `upsert`. Its request and response messages must match the
pattern (rule [DAL107](lint-rules.md#dal107)), so the proto stays an
accurate, readable description of your data access.

The proto is **only an IDL**. Nothing at runtime uses protobuf: the generated
Go code works with plain structs (`uuid.UUID`, `time.Time`, pointers for
nullable columns). Don't confuse these services with gRPC services; you
never implement or serve them.

## What's yours, what's generated

| Yours (committed) | Generated (gitignore it) |
|---|---|
| `proto/**.proto`: the IDL | `schema/schema.sql`: tables and indexes |
| `dalforge.yaml`: project config | `queries/generated/*.sql`: one sqlc query per pattern |
| `dalforge.lock`: pinned toolchain | `sqlc.yaml`: sqlc config with every type override |
| `queries/custom/*.sql`: your own sqlc queries | `gen/sqlcdb/`: sqlc's Go output |
| your application code | `gen/<pkg>/<ver>/<pkg>dal/`: the DAL package |

`dalforge generate` recreates everything in the right-hand column, and it
never touches `queries/custom/`. Generated files carry a `DO NOT EDIT`
header, and stale ones (for a removed rpc, say) are deleted automatically.
Whether you commit generated output is your choice. The example ignores it,
so you always see what the current IDL produces.

The `dalforge` CLI that does the generating is a development tool, pinned in
`mise.toml`. The code it generates imports a small runtime library, pinned in
your `go.mod` at the same version. See
[the generator and the runtime](project-setup.md#two-pieces-the-generator-and-the-runtime).

## Where sqlc fits

dalforge doesn't replace sqlc; it drives it. The generated queries are
ordinary sqlc queries, and the models are sqlc's structs, re-exported as type
aliases rather than copied. Your hand-written queries in `queries/custom/`
are compiled by the same sqlc run into the same `Queries` type. They're the
escape hatch for anything dalforge doesn't generate: joins, aggregates,
state-machine guards, bulk updates.

What dalforge adds on top of sqlc:

- the schema and indexes, derived from the access patterns;
- the queries that are easy to get subtly wrong: keyset pagination, soft
  delete, compare-and-swap, defaults;
- a DAL boundary: interfaces split into read and write sides, with no pgx
  types and errors mapped to `dal` sentinels;
- reader/writer routing, retries that know which operations are safe to
  repeat, and operation labels for tracing;
- a linter that explains why a pattern is unsafe.

## Read and write sides

Every store becomes three interfaces:

```go
type OrderReadRepository interface { … }  // Get and List
type OrderWriteRepository interface { … } // Create, Update, Delete, Upsert
type OrderRepository interface { OrderReadRepository; OrderWriteRepository }
```

A reporting service can depend on `OrderReadRepository` alone, so it provably
can't write. Reads go to a reader pool unless an rpc asks for
`CONSISTENCY_STRONG`; writes always go to the writer pool.

## What dalforge deliberately doesn't do

- **Joins and ad-hoc queries.** Write them as custom sqlc queries.
- **Foreign keys.** They make sharding and online migrations hard. Referential
  integrity is an application pattern (see [The Go API](go-api.md#referential-integrity)).
- **Optional filters.** A list with "maybe filter by status" can't have one
  good index. Each filter combination is its own rpc
  ([why](lists.md#one-shape-per-rpc)).
- **`OFFSET` pagination.** Never generated.
- **Destructive schema changes.** Removing a field retires its column rather
  than dropping it ([Changing the schema](schema-changes.md)).
- **An ORM.** There's no lazy loading, no relationships and no query builder.

See [Limitations](limitations.md) for the full list, with an escape hatch
for each.
