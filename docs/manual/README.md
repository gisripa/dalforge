# The dalforge manual

dalforge turns a protobuf description of your tables and **access patterns**
into a Postgres schema with the right indexes, [sqlc](https://sqlc.dev)
queries, and a typed Go data access layer on pgx v5. It's meant for teams
whose engineers know Go well and SQL less well. The patterns they declare
come out correct by construction, and the ones that would hurt in production
are rejected by the linter, with an explanation.

New here? Start with the **[Quickstart](quickstart.md)**. It runs the
example shop end to end and shows everything dalforge generates.

## Contents

| Chapter | What it covers |
|---|---|
| [Quickstart](quickstart.md) | Run the demo, tour the generated files, change an access pattern, trip the linter |
| [Concepts](concepts.md) | Entities, stores and access patterns; what's yours and what's generated; non-goals |
| [Setting up a project](project-setup.md) | Installing and pinning dalforge, `dalforge.yaml`, `dalforge.lock`, the CLI, CI |
| [Writing the IDL](idl.md) | Tables, fields, keys, roles and stores: how the options fit together |
| [Option reference](options.md) | Every option and value *(generated from the options protos)* |
| [Types](types.md) | How proto types map to Postgres and Go *(tables generated from the backend)*; custom and extension types; Postgres versions |
| [Access patterns](queries.md) | Get, Create, Update, Delete, Upsert: what each generates and what nil means |
| [Lists, indexes and pagination](lists.md) | One shape per rpc, the ESR rule, sort resolution, derived indexes, page tokens and their guarantees |
| [The Go API](go-api.md) | The generated package, wiring, errors, retries, read routing, tracing, custom queries, transactions |
| [Changing the schema](schema-changes.md) | The additive-only contract, and how to remove or retype a field safely |
| [Lint rules](lint-rules.md) | Every rule: what it catches, why it matters, how to fix it *(generated from `internal/rules`)* |
| [Limitations](limitations.md) | What dalforge doesn't do, and the escape hatch for each |

## Status

This manual describes dalforge v0: CRUD by key, lists with derived indexes
and keyset pagination, upsert, transactions, and the lint rules.

> **Schema changes aren't checked yet.** Generated migrations, and the lint
> rules that catch unsafe schema changes, need dalforge to know what has
> already shipped, which it doesn't track yet. Until it does, write migrations by
> hand following [Changing the schema](schema-changes.md). In particular,
> never drop a removed field's column, and never let a schema-diff tool do it
> for you: the release still running during a deploy uses it.

For the reasons behind the design, see the [design doc](../design.md).

## Generated pages

Some pages must match the code exactly, so they're generated rather than
written: the [lint rule catalog](lint-rules.md) (from `internal/rules/catalog`),
the [option reference](options.md) (from the comments in
`proto/dal/*/options.proto`) and the type tables in [Types](types.md) (by
running a probe IDL through the real Postgres backend). Edit the sources, then
run `mise run docs`; `mise run check` fails while a generated page is stale.
