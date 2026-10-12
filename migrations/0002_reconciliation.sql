-- +goose Up
-- Audit trail of reconciliation runs. A run that finds nothing still gets a
-- row, so "no mismatches" is evidence rather than silence.
CREATE TABLE reconciliation_runs (
    id               bigserial PRIMARY KEY,
    started_at       timestamptz NOT NULL,
    finished_at      timestamptz NOT NULL,
    accounts_checked integer     NOT NULL,
    mismatches       integer     NOT NULL,
    -- Every finding, so a mismatch can be investigated after the fact.
    details          jsonb       NOT NULL DEFAULT '[]'
);

CREATE INDEX reconciliation_runs_started_at ON reconciliation_runs (started_at DESC);

-- +goose Down
DROP TABLE reconciliation_runs;
