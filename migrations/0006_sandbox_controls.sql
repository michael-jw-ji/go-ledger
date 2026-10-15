-- +goose Up
-- archived_at: the visitor reset this sandbox. Entries are append-only, so
-- nothing is deleted; the console just moves to a fresh sandbox id.
-- chaos_active: a chaos endpoint has deliberately corrupted this sandbox.
-- The global reconciliation worker skips such sandboxes, so a visitor's
-- experiment never fires the real mismatch alert. Repair clears it.
ALTER TABLE sandboxes
    ADD COLUMN archived_at  timestamptz,
    ADD COLUMN chaos_active boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE sandboxes
    DROP COLUMN chaos_active,
    DROP COLUMN archived_at;
