# Changing the schema

> **Status:** dalforge doesn't generate migrations yet. `dalforge generate`
> writes the full `schema/schema.sql` for a new database, and you write
> migrations by hand. This chapter describes the rules that keep schema
> changes safe during a rolling deploy; generated migrations (a schema
> snapshot, `dalforge migrate`, and the DAL3xx rules) will check the same
> rules.

## The contract: every change is additive

During a rolling deploy, the migration runs first, and then instances of the
old release (N) and the new one (N+1) run side by side against the new
schema. So **every schema change for N+1 must keep N working.**

| Change | Migration | Safe? |
|---|---|---|
| new entity | `CREATE TABLE` | yes |
| new `optional` field | `ALTER TABLE … ADD COLUMN … NULL` | yes: N's inserts leave it out |
| new required field **with** a constant `default` | `ADD COLUMN … NOT NULL DEFAULT <const>` | yes: N's inserts get the default |
| new required field **without** a default | — | **no**: N's inserts fail. Add a `default`, or make it `optional` |
| new access pattern | `CREATE INDEX CONCURRENTLY` | yes |
| change a field's type (kind, `format`, pg `type`) | — | **no**: N reads and writes the old type. [Add a new field instead](#changing-a-type-or-a-name) |
| rename a column | — | **no**: N uses the old name. Renaming only the proto field while pinning `(dal.v1.field).name` is fine |
| make a field required, or add `unique` | — | **no**: N may write values the new rule rejects, and existing rows may already violate it. Do it as a deliberate, hand-written migration |
| remove a field | — (the column stays) | yes: the column is **retired** (see below) |
| remove an entity or a list | — (the table or index stays) | yes: retired |

## Removing things: retire, never drop

To remove a field, delete it from the message and **reserve its number and
name**:

```proto
message Order {
  reserved 5;
  reserved "note";
  …
}
```

N+1 stops reading and writing the column, but the column stays in the
database, because N still uses it. If it's `NOT NULL` without a default, relax
it (`ALTER COLUMN … DROP NOT NULL`) so N+1's inserts, which leave it out,
still succeed. Removed tables and indexes stay the same way.

Dropping a retired column, table or index is a separate, later, hand-written
migration, once no deployed code uses it. That's never part of a normal
release. Until then, a retired index still costs writes.

**Never reuse a field number.** The number is the column's identity: reusing
one, or reviving a reserved one, is how a "new" field silently reads an old
column's data. The planned rules DAL111–113 will enforce this.

## Changing a type or a name

A type change or a rename is done as additive steps, spread over releases:

1. Add the new field, with a new number and the new type or name.
2. Backfill it: write both fields in application code, and copy the existing
   rows in a migration or a background job.
3. Switch reads to the new field.
4. Remove the old field. Its column retires.

## Writing migrations by hand today

Use any migration tool; generated migrations will be
[golang-migrate](https://github.com/golang-migrate/migrate) files. Compare the
new `schema/schema.sql` with the previous one (generated files are
deterministic, so `diff` works), and write the matching statements.

**If you use a schema-diff tool** (Atlas, migra, pg-schema-diff) against
`schema.sql`, review what it proposes. It doesn't know the additive contract:
for a removed field, it will propose `DROP COLUMN`, and it creates indexes
without `CONCURRENTLY` unless told to. Replace drops with the
[retire step](#removing-things-retire-never-drop), and make index creation
concurrent.

When writing them:

- Create derived indexes with `CREATE INDEX CONCURRENTLY`, so writes aren't
  blocked, and in a migration of their own: `CONCURRENTLY` can't run inside a
  transaction.
- Start other migrations with `SET lock_timeout = '5s';`, so a migration
  stuck behind a long query fails fast instead of queueing every write behind
  it.
- An index that disappeared from `schema.sql` because a new, longer index
  now serves its lists is retired, not dropped: create the new one, and drop
  the old one in a later release. Derived index names only ever change when
  the index's columns do, so a renamed index really is a different index.

## Planned: generated migrations

- `dalforge.snapshot.json`, committed next to the IDL, recording what has
  shipped: tables, columns keyed by field number, indexes, and what's retired.
- `dalforge migrate -name <slug>`, which diffs the IDL against the snapshot
  and writes the migration files.
- Lint rules for unsafe changes (DAL301–306) and for field-number identity
  (DAL111–113), and a check that fails when the IDL changed without a
  migration.
