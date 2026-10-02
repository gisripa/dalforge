# Setting up a project

This chapter sets dalforge up in your own Go repository. The
[example project](../../examples/orders) is a working reference for every
step.

## Install and pin the tools

dalforge runs `sqlc` and checks its version, so pin both. With
[mise](https://mise.jdx.dev), add them to your project's `mise.toml`:

```toml
[tools]
"github:gisripa/dalforge" = "0.1.0"   # a prebuilt binary from GitHub Releases
sqlc = "1.31"
```

Other ways to install the same release:

- **From source:** `go install github.com/gisripa/dalforge/cmd/dalforge@v0.1.0`,
  or with mise, `"go:github.com/gisripa/dalforge/cmd/dalforge" = "0.1.0"`.
- **By hand:** download `dalforge_<version>_<os>_<arch>.tar.gz` for Linux or
  macOS (amd64, arm64) from the
  [releases page](https://github.com/gisripa/dalforge/releases), and check it
  against `checksums.txt`.

`dalforge version` prints the version you're running. dalforge is a single
binary with no cgo. It doesn't need `protoc` or `buf`: it compiles your
`.proto` files itself. It finds `sqlc` on your `PATH`.

Your module needs the runtime that generated code imports. It's a separate,
small Go module, released in lockstep with dalforge, so **use the same
version as your dalforge**:

```sh
go get github.com/gisripa/dalforge/dal@v0.1.0 github.com/jackc/pgx/v5 github.com/google/uuid
```

The runtime has two packages: `github.com/gisripa/dalforge/dal` (standard
library only) and `github.com/gisripa/dalforge/dal/dalpg` (pgx v5). It doesn't
pull in any of the generator's dependencies.

## Lay out the project

```text
your-service/
├── dalforge.yaml
├── dalforge.lock          created by the first `dalforge generate`; commit it
├── proto/
│   └── shop/v1/shop.proto
├── queries/custom/        optional: your own sqlc queries
└── …                      your code
```

`.gitignore` the generated output, unless you'd rather review it in pull
requests:

```gitignore
/schema/
/queries/generated/
/sqlc.yaml
/gen/
```

## `dalforge.yaml`

```yaml
version: 1
module: github.com/acme/shop   # your Go module path (required)
backend: pg
proto:
  roots: [proto]               # import paths; default [proto]
  files: ["shop/v1/*.proto"]   # globs under the roots; default: every .proto
pg:
  version: "16.9"              # the Postgres you deploy to; default 16
out:                           # defaults shown
  schema: schema/schema.sql
  queries: queries/generated
  custom: queries/custom       # yours; dalforge never writes here
  sqlc: gen/sqlcdb             # sqlc's Go package (its last element is the package name)
  dal: gen                     # the DAL goes to gen/<proto package path>/<pkg>dal
```

- **Unknown keys are errors,** so a typo can't silently do nothing.
- **Paths** are relative to the file and must stay inside the project.
  `out.custom` may not overlap a generated directory.
- **`pg.version`** is the deploy target. Lint checks every type and feature
  you use against it (rule [DAL207](lint-rules.md#dal207)).

### Importing the options

Your `.proto` files import `dal/v1/options.proto` and, for Postgres
specifics, `dal/pg/v1/options.proto`. These ship inside the dalforge binary,
so you don't need copies. If your editor wants them for completion, vendor a
copy under a proto root. dalforge always uses its bundled options, and warns
when your copy differs from them ([DAL116](lint-rules.md#dal116)).

## The commands

```text
dalforge lint      [-config dalforge.yaml]
dalforge generate  [-config dalforge.yaml]
dalforge lock      [-config dalforge.yaml] [-upgrade]
```

**`dalforge lint`** checks the IDL without writing anything. It prints
findings to stderr as `file:line:col: severity RULE: message`, and exits 1
if there are errors. On a clean IDL it prints nothing.

**`dalforge generate`** runs the same checks and refuses to write anything if
there are errors. Otherwise it:

1. writes the schema, the queries, `sqlc.yaml` and the DAL package, touching
   only files whose content changed, so file timestamps stay stable;
2. removes stale generated files, but only files with a dalforge or sqlc
   `Code generated` header, and never anything under `out.custom`;
3. runs `sqlc generate` (skipped, with a note, if there are no queries at
   all yet);
4. creates `dalforge.lock` if there isn't one.

**`dalforge lock`** reports whether `dalforge.lock` matches the running
toolchain. `-upgrade` rewrites the lock to match.

`dalforge migrate` is reserved for generated migrations, the next phase. See
[Changing the schema](schema-changes.md).

## `dalforge.lock`

```toml
# dalforge.lock: maintained by dalforge, do not edit. Commit it.
lock_version = 1
dalforge = "v0.3.1"

[options]
"dal/pg/v1/options.proto" = "sha256:…"
"dal/v1/options.proto" = "sha256:…"

[tools]
"sqlc" = "v1.31.1"
```

Like `go.sum`, the lock makes sure everyone generates the same code.
`lint` and `generate` fail when the running dalforge, its bundled options or
`sqlc` differ from the lock. That catches a teammate with a newer binary
before they regenerate everything differently. After an intentional upgrade:

```sh
dalforge lock -upgrade   # then review and commit dalforge.lock with the regenerated code
```

A dalforge built from source rather than from a release (shown as
`(devel)`) gets a warning instead of an error.

## Wire it into your build

A typical sequence for a developer machine or CI:

```sh
dalforge lint                # fast; fails on unsafe patterns
dalforge generate            # if you don't commit generated code
go build ./... && go test ./...
```

If you commit generated code, add a freshness check: run `dalforge generate`,
then fail if `git status --porcelain` shows changes.

## Next

- [Writing the IDL](idl.md)
- [The Go API](go-api.md): wiring the generated repositories into your code
