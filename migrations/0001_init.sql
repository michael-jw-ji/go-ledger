-- +goose Up
CREATE TYPE side AS ENUM ('debit', 'credit');
CREATE TYPE transfer_status AS ENUM ('pending', 'posted', 'rejected');

CREATE TABLE accounts (
    id             bigserial PRIMARY KEY,
    name           text        NOT NULL,
    currency       text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    normal_balance side        NOT NULL,
    allow_negative boolean     NOT NULL DEFAULT false,
    hot            boolean     NOT NULL DEFAULT false,
    -- Cached balance in minor units, signed relative to normal_balance.
    balance        bigint      NOT NULL DEFAULT 0,
    -- Number of entries applied to the cached balance.
    lock_version   bigint      NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- Defense in depth: the service checks overdraft first, this catches bugs.
    CONSTRAINT accounts_no_overdraft CHECK (allow_negative OR balance >= 0)
);

CREATE TABLE transfers (
    id                   uuid PRIMARY KEY,
    -- The Postgres half of idempotency: a retried key can never post twice.
    idempotency_key      text            NOT NULL UNIQUE,
    request_hash         bytea           NOT NULL,
    status               transfer_status NOT NULL,
    error_code           text,
    reverses_transfer_id uuid REFERENCES transfers (id),
    created_at           timestamptz     NOT NULL DEFAULT now(),
    CONSTRAINT transfers_error_code_iff_rejected
        CHECK ((status = 'rejected') = (error_code IS NOT NULL))
);

-- A transfer can be reversed at most once. Rejected reversal attempts don't
-- count, so a reversal that failed for insufficient funds can be retried.
CREATE UNIQUE INDEX transfers_one_reversal
    ON transfers (reverses_transfer_id)
    WHERE reverses_transfer_id IS NOT NULL AND status <> 'rejected';

CREATE TABLE entries (
    id          bigserial PRIMARY KEY,
    transfer_id uuid        NOT NULL REFERENCES transfers (id),
    account_id  bigint      NOT NULL REFERENCES accounts (id),
    direction   side        NOT NULL,
    amount      bigint      NOT NULL CHECK (amount > 0),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX entries_account_id_id ON entries (account_id, id);
CREATE INDEX entries_transfer_id ON entries (transfer_id);

-- Entries are append-only. Mistakes are fixed with a reversal transfer.
-- +goose StatementBegin
CREATE FUNCTION entries_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'entries are append-only (% not allowed)', TG_OP;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER entries_append_only
    BEFORE UPDATE OR DELETE ON entries
    FOR EACH ROW EXECUTE FUNCTION entries_append_only();

-- +goose Down
DROP TABLE entries;
DROP FUNCTION entries_append_only();
DROP TABLE transfers;
DROP TABLE accounts;
DROP TYPE transfer_status;
DROP TYPE side;
