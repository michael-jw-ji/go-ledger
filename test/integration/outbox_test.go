//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/michael-jw-ji/go-ledger/internal/ledger"
	"github.com/michael-jw-ji/go-ledger/internal/outbox"
)

// drainOutbox publishes whatever earlier tests left in the outbox to a
// scratch stream, so a test only sees its own events.
func drainOutbox(t *testing.T) {
	t.Helper()
	p := &outbox.Publisher{Pool: pool, Redis: rdb, Stream: uniq("scratch"), Batch: 1000}
	if _, err := p.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type streamEvent struct {
	EventID, Type, AggregateID string
	Transfer                   ledger.Transfer
}

func readStream(t *testing.T, stream string) []streamEvent {
	t.Helper()
	msgs, err := rdb.XRange(context.Background(), stream, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]streamEvent, 0, len(msgs))
	for _, m := range msgs {
		e := streamEvent{
			EventID:     m.Values["event_id"].(string),
			Type:        m.Values["type"].(string),
			AggregateID: m.Values["aggregate_id"].(string),
		}
		if err := json.Unmarshal([]byte(m.Values["payload"].(string)), &e.Transfer); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func unpublished(t *testing.T, ids []string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM outbox WHERE aggregate_id::text = ANY($1) AND published_at IS NULL`, ids).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An event exists exactly when money moved: one per posted transfer or
// reversal, none for a rejection, none for a replay.
func TestOutboxEventsCommitWithTransfers(t *testing.T) {
	ctx := context.Background()
	drainOutbox(t)
	funding, alice := newAccount(t, true), newAccount(t, false)

	key := uniq("k")
	pay := transfer(t, key, move(funding.ID, alice.ID, 500))
	transfer(t, key, move(funding.ID, alice.ID, 500)) // replay
	rejected := transfer(t, uniq("k"), move(alice.ID, funding.ID, 9999))
	rev, err := svc.ReverseTransfer(ctx, pay.ID, uniq("k"), nil)
	if err != nil {
		t.Fatal(err)
	}

	stream := uniq("events")
	p := &outbox.Publisher{Pool: pool, Redis: rdb, Stream: stream}
	if _, err := p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	events := readStream(t, stream)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (posted + reversed): %+v", len(events), events)
	}
	if e := events[0]; e.Type != ledger.EventTransferPosted || e.AggregateID != pay.ID.String() ||
		e.Transfer.ID != pay.ID || len(e.Transfer.Entries) != 2 {
		t.Errorf("first event = %+v", e)
	}
	if e := events[1]; e.Type != ledger.EventTransferReversed || e.AggregateID != rev.ID.String() ||
		e.Transfer.ReversesTransferID == nil || *e.Transfer.ReversesTransferID != pay.ID {
		t.Errorf("second event = %+v", e)
	}
	for _, e := range events {
		if e.AggregateID == rejected.ID.String() {
			t.Error("a rejected transfer published an event")
		}
	}
}

// The worst crash: events reached the stream but the process died before
// marking them published. They are sent again (a duplicate) and none is
// lost; deduping by event_id gives exactly one of each.
func TestOutboxCrashAfterPublishRedelivers(t *testing.T) {
	ctx := context.Background()
	drainOutbox(t)
	funding, alice := newAccount(t, true), newAccount(t, false)
	want := map[string]bool{}
	var ids []string
	for range 5 {
		tr := transfer(t, uniq("k"), move(funding.ID, alice.ID, 10))
		want[tr.ID.String()] = true
		ids = append(ids, tr.ID.String())
	}

	stream := uniq("events")
	crash := errors.New("simulated crash after XADD")
	p := &outbox.Publisher{Pool: pool, Redis: rdb, Stream: stream, AfterPublish: func() error { return crash }}
	if _, err := p.RunOnce(ctx); !errors.Is(err, crash) {
		t.Fatalf("crashing run: %v", err)
	}
	if n := len(readStream(t, stream)); n != 5 {
		t.Fatalf("stream has %d events after the crash, want 5", n)
	}
	if n := unpublished(t, ids); n != 5 {
		t.Fatalf("%d events still unpublished after the crash, want 5", n)
	}

	p.AfterPublish = nil // restarted
	if n, err := p.Drain(ctx); err != nil || n != 5 {
		t.Fatalf("drain after restart: %d, %v", n, err)
	}
	if n := unpublished(t, ids); n != 0 {
		t.Fatalf("%d events unpublished after the retry", n)
	}

	events := readStream(t, stream)
	if len(events) != 10 {
		t.Fatalf("stream has %d events, want 10 (each delivered twice)", len(events))
	}
	seen := map[string]bool{}
	for _, e := range events {
		if !want[e.AggregateID] {
			t.Fatalf("unexpected event %+v", e)
		}
		seen[e.EventID] = true // a consumer's dedupe
	}
	if len(seen) != 5 {
		t.Fatalf("%d distinct event ids after dedupe, want 5", len(seen))
	}
}

// Redis down: publishing fails, events wait in Postgres, nothing is lost.
func TestOutboxRedisDownKeepsEvents(t *testing.T) {
	ctx := context.Background()
	drainOutbox(t)
	funding, alice := newAccount(t, true), newAccount(t, false)
	tr := transfer(t, uniq("k"), move(funding.ID, alice.ID, 10))

	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer dead.Close() //nolint:errcheck
	if _, err := (&outbox.Publisher{Pool: pool, Redis: dead}).RunOnce(ctx); err == nil {
		t.Fatal("publishing to a dead Redis succeeded")
	}
	if n := unpublished(t, []string{tr.ID.String()}); n != 1 {
		t.Fatalf("event unpublished count = %d, want 1", n)
	}

	stream := uniq("events")
	if _, err := (&outbox.Publisher{Pool: pool, Redis: rdb, Stream: stream}).Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if events := readStream(t, stream); len(events) != 1 || events[0].AggregateID != tr.ID.String() {
		t.Fatalf("after Redis returned: %+v", events)
	}
}

// Several publishers draining at once (SKIP LOCKED) never send the same
// event twice when nothing crashes.
func TestOutboxConcurrentPublishersDontDuplicate(t *testing.T) {
	ctx := context.Background()
	drainOutbox(t)
	funding, alice := newAccount(t, true), newAccount(t, false)
	const n = 200
	for range n {
		transfer(t, uniq("k"), move(funding.ID, alice.ID, 1))
	}

	stream := uniq("events")
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			p := &outbox.Publisher{Pool: pool, Redis: rdb, Stream: stream, Batch: 7}
			if _, err := p.Drain(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	events := readStream(t, stream)
	seen := map[string]int{}
	for _, e := range events {
		seen[e.EventID]++
	}
	if len(events) != n || len(seen) != n {
		t.Fatalf("%d events, %d distinct, want %d of each", len(events), len(seen), n)
	}
}
