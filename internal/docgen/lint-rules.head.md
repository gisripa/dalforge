# Lint rules

`dalforge lint` and `dalforge generate` report findings as:

```text
shop/v1/shop.proto:131:3: error DAL115: rpc ListOrdersByAccountAndStatus: order_by "note" is optional (nullable); …
```

- **error:** the IDL can't be generated safely. `generate` writes nothing,
  and `lint` exits 1.
- **warning:** this works, but it's probably not what you want. Fix it, or
  accept it knowingly.
- **info:** a suggestion.

Rule numbers: `DAL1xx` rules are backend-neutral (they apply to any backend),
`DAL2xx` rules are Postgres-specific, and `DAL3xx` is reserved for migration
safety.

**One mistake, one error.** When a query names a field that doesn't exist
(DAL117), the rules that would trip over the same typo are skipped for that
query.

Problems the IDL loader catches before any rule runs (proto syntax errors, a
store without an entity, an rpc without `(dal.v1.query)`, `oneof` in an
entity, an unsupported well-known type) are reported without a rule ID.
