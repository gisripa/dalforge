-- Referential integrity without a foreign key: lock the account row for the
-- rest of the transaction (FOR KEY SHARE), so it can't be deleted before the
-- order referencing it commits.

-- name: LockAccountForKeyShare :one
SELECT id FROM accounts WHERE id = @id FOR KEY SHARE;
