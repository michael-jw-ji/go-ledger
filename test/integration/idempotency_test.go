//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/michael-jw-ji/go-ledger/internal/idempotency"
)

func redisKey(clientKey string) string { return "idem:global:POST /transfers:" + clientKey }

func TestRedisReplaysStoredResponse(t *testing.T) {
	srv, _ := newServer(t, rdb)
	funding, alice := newAccount(t, true), newAccount(t, false)
	body := fmt.Sprintf(`{"debit_account_id": %d, "credit_account_id": %d, "amount": 40}`, funding.ID, alice.ID)
	key := uniq("idem")

	resp, first := do(t, srv, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first: %d %v", resp.StatusCode, first)
	}
	raw, err := rdb.Get(context.Background(), redisKey(key)).Bytes()
	if err != nil {
		t.Fatalf("redis key after success: %v", err)
	}
	var rec idempotency.Record
	if err := json.Unmarshal(raw, &rec); err != nil || rec.State != "done" || rec.Status != http.StatusCreated {
		t.Fatalf("redis record = %+v, %v", rec, err)
	}

	resp, second := do(t, srv, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotent-Replayed") != "true" || second["id"] != first["id"] {
		t.Fatalf("replay: %d replayed=%q id=%v want %v", resp.StatusCode,
			resp.Header.Get("Idempotent-Replayed"), second["id"], first["id"])
	}
	wantBalance(t, alice.ID, 40)
}

func TestInProgressKeyReturns409(t *testing.T) {
	ctx := context.Background()
	srv, _ := newServer(t, rdb)
	funding, alice := newAccount(t, true), newAccount(t, false)
	body := fmt.Sprintf(`{"debit_account_id": %d, "credit_account_id": %d, "amount": 40}`, funding.ID, alice.ID)
	key := uniq("idem")

	// Simulate another request holding the key.
	if _, _, err := idempotency.NewStore(rdb).Claim(ctx, redisKey(key), idempotency.Fingerprint([]byte(body))); err != nil {
		t.Fatal(err)
	}

	resp, out := do(t, srv, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusConflict || errCode(out) != "request_in_progress" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("in progress: %d %v", resp.StatusCode, out)
	}
	resp, out = do(t, srv, "POST", "/transfers", key, `{"debit_account_id": 1, "credit_account_id": 2, "amount": 1}`)
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(out) != "idempotency_key_reused" {
		t.Fatalf("different body: %d %v", resp.StatusCode, out)
	}
	wantBalance(t, alice.ID, 0)
}

func TestServerErrorReleasesKey(t *testing.T) {
	calls := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	mw := idempotency.NewMiddleware(idempotency.NewStore(rdb), quietLog())
	srv := httptest.NewServer(mw.Wrap(h))
	defer srv.Close()
	key := uniq("idem")

	for i, want := range []int{http.StatusInternalServerError, http.StatusCreated} {
		req, _ := http.NewRequest("POST", srv.URL+"/transfers", nil)
		req.Header.Set("Idempotency-Key", key)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("call %d: got %d, want %d (a 5xx must free the key for retry)", i+1, resp.StatusCode, want)
		}
	}
}

func TestReleaseOnlyRemovesOwnClaim(t *testing.T) {
	ctx := context.Background()
	store := idempotency.NewStore(rdb)
	key := uniq("idem:release")

	_, staleToken, err := store.Claim(ctx, key, []byte("h"))
	if err != nil {
		t.Fatal(err)
	}
	rdb.Del(ctx, key)                              // the stale claim expired...
	_, _, err = store.Claim(ctx, key, []byte("h")) // ...and another request took the key
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Release(ctx, key, staleToken); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.Exists(ctx, key).Result(); n != 1 {
		t.Fatal("a stale release deleted another request's claim")
	}
}

func TestRedisDownFallsBackToPostgres(t *testing.T) {
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer dead.Close() //nolint:errcheck
	srv, mw := newServer(t, dead)
	var mu sync.Mutex
	var results []string
	mw.OnLookup = func(r string) { mu.Lock(); results = append(results, r); mu.Unlock() }

	funding, alice := newAccount(t, true), newAccount(t, false)
	body := fmt.Sprintf(`{"debit_account_id": %d, "credit_account_id": %d, "amount": 75}`, funding.ID, alice.ID)
	key := uniq("idem")

	resp, first := do(t, srv, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first: %d %v", resp.StatusCode, first)
	}
	resp, second := do(t, srv, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotent-Replayed") != "true" || second["id"] != first["id"] {
		t.Fatalf("retry without redis: %d %v", resp.StatusCode, second)
	}
	wantBalance(t, alice.ID, 75)
	if len(results) != 2 || results[0] != idempotency.ResultRedisError {
		t.Errorf("lookup results = %v, want 2x redis_error", results)
	}
}
