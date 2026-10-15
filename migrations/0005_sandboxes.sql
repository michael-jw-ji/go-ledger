-- +goose Up
-- Sandboxes give each visitor of the public demo their own accounts and
-- transfers. Rows with a NULL sandbox_id are the unscoped data the API has
-- always had (local dev, curl, tests), and requests without a sandbox header
-- see only that data.
CREATE TABLE sandboxes (
    id         text        PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE accounts  ADD COLUMN sandbox_id text REFERENCES sandboxes (id);
ALTER TABLE transfers ADD COLUMN sandbox_id text REFERENCES sandboxes (id);

CREATE INDEX accounts_sandbox  ON accounts (sandbox_id);
CREATE INDEX transfers_sandbox ON transfers (sandbox_id);

-- +goose Down
DROP INDEX transfers_sandbox;
DROP INDEX accounts_sandbox;
ALTER TABLE transfers DROP COLUMN sandbox_id;
ALTER TABLE accounts DROP COLUMN sandbox_id;
DROP TABLE sandboxes;
