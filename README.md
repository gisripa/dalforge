# dalforge

Generate a DAL layer which models around access patterns than query knowledge.

DALForge reads a protobuf IDL of tables and access patterns and generates the
Postgres schema, indexes, sqlc queries and a typed Go data access layer on pgx
v5. See [docs/design.md](docs/design.md). Status: design + scaffold; the
generator is not implemented yet.

The Go toolchain, linters and tasks are managed by
[mise](https://mise.jdx.dev) via `mise.toml`. You don't need Go or Docker Desktop
installed: integration tests run Postgres in a repo-scoped [Colima](https://github.com/abiosoft/colima) VM.

## Setup (once per machine)

Requires `make` and `curl` (both ship with macOS and most Linux distros).

```sh
make          # installs mise to ~/.local/bin, activates it in your shell rc, installs Go 1.26, golangci-lint, Colima + docker CLI/compose
exec $SHELL   # reload your shell
```

## Tasks

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
mise run check:all                # check + integration (what CI runs)
```

## Layout

```
cmd/dalforge/       CLI entrypoint
internal/cli/       subcommand dispatch
internal/idl/       IDL loading (currently a smoke test for the options proto)
internal/golden/    golden-file assertions for generator output
internal/pgtest/    per-test Postgres databases for integration tests
internal/integration/ end-to-end tests (build tag `integration`)
compose.yaml        local postgres:16.9 for integration tests and examples
proto/dal/v1/       dal.v1 backend-neutral options (draft) + Go bindings (dalv1)
proto/dal/pg/v1/    dal.pg.v1 Postgres options (draft) + Go bindings (pgv1)
scripts/            protogen.sh (used by `mise run proto`)
docs/design.md      design doc
```
