# Limitations

dalforge covers the common, single-entity access patterns and makes them
safe. Everything else has an escape hatch, usually a
[custom query](go-api.md#custom-queries), which runs through the same sqlc
setup and type overrides.

| Not supported | Why | Instead |
|---|---|---|
| Joins, aggregates, reports | not single-entity access patterns | a custom query |
| Optional filters in a list | no single index serves every combination | one list rpc per combination ([why](lists.md#one-shape-per-rpc)) |
| `OFFSET` pagination | skips and repeats rows under writes | keyset pagination (generated) |
| A consistent snapshot across pages | each page is its own query | a custom query in a `REPEATABLE READ` transaction |
| More than one `range` per list | an index serves one range | filter the rest in Go, or a custom query |
| Guarded updates ("only if still pending"), increments | business rules, not generic ones | a custom query |
| Batch inserts and updates | out of scope for now | a custom query (sqlc's `:copyfrom` for bulk inserts) |
| Foreign keys | they complicate sharding and online migrations | check in a transaction with `FOR KEY SHARE` ([pattern](go-api.md#referential-integrity)) |
| Sequences and identity columns | client-side UUIDv7 keys are the default | supply integer keys yourself |
| `uint64`, `fixed64` | no Postgres type holds the range | `int64`, or a `custom_type` such as `numeric(20, 0)` |
| `oneof` in an entity | no clean column mapping | separate `optional` fields |
| Well-known types other than `Timestamp`, `Struct`, `Value`, `ListValue` | | a plain field with `optional`, or a message stored as `jsonb` |
| proto2 and editions | | `syntax = "proto3"` |
| Streaming rpcs | | |
| Enum constants and validation in Go | enums are stored as `text` and typed `string` | your own constants, or the protobuf-generated enum's `String()` |
| Choosing a transaction's isolation level per call | `WithTx` uses the connection's default (`READ COMMITTED`) | a separate writer pool whose connections default to another level: `cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "serializable"`. Serialization failures are retried |
| Dropping retired columns, tables and indexes | not additive | a hand-written migration, once no deployed release uses them |
| Generated migrations | planned | hand-written migrations ([Changing the schema](schema-changes.md)) |
| Backends other than Postgres | Postgres 16+ (Aurora PostgreSQL) only | |
| Drivers other than pgx v5 | | |

## Known rough edges

- **Upsert by key on a soft-deleted row** returns `ErrAlreadyExists`, because
  dalforge never revives deleted rows. Delete for good with a custom query
  first if you mean to reuse the key.
- **An optional field forgotten in an update params struct is cleared to
  NULL.** Keep updates narrow (list their `columns`), and read the params
  type's doc comment, which says what nil means for each field.
- **Generated and custom queries share sqlc's output directory.** sqlc names
  its output files after the query file, so `queries/custom/accounts.sql` and
  the generated `accounts.sql` both end up in `gen/sqlcdb/accounts.sql.go`.
  That's harmless, but prefer distinct custom file names for clarity.
- **A new list can absorb an existing index.** If a new list's index starts
  with all the columns of an existing one, the longer index replaces the
  shorter: `schema.sql` drops the old index and adds the new. Create the new
  index first, and drop the old one in a later release.
