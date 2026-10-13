-- +goose Up
-- Transactional outbox. A posted transfer writes its event here in the same
-- transaction as its entries, so the event exists if and only if the money
-- moved. A worker publishes unpublished rows to a Redis stream and then
-- marks them. Delivery is at least once: a crash between publishing and
-- marking republishes the batch, and consumers dedupe by event_id.
CREATE TABLE outbox (
    id           bigserial PRIMARY KEY,
    event_type   text        NOT NULL,
    aggregate_id uuid        NOT NULL,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);

-- The worker only ever scans the unpublished tail.
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE outbox;
