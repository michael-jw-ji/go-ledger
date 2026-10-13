// Package outbox publishes ledger events written by the transactional
// outbox to a Redis stream.
//
// Each batch is claimed with FOR UPDATE SKIP LOCKED, published with XADD,
// and marked published in the same Postgres transaction. If the process dies
// after XADD but before the commit, the rows stay unpublished and the next
// run sends them again. Delivery is therefore at least once, never at most
// once: consumers dedupe on the event_id field, which is the outbox row id.
package outbox

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// DefaultStream is the Redis stream events are appended to.
const DefaultStream = "ledger.events"

// Event is one outbox row.
type Event struct {
	ID          int64
	Type        string
	AggregateID string
	Payload     []byte
	CreatedAt   time.Time
}

type Publisher struct {
	Pool   *pgxpool.Pool
	Redis  *redis.Client
	Stream string
	// Batch is the most rows claimed per transaction (default 100).
	Batch int
	// MaxLen caps the stream length approximately (default 1,000,000), so
	// an unread stream can't grow without bound. Consumers that fall
	// further behind than that must resync from Postgres.
	MaxLen int64

	// AfterPublish, if set, runs between XADD and marking the rows
	// published; an error aborts the batch as a crash there would. It exists
	// so tests can fail at the worst possible moment.
	AfterPublish func() error
}

// RunOnce publishes up to one batch and returns how many events it sent.
func (p *Publisher) RunOnce(ctx context.Context) (int, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// SKIP LOCKED lets several publishers drain the table without waiting
	// on, or double-sending, each other's batches.
	rows, err := tx.Query(ctx, `
		SELECT id, event_type, aggregate_id::text, payload, created_at
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, p.batch())
	if err != nil {
		return 0, err
	}
	events, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.Type, &e.AggregateID, &e.Payload, &e.CreatedAt)
		return e, err
	})
	if err != nil || len(events) == 0 {
		return 0, err
	}

	pipe := p.Redis.Pipeline()
	for _, e := range events {
		pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: p.stream(),
			MaxLen: p.maxLen(),
			Approx: true,
			Values: map[string]any{
				"event_id":     strconv.FormatInt(e.ID, 10),
				"type":         e.Type,
				"aggregate_id": e.AggregateID,
				"created_at":   e.CreatedAt.UTC().Format(time.RFC3339Nano),
				"payload":      string(e.Payload),
			},
		})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		// Some events may have been appended. They stay unpublished here
		// and go out again next run: a duplicate, never a loss.
		return 0, err
	}
	if p.AfterPublish != nil {
		if err := p.AfterPublish(); err != nil {
			return 0, err
		}
	}

	ids := make([]int64, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(events), nil
}

// Drain publishes batches until the outbox is empty.
func (p *Publisher) Drain(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := p.RunOnce(ctx)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}

func (p *Publisher) batch() int {
	if p.Batch > 0 {
		return p.Batch
	}
	return 100
}

func (p *Publisher) stream() string {
	if p.Stream != "" {
		return p.Stream
	}
	return DefaultStream
}

func (p *Publisher) maxLen() int64 {
	if p.MaxLen > 0 {
		return p.MaxLen
	}
	return 1_000_000
}

// Worker drains the outbox on an interval until its context ends.
type Worker struct {
	Publisher *Publisher
	Interval  time.Duration
	Log       *slog.Logger
	// OnTick, if set, receives each tick's published count and error.
	OnTick func(published int, err error)
}

func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := w.Publisher.Drain(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			w.Log.Warn("outbox publish failed, will retry", "err", err)
		}
		if w.OnTick != nil {
			w.OnTick(n, err)
		}
	}
}
