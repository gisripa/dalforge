# dalforge

[![CI](https://github.com/gisripa/dalforge/actions/workflows/ci.yml/badge.svg)](https://github.com/gisripa/dalforge/actions/workflows/ci.yml)

Generate a DAL layer which models around access patterns than query knowledge.

DALForge reads a protobuf IDL of tables and access patterns and generates the
Postgres schema, derived indexes, [sqlc](https://sqlc.dev) queries and a typed
Go data access layer on pgx v5. It's built for engineers who know Go better
than SQL: declared patterns come out correct by construction (keyset
pagination, soft delete, optimistic locking, column defaults), and unsafe ones
are rejected by a linter that explains why.

- **Try it:** [Quickstart](docs/manual/quickstart.md), which runs `mise run demo`
  and tours everything dalforge generates for the [example shop](examples/orders).
- **Use it:** [the manual](docs/manual/README.md).
- **Why it's built this way:** [the design doc](docs/design.md).

Status: CRUD by key, lists with derived indexes and page tokens, upsert,
transactions and the lint rules work end to end on Postgres 16. Generated
migrations are next.

## Install

```toml
# mise.toml in your project
[tools]
"github:gisripa/dalforge" = "0.1.0"
sqlc = "1.31"
```

Or `go install github.com/gisripa/dalforge/cmd/dalforge@v0.1.0`. Generated code
needs the runtime at the same version:
`go get github.com/gisripa/dalforge/dal@v0.1.0`. See
[Setting up a project](docs/manual/project-setup.md).

## Developing dalforge

The Go toolchain, linters and tasks are managed by
[mise](https://mise.jdx.dev) via `mise.toml`. You don't need Go or Docker Desktop
installed: integration tests run Postgres in a repo-scoped [Colima](https://github.com/abiosoft/colima) VM.

### Setup (once per machine)

Requires `make` and `curl` (both ship with macOS and most Linux distros).

```sh
make          # installs mise to ~/.local/bin, activates it in your shell rc, installs Go 1.26, golangci-lint, Colima + docker CLI/compose
exec $SHELL   # reload your shell
```

### Tasks

```sh
mise tasks                        # list everything
mise run dalforge -- -h           # run the CLI
mise run test
mise run test:update               # accept new generator output into testdata/*.golden (review the diff)
mise run lint
mise run fmt
mise run build                    # -> ./bin/dalforge
mise run proto                    # regenerate options Go bindings after editing proto/
mise run check                    # lint + unit tests + build (no Docker)
mise run db:up                    # start the Colima VM + local postgres:16.9 (docker compose)
mise run vm:down                  # stop the VM when done (frees ~2 GB RAM)
mise run test:integration         # integration tests against it
mise run check:all                # check + Postgres tests
mise run demo                     # run examples/orders end to end, like a user would
mise run docs                     # regenerate the generated parts of docs/manual
```

### Layout

```
cmd/dalforge/          CLI entrypoint
dal/                   the runtime module (own go.mod), stdlib only: errors, ops, retries, pages
dal/dalpg/             runtime on pgx v5: reader/writer routing, SQLSTATE mapping, transactions
internal/cli/          subcommands (generate, lint, lock)
internal/config/       dalforge.yaml
internal/pipeline/     load → check → build → emit → write → sqlc; dalforge.lock
internal/idl/          IDL loading: resolver serving the bundled options, typed option decoding
internal/ir/           the backend-neutral model (entities, stores, access patterns)
internal/check/        backend-neutral lint rules (DAL1xx)
internal/backend/pg/   Postgres: physical model, indexes, rules (DAL2xx), SQL, sqlc.yaml, the Go DAL
internal/manualtest/   keeps docs/manual true to the code (rule catalog, links)
internal/golden/       golden-file assertions for generator output
internal/pgtest/       per-test Postgres databases for integration tests
internal/integration/  Postgres tests (build tag `integration`)
examples/orders/       a user project: IDL, config, custom queries and a demo app
compose.yaml           local postgres:16.9 for integration tests and the demo
proto/dal/v1/          dal.v1 backend-neutral options + Go bindings (dalv1)
proto/dal/pg/v1/       dal.pg.v1 Postgres options + Go bindings (pgv1)
internal/rules/         the lint rule catalog (one Markdown file per rule)
internal/docgen/       generates the rule catalog, option reference and type tables in docs/manual
scripts/               protogen.sh (used by `mise run proto`)
docs/manual/           the user manual
docs/design.md         design doc
```

Releases: see [RELEASING.md](RELEASING.md). License: [MIT](LICENSE).
