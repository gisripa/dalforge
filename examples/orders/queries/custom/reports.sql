-- Hand-written sqlc queries: the escape hatch for anything the IDL doesn't
-- generate. This directory is yours; dalforge never writes here.

-- name: OrderTotalsByStatus :many
SELECT status, count(*) AS orders, sum(amount_cents)::bigint AS amount_cents
FROM orders
WHERE account_id = @account_id
  AND deleted_at IS NULL
GROUP BY status
ORDER BY status;
