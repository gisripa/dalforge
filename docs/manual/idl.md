# Writing the IDL

A dalforge IDL is ordinary proto3. Messages describe tables, services
describe the access patterns on them, and custom options carry everything
else. This chapter explains how the pieces fit together. The
**[Option reference](options.md)** lists every option and value, generated
from the options protos themselves. [Types](types.md) covers how fields map to
columns, and [Access patterns](queries.md) and [Lists](lists.md) cover the
rpcs.

```proto
syntax = "proto3";

package shop.v1;

import "dal/pg/v1/options.proto";   // Postgres specifics (optional)
import "dal/v1/options.proto";      // the backend-neutral options
import "google/protobuf/timestamp.proto";
```

- Only `syntax = "proto3"` is accepted (not proto2 or editions).
- **References use proto field names.** Every place an option names a field
  (`by`, `eq`, `order_by`, `columns`, index columns, …) uses the proto field
  name, never the SQL column name. If you use a column name by mistake, the
  error says so ([DAL117](lint-rules.md#dal117)).
- A proto package becomes one Go DAL package: `shop.v1` →
  `gen/shop/v1/shopdal`.
- Options come in two packages: `dal.v1` is backend-neutral (tables, fields,
  stores, access patterns), and `dal.pg.v1` refines the Postgres side (column
  types, defaults, explicit indexes).

## Entities

A message is an entity, and gets a table, when it has
[`(dal.v1.table)`](options.md#dalv1table) or when a store names it:

```proto
message Order {
  option (dal.v1.table) = {
    name: "orders"              // default: snake_case of the message name (Order → order)
    shard_key: ["account_id"]   // recorded and validated, not enforced yet
  };
  option (dal.pg.v1.table) = {
    index_budget: 6             // see Lists: derived indexes
  };
  …
}
```

- **Table names** default to the snake_case message name. They must be valid
  unquoted identifiers ([DAL204](lint-rules.md#dal204)), so a message called
  `Order` or `User`, whose default is a reserved word, needs an explicit
  `name`.
- **Entities can't contain `oneof`s**, and need at least one primary-key
  field.
- [`(dal.pg.v1.table)`](options.md#dalpgv1table) declares explicit indexes and
  the index budget. See [Lists](lists.md#explicit-indexes).

## Fields

```proto
  string id = 1 [(dal.v1.field) = {format: FORMAT_UUID primary_key: true}];
  string email = 2 [(dal.v1.field).unique = true];
  string status = 3 [(dal.pg.v1.column).default = "'pending'"];
  optional string note = 4;
```

Fields are refined by [`(dal.v1.field)`](options.md#dalv1field) (column name,
format, key, unique, role) and
[`(dal.pg.v1.column)`](options.md#dalpgv1column) (Postgres type, custom type,
default).

**Nullability comes from proto3 `optional`.** An `optional` field is a
nullable column and a pointer in Go. Every other field is `NOT NULL`. Don't
confuse this with message-typed fields, which always have presence in proto:
a plain `google.protobuf.Timestamp` is still required here.

**Defaults** matter to the generated Create: when you pass nil for a field
with a default, the column default applies ([Access patterns](queries.md#create)).
A default also makes adding a required field to an existing table safe
([Changing the schema](schema-changes.md)).

**Unique** fields on a soft-delete table are unique among live rows only, so
deleting a row frees its value for reuse.

### Primary keys

Prefer a `string` with `FORMAT_UUID`. When you create a row with a nil ID, the
DAL assigns a **UUIDv7**, which is time-ordered: it indexes well, and lists
that fall back to sorting by key come out in creation order. Several
`primary_key` fields form a composite key, in declaration order. Integer keys
work too, but dalforge doesn't generate sequences, so you supply the values.

### Roles

Roles are columns whose values the generated code manages: creation and
update times, soft delete, and an optimistic-locking version (see
[Role](options.md#dalv1role) for each). You never set them, and update or
upsert `columns` can't include them ([DAL118](lint-rules.md#dal118)). Each
role needs a particular field type, appears at most once per entity, and
can't be on a key field ([DAL119](lint-rules.md#dal119)):

```proto
  int64 version = 6 [(dal.v1.field).role = ROLE_VERSION];
  google.protobuf.Timestamp created_at = 7 [(dal.v1.field).role = ROLE_CREATE_TIME];
  google.protobuf.Timestamp updated_at = 8 [(dal.v1.field).role = ROLE_UPDATE_TIME];
  optional google.protobuf.Timestamp deleted_at = 9 [(dal.v1.field).role = ROLE_DELETE_TIME];
```

Role columns get schema defaults (`now()`, `1`), so you can add them to an
existing table safely. What they do to each access pattern is described in
[Access patterns](queries.md).

## Stores

A store is a service bound to one entity by
[`(dal.v1.store)`](options.md#dalv1store). Every rpc in it declares exactly
one access pattern with [`(dal.v1.query)`](options.md#dalv1query):

```proto
service OrderStore {
  option (dal.v1.store) = {entity: "Order"};   // a message name, local or fully qualified

  rpc GetOrder(OrderKey) returns (Order) {
    option (dal.v1.query) = {get: {}};
  }
}
```

- Every rpc in a store needs a `(dal.v1.query)` with one of `get`, `list`,
  `create`, `update`, `delete` or `upsert`. Streaming rpcs aren't allowed.
- Services without `(dal.v1.store)` are ignored, so the IDL can share a file
  with other services.
- The rpc name becomes the Go method name.
- Each store gets a Go name: its service name minus a trailing `Store`. It
  prefixes the store's sqlc query names (`OrderGetOrder`), which therefore
  must be unique across the project ([DAL205](lint-rules.md#dal205)).

### Request and response messages

The request and response messages must match the pattern exactly
([DAL107](lint-rules.md#dal107)). They document the access pattern in the
proto, and they keep the IDL honest if you ever expose it:

| Pattern | Request | Response |
|---|---|---|
| `get` | the entity, or a message with exactly the `by` fields (default: the key) | the entity |
| `list` | exactly the `eq` fields, `<range>_from` and `<range>_to` if there's a range, `int32 page_size`, `string page_token` | one `repeated` entity field, plus `string next_page_token` |
| `create`, `update`, `upsert` | the entity | the entity |
| `delete` | the entity, or exactly the key fields | the entity, or `google.protobuf.Empty` |

The request fields need the same proto types as the entity fields they stand
for.

## File options

[`(dal.pg.v1.file)`](options.md#dalpgv1file) states what a file's schema
needs from Postgres:

```proto
option (dal.pg.v1.file) = {
  min_version: 16           // default: dalforge's floor (16)
  extensions: ["vector"]    // emitted as CREATE EXTENSION IF NOT EXISTS
};
```

The deploy target, `pg.version` in `dalforge.yaml`, is checked against both.
See [Types](types.md#postgres-versions-and-extensions).
