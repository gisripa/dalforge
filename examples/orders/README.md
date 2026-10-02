# orders: a dalforge example project

A small shop (accounts and orders), set up the way a dalforge user's project
would be. You write the IDL (`proto/`), a config (`dalforge.yaml`) and,
optionally, hand-written queries (`queries/custom/`). dalforge generates the
rest.

For a guided tour of this example (what gets generated, changing an access
pattern, tripping the linter), see the [Quickstart](../../docs/manual/quickstart.md).

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

The app recreates an `orders_demo` database on each run and walks through:

- accounts: create (UUIDv7 ids), lookup by unique email, a duplicate, and an
  upsert by email;
- orders placed in a transaction (`shopdal.WithTx`) that first locks the
  account with a custom `FOR KEY SHARE` query, which is how referential
  integrity works without foreign keys; an unknown account rolls it back;
- keyset pagination with page tokens, `dal.All`, a token replayed against
  other filters (rejected), and a time-window range list;
- optimistic locking (an update, then a stale one), missing required fields,
  a soft delete, and a custom report query;
- finally, the indexes dalforge derived from the List rpcs.

It exits non-zero if anything behaves unexpectedly, so a green run means the
whole chain works.

## The ESR ladder

`OrderStore` declares one rpc per filter combination. dalforge derives an
index for each, equality columns first, then the sort/range column, then the
key (Equality, Sort, Range), and merges the ones that can share:

| rpc | filters | derived index |
|---|---|---|
| `ListOrdersByAccount` | `account_id`, newest first | `(account_id, created_at, id)`, read backwards |
| `ListOrdersByStatus` | `status`, newest first | `(status, created_at, id)`, read backwards |
| `ListOrdersByStatusAndCreatedAt` | `status`, `created_at` window | shares the one above |
| `ListOrdersByAccountAndStatusAndCreatedAt` | `account_id`, `status`, window | `(account_id, status, created_at, id)` |

All are partial (`WHERE deleted_at IS NULL`). See `schema/schema.sql` after
generating; `dalforge lint` explains any pattern it rejects.

## What's yours, what's generated

| Path | Owner |
|---|---|
| `proto/`, `dalforge.yaml`, `queries/custom/`, `main.go` | you (committed) |
| `schema/`, `queries/generated/`, `sqlc.yaml`, `gen/` | generated (gitignored; recreate with `dalforge generate`) |

Your app imports the generated DAL package (`gen/shop/v1/shopdal`): model
types and `…Repository` interfaces, wired once with `dalpg.New`.
