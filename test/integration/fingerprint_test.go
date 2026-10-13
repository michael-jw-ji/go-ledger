//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/michael-jw-ji/go-ledger/internal/idempotency"
)

// A retry that differs only in formatting is the same request, and both
// layers must say so: Redis while it remembers the key, Postgres after it
// forgets. A real change is rejected by both.
func TestReformattedRetryGetsSameAnswerFromBothLayers(t *testing.T) {
	ctx := context.Background()
	srv, _ := newServer(t, rdb)
	funding, alice := newAccount(t, true), newAccount(t, false)
	compact := fmt.Sprintf(`{"debit_account_id":%d,"credit_account_id":%d,"amount":40}`, funding.ID, alice.ID)
	pretty := fmt.Sprintf("{\n  \"amount\": 40,\n  \"credit_account_id\": %d,\n  \"debit_account_id\": %d\n}\n", alice.ID, funding.ID)
	changed := fmt.Sprintf(`{"debit_account_id":%d,"credit_account_id":%d,"amount":41}`, funding.ID, alice.ID)
	// The same legs in the explicit form are a different request: a retry
	// must resend what it sent. Both layers have to agree on that too.
	explicit := fmt.Sprintf(`{"entries":[{"account_id":%d,"direction":"debit","amount":40},{"account_id":%d,"direction":"credit","amount":40}]}`,
		funding.ID, alice.ID)
	key := uniq("idem")

	resp, first := do(t, srv, "POST", "/transfers", key, compact)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first: %d %v", resp.StatusCode, first)
	}
	for _, layer := range []string{"redis", "postgres"} {
		if layer == "postgres" {
			rdb.Del(ctx, redisKey(key)) // Redis forgot the key
		}
		resp, out := do(t, srv, "POST", "/transfers", key, pretty)
		if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotent-Replayed") != "true" || out["id"] != first["id"] {
			t.Fatalf("%s, reformatted retry: %d %v", layer, resp.StatusCode, out)
		}
		if layer == "postgres" {
			rdb.Del(ctx, redisKey(key)) // the replay re-cached it; forget again
		}
		for name, body := range map[string]string{"changed amount": changed, "explicit form": explicit} {
			if layer == "postgres" {
				rdb.Del(ctx, redisKey(key))
			}
			resp, out = do(t, srv, "POST", "/transfers", key, body)
			if resp.StatusCode != http.StatusUnprocessableEntity || errCode(out) != "idempotency_key_reused" {
				t.Fatalf("%s, %s: %d %v", layer, name, resp.StatusCode, out)
			}
		}
	}
	wantBalance(t, alice.ID, 40)
}

// A request rejected before it reached the ledger isn't recorded in
// Postgres, so Redis must not remember it either: the corrected request
// posts under the same key.
func TestRejectedBeforeLedgerDoesNotBurnKey(t *testing.T) {
	ctx := context.Background()
	srv, _ := newServer(t, rdb)
	funding, alice := newAccount(t, true), newAccount(t, false)
	key := uniq("idem")

	r, body := do(t, srv, "POST", "/transfers", key, `{"debit_account_id": 1,`)
	if r.StatusCode != http.StatusBadRequest || errCode(body) != "malformed_json" {
		t.Fatalf("malformed: %d %v", r.StatusCode, body)
	}
	unbalanced := fmt.Sprintf(`{"entries":[{"account_id":%d,"direction":"debit","amount":5},{"account_id":%d,"direction":"credit","amount":4}]}`,
		funding.ID, alice.ID)
	r, body = do(t, srv, "POST", "/transfers", key, unbalanced)
	if r.StatusCode != http.StatusUnprocessableEntity || errCode(body) != "unbalanced" {
		t.Fatalf("unbalanced: %d %v", r.StatusCode, body)
	}
	if n, _ := rdb.Exists(ctx, redisKey(key)).Result(); n != 0 {
		t.Fatal("redis kept a key that postgres never recorded")
	}

	good := fmt.Sprintf(`{"debit_account_id":%d,"credit_account_id":%d,"amount":5}`, funding.ID, alice.ID)
	r, body = do(t, srv, "POST", "/transfers", key, good)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("corrected request: %d %v", r.StatusCode, body)
	}
	wantBalance(t, alice.ID, 5)
}

// Business rejections are recorded in Postgres, so Redis caches them.
func TestBusinessRejectionIsCached(t *testing.T) {
	srv, _ := newServer(t, rdb)
	alice, bob := newAccount(t, false), newAccount(t, false)
	key := uniq("idem")
	body := fmt.Sprintf(`{"debit_account_id":%d,"credit_account_id":%d,"amount":5}`, alice.ID, bob.ID)

	resp, out := do(t, srv, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(out) != "insufficient_funds" {
		t.Fatalf("overdraft: %d %v", resp.StatusCode, out)
	}
	if rec := getRecord(t, redisKey(key)); rec.State != "done" || rec.Status != http.StatusUnprocessableEntity {
		t.Fatalf("redis record = %+v", rec)
	}
}

// A reversal's body is part of its fingerprint in both layers too.
func TestReversalFingerprintMatchesAcrossLayers(t *testing.T) {
	ctx := context.Background()
	srv, _ := newServer(t, rdb)
	funding, alice := newAccount(t, true), newAccount(t, false)
	pay := transfer(t, uniq("k"), move(funding.ID, alice.ID, 30))
	path := "/transfers/" + pay.ID.String() + "/reverse"
	key := uniq("idem")
	rkey := "idem:POST " + path + ":" + key

	resp, first := do(t, srv, "POST", path, key, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("reverse: %d %v", resp.StatusCode, first)
	}
	for _, layer := range []string{"redis", "postgres"} {
		if layer == "postgres" {
			rdb.Del(ctx, rkey)
		}
		resp, out := do(t, srv, "POST", path, key, `{"note":"different"}`)
		if resp.StatusCode != http.StatusUnprocessableEntity || errCode(out) != "idempotency_key_reused" {
			t.Fatalf("%s, different body: %d %v", layer, resp.StatusCode, out)
		}
		resp, out = do(t, srv, "POST", path, key, "")
		if resp.StatusCode != http.StatusCreated || out["id"] != first["id"] {
			t.Fatalf("%s, same body: %d %v", layer, resp.StatusCode, out)
		}
	}
	wantBalance(t, alice.ID, 0)
}

// Complete only overwrites its own claim: if a slow request's claim expired
// and another request now holds the key, the slow one leaves it alone.
func TestCompleteOnlyOverwritesOwnClaim(t *testing.T) {
	ctx := context.Background()
	store := idempotency.NewStore(rdb)
	key := uniq("idem:complete")

	_, staleToken, err := store.Claim(ctx, key, []byte("h"))
	if err != nil {
		t.Fatal(err)
	}
	rdb.Del(ctx, key) // the stale claim expired...
	_, token, err := store.Claim(ctx, key, []byte("h"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, key, staleToken, []byte("h"), http.StatusCreated, []byte(`{"stale":true}`)); err != nil {
		t.Fatal(err)
	}
	if rec := getRecord(t, key); rec.State != "in_progress" || rec.Token != token {
		t.Fatalf("a stale complete overwrote another request's claim: %+v", rec)
	}

	if err := store.Complete(ctx, key, token, []byte("h"), http.StatusCreated, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if rec := getRecord(t, key); rec.State != "done" || string(rec.Body) != `{"ok":true}` {
		t.Fatalf("own complete not stored: %+v", rec)
	}

	// An expired key that nobody holds still takes a completed result.
	rdb.Del(ctx, key)
	if err := store.Complete(ctx, key, staleToken, []byte("h"), http.StatusCreated, []byte(`{"late":true}`)); err != nil {
		t.Fatal(err)
	}
	if rec := getRecord(t, key); rec.State != "done" {
		t.Fatalf("complete on an expired key: %+v", rec)
	}
}

func getRecord(t *testing.T, key string) idempotency.Record {
	t.Helper()
	raw, err := rdb.Get(context.Background(), key).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var rec idempotency.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}
