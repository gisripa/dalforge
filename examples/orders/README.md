# orders: a dalforge example project

A small shop (accounts and orders), set up the way a dalforge user's project
would be. You write the IDL (`proto/`), a config (`dalforge.yaml`) and,
optionally, hand-written queries (`queries/custom/`). dalforge generates the
rest.

## Run it

From the repository root:

```sh
mise run demo
```

That starts the local Postgres, builds `dalforge`, then in this directory:

```sh
dalforge generate   # schema, queries, sqlc.yaml, and the Go DAL (runs sqlc)
go run .            # the app: fake data, every step checked
```

The app recreates an `orders_demo` database on each run and walks through
creating accounts and orders, lookups, a duplicate, a missing field,
optimistic locking (an update, then a stale one), a soft delete and a custom
query. It exits non-zero if anything behaves unexpectedly, so a green run
means the whole chain works.

## What's yours, what's generated

| Path | Owner |
|---|---|
| `proto/`, `dalforge.yaml`, `queries/custom/`, `main.go` | you (committed) |
| `schema/`, `queries/generated/`, `sqlc.yaml`, `gen/` | generated (gitignored; recreate with `dalforge generate`) |

Your app imports the generated DAL package (`gen/shop/v1/shopdal`): model
types and `…Repository` interfaces, wired once with `dalpg.New`.
