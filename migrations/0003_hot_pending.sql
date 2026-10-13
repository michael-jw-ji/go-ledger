-- +goose Up
-- Entries posted to a hot account whose effect is not yet in its cached
-- balance. Increases to a hot account skip the account row lock: the entry
-- and this row commit with the transfer, and the rollup worker folds pending
-- rows into accounts.balance a moment later.
--
-- Effective balance = accounts.balance + SUM(hot_pending.signed_amount), read
-- in one statement. It equals the sum of the account's entries at all times.
--
-- This is a set of rows, not a "rolled up to entry id N" watermark: bigserial
-- ids are assigned at insert but commit in any order, so a watermark could
-- skip an entry whose transaction commits after a later id was rolled up.
CREATE TABLE hot_pending (
    entry_id      bigint PRIMARY KEY REFERENCES entries (id),
    account_id    bigint NOT NULL REFERENCES accounts (id),
    -- The entry's effect on the balance, signed by the account's normal side.
    signed_amount bigint NOT NULL
);

CREATE INDEX hot_pending_account_id ON hot_pending (account_id);

-- +goose Down
DROP TABLE hot_pending;
