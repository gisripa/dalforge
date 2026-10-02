# Lists, indexes and pagination

Lists are where hand-written data access goes wrong most often: a missing
index, `OFFSET` pagination, an unstable sort, or a filter that can't use the
index it has. dalforge makes each list one fixed, indexable shape, derives
its index, and paginates with keysets.

## Declaring a list

```proto
rpc ListOrdersByAccount(ByAccount) returns (OrderPage) {
  option (dal.v1.query) = {
    list: {
      eq: ["account_id"]               // equality filters (all required)
      order_by: ["created_at DESC"]    // sort; ASC is the default
    }
  };
}

message ByAccount {
  string account_id = 1;
  int32 page_size = 2;
  string page_token = 3;
}

message OrderPage {
  repeated Order orders = 1;
  string next_page_token = 2;
}
```

| `list` field | Meaning |
|---|---|
| `eq` | Fields that must equal the given values. All of them are required at every call |
| `range` | At most one field filtered by a half-open interval `[from, to)`. It must be the leading `order_by` field |
| `order_by` | Sort fields, each optionally `ASC` or `DESC`. See [where the sort comes from](#where-the-sort-comes-from) |
| `default_page_size`, `max_page_size` | Page size when the caller passes 0, and the cap. Defaults 50 and 500 |
| `consistency` | `CONSISTENCY_STRONG` reads from the writer pool |

In Go, you get a params struct with the filters and paging fields, and a
method returning one page:

```go
p, err := orders.ListOrdersByAccount(ctx, shopdal.OrderListOrdersByAccountParams{
	AccountID: id,
	PageSize:  20,           // 0 → the default
	PageToken: "",           // "" → the first page
})
// p.Items []Order, p.NextPageToken string ("" on the last page)
```

## One shape per rpc

A list has **no optional filters**. "Orders, optionally filtered by status,
optionally within a date range" is three different queries, and no single
index serves all three well. A query planner faced with
`(@status IS NULL OR status = @status)` usually gives up on the index
entirely.

So each combination is its own rpc, with its own index. Name lists after what
they filter by: `List<Entities>By<Eq…>And<Range>`, such as
`ListOrdersByAccountAndStatusAndCreatedAt`. A name that doesn't say what the
list filters by gets a warning ([DAL114](lint-rules.md#dal114)).

## The ESR rule

Indexes serve queries best in **E**quality, **S**ort, **R**ange order. dalforge
builds every list's index that way: `(eq…, sort…, primary key)`. That's also
why a `range` must be the leading sort field ([DAL101](lint-rules.md#dal101)):
an index can range over one column and return rows sorted by it, but it can't
range over one column while sorting by another.

The example's ladder, from broad to narrow:

| rpc | `eq` | `range` / `order_by` | Index |
|---|---|---|---|
| `ListOrdersByAccount` | account_id | `created_at DESC` | `(account_id, created_at, id)`, read backwards |
| `ListOrdersByStatus` | status | `created_at DESC` | `(status, created_at, id)`, read backwards |
| `ListOrdersByStatusAndCreatedAt` | status | range `created_at` | shares the one above, read forwards |
| `ListOrdersByAccountAndStatusAndCreatedAt` | account_id, status | range `created_at` | `(account_id, status, created_at, id)` |

### Ranges

```proto
list: {eq: ["status"] range: "created_at"}
```

The request message gets `created_at_from` and `created_at_to`, and the Go
params get `CreatedAtFrom` and `CreatedAtTo`. Both are required, and the
interval is half-open: `created_at >= from AND created_at < to`. Consecutive
windows such as days or months therefore never overlap or leave gaps.

## Where the sort comes from

1. **`order_by`**, if declared.
2. Otherwise, **the range field**, ascending.
3. Otherwise, **an explicit index** whose leading columns are exactly the `eq`
   fields: the list inherits that index's remaining columns as its sort, and
   that index serves it. See [Explicit indexes](#explicit-indexes).
4. Otherwise, **the primary key.** That's stable, but meaningful only if the key
   is time-ordered: a UUID key (dalforge assigns UUIDv7) or an integer. For
   other keys, you get a warning to declare `order_by`
   ([DAL103](lint-rules.md#dal103)).

The primary key is always appended as a tie-breaker, in the direction of the
last sort field, so the order is total and pagination never skips or repeats
rows with equal sort values.

### Sort restrictions

- **No nullable sort fields**, except the range field
  ([DAL115](lint-rules.md#dal115)). Keyset comparisons treat NULL as unknown,
  so rows with NULLs would silently vanish from pages. The range filter
  excludes NULLs explicitly, so a nullable range field is fine, and its index
  is partial on `IS NOT NULL`.
- **Avoid sorting by fields an update can change**
  ([DAL109](lint-rules.md#dal109), a warning). If a row's sort value changes
  while a client pages through, the row can move to a page the client has
  already read, or to one it hasn't read yet.
- **Mixed directions work, but cost more** ([DAL203](lint-rules.md#dal203)).
  `order_by: ["points DESC", "player"]` can't use a single row comparison for
  the cursor, so the query uses a chain of ORs, which Postgres plans less
  efficiently.

## Derived indexes

You don't declare indexes for lists. dalforge derives them and writes them to
`schema/schema.sql`, with a comment naming the rpcs each one serves:

```sql
-- derived for: ListOrdersByStatus, ListOrdersByStatusAndCreatedAt
CREATE INDEX orders_status_created_at_id_idx ON orders (status, created_at, id) WHERE deleted_at IS NULL;
```

- **One direction:** a derived index is stored with its first sort column
  ascending, and B-trees scan both ways, so it serves a newest-first list
  as well as an oldest-first one. This keeps every index's shape and name a
  function of its columns alone: adding, removing or reordering *other*
  lists never renames or rebuilds an index you've already deployed.

- **Partial on live rows:** on soft-delete tables, indexes cover only rows
  with `deleted_at IS NULL`, the only rows lists return. For a nullable range
  field, they also exclude rows where it's NULL.
- **Shared where possible:** an index is dropped when another one already
  serves its list. That happens when the other index starts with the same
  columns, either in the same directions or all reversed, since B-trees scan
  both ways. The other index can be the primary key, an explicit index or a
  longer derived index.
- **Budgeted:** every index slows every insert and update. Past the table's
  budget (5 by default, counting the primary key and unique indexes), lint
  warns ([DAL201](lint-rules.md#dal201)). Raise it with
  `(dal.pg.v1.table).index_budget` if the reads are worth it.
- **Hints:** two lists with the same `eq` fields but unrelated sorts each
  need an index. Lint points this out ([DAL206](lint-rules.md#dal206)), since
  aligning their `order_by` would let them share one.

Get lookups by a unique field use that field's unique index, and lookups by
the key use the primary key, so neither adds indexes.

## Explicit indexes

Declare an index yourself when you want a partial or covering index, or a
shared sort for lists that don't declare `order_by`:

```proto
message Doc {
  option (dal.pg.v1.table) = {
    indexes: [
      {
        name: "docs_by_folder"            // default: <table>_<cols>_idx
        columns: ["folder_id", "title"]   // proto field names, optionally ASC/DESC
      },
      {
        columns: ["path", "created_at DESC"]
        include: ["status"]               // covering: INCLUDE (status)
        where: "status >= 500"            // partial: a SQL predicate
      }
    ]
  };
  …
}
```

- A list with `eq: ["folder_id"]` and no `order_by` inherits `title` as its
  sort, and `docs_by_folder` serves it.
- A partial index (`where`) serves only queries whose filters imply its
  predicate. dalforge doesn't try to prove that, so it uses a partial
  explicit index for a list only when the predicate is exactly the list's own
  (`deleted_at IS NULL`). Partial and covering indexes are for your
  [custom queries](go-api.md#custom-queries).
- If a list declares an `order_by` that contradicts an explicit index on the
  same `eq` fields, that's an error ([DAL202](lint-rules.md#dal202)): either
  align them, or accept that the list needs a second index.
- A `unique` explicit index can back a `get.by` or an `upsert.conflict_on`
  ([DAL104](lint-rules.md#dal104)).

## Page tokens

Every list returns `dal.Page[T]{Items, NextPageToken}`. To fetch the next
page, pass the token back with the **same filters**:

```go
params := shopdal.OrderListOrdersByAccountParams{AccountID: id, PageSize: 20}
for {
	page, err := orders.ListOrdersByAccount(ctx, params)
	if err != nil {
		return err
	}
	render(page.Items)
	if page.NextPageToken == "" {
		break
	}
	params = params.WithPageToken(page.NextPageToken)
}
```

- **Opaque, but not secret.** A token is base64url-encoded JSON holding the
  last row's sort values and key. They're values from a row the client has
  already seen, so you can hand tokens to API clients and accept them back.
  Tokens aren't signed or encrypted; don't put anything in a sort key you
  wouldn't show the client.
- **Bound to the query.** A token carries a hash of the list and of the
  filter values. Using it with another list, or with different filters,
  returns `dal.ErrInvalidPageToken` instead of a wrong page. That error wraps
  `dal.ErrInvalidArgument`, so it's never retried.
- **Page size can change between calls.** It's clamped to the list's
  maximum.
- An empty `NextPageToken` means the last page. A page with no items never
  has a token.

### Iterating everything in-process

For batch jobs and exports, `dal.All` turns any list method into an iterator:

```go
for o, err := range dal.All(ctx, shopdal.OrderListOrdersByStatusParams{Status: "paid"}, orders.ListOrdersByStatus) {
	if err != nil {
		return err
	}
	process(o)
}
```

It fetches page by page, stops when you `break`, and stops at the first
error, yielding it.

## Consistency while paging

Each page is a separate query, so an iteration doesn't see the table as of a
single moment. A snapshot would need a transaction held open across
requests. What keyset pagination does guarantee:

| Row during the iteration | Outcome |
|---|---|
| present throughout, sort values unchanged | returned **exactly once**: no duplicates, no skips |
| inserted, sorting behind the cursor | not returned (for newest-first lists, that's where new rows land) |
| inserted, sorting ahead of the cursor | returned |
| deleted, or soft-deleted | returned only if its page was already read |
| sort value changed by an update | may be returned twice or not at all ([DAL109](lint-rules.md#dal109)) |

`OFFSET` pagination can't even give the first row of this table: one insert
or delete before the offset shifts every later page. That's why dalforge
never generates it.

If you need a true snapshot, for example for an export or a reconciliation,
write a [custom query](go-api.md#custom-queries) and read everything in one
`REPEATABLE READ` transaction.

## What the generated SQL looks like

Each list is two sqlc queries: the first page, and the page after a cursor.
Two fixed shapes keep Postgres on the index even for prepared statements with
generic plans, which a single query with `@cursor IS NULL OR …` doesn't.

```sql
-- name: OrderListOrdersByAccount :many
SELECT … FROM orders
WHERE account_id = @account_id
  AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: OrderListOrdersByAccountAfter :many
SELECT … FROM orders
WHERE account_id = @account_id
  AND deleted_at IS NULL
  AND (created_at, id) < (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;
```

The repository asks for one row more than the page size. The extra row only
signals that another page exists.
