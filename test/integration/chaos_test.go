//go:build integration && chaos

package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/michael-jw-ji/go-ledger/internal/chaos"
	"github.com/michael-jw-ji/go-ledger/internal/httpapi"
	"github.com/michael-jw-ji/go-ledger/internal/idempotency"
	"github.com/michael-jw-ji/go-ledger/internal/invariants"
	"github.com/michael-jw-ji/go-ledger/internal/recon"
	"github.com/michael-jw-ji/go-ledger/internal/sandbox"
)

// newChaosServer serves the API with the chaos routes and crash middleware,
// in the same order as main.
func newChaosServer(t *testing.T) *httptest.Server {
	t.Helper()
	idem := idempotency.NewMiddleware(idempotency.NewStore(rdb), quietLog())
	mux := httpapi.New(svc, quietLog(), idem)
	chaos.Register(mux, chaos.Deps{Pool: pool, Redis: rdb, Log: quietLog()})
	srv := httptest.NewServer(sandbox.Middleware(chaos.Wrap(mux)))
	t.Cleanup(srv.Close)
	return srv
}

// fundedPair creates a funding account and a payee in sandbox sb, and
// returns the two-leg body moving amount between them.
func fundedPair(t *testing.T, srv *httptest.Server, sb string, amount int) (body string, payee int64) {
	t.Helper()
	_, fund := postIn(t, srv, sb, "POST", "/accounts", "", `{"name":"F","currency":"USD","normal_balance":"credit","allow_negative":true}`)
	_, p := postIn(t, srv, sb, "POST", "/accounts", "", `{"name":"P","currency":"USD","normal_balance":"credit"}`)
	return fmt.Sprintf(`{"debit_account_id":%d,"credit_account_id":%d,"amount":%d}`, idOf(fund), idOf(p), amount), idOf(p)
}

func TestChaosFlushRedisOnlyTouchesItsSandbox(t *testing.T) {
	ctx := context.Background()
	srv := newChaosServer(t)
	sbA, sbB := uuid.NewString(), uuid.NewString()
	keyA, keyB := "idem:"+sbA+":POST /transfers:x", "idem:"+sbB+":POST /transfers:x"
	rdb.Set(ctx, keyA, "a", 0)
	rdb.Set(ctx, keyB, "b", 0)
	t.Cleanup(func() { rdb.Del(context.Background(), keyA, keyB) })

	resp, out := postIn(t, srv, sbA, "POST", "/chaos/flush-redis", "", "")
	if resp.StatusCode != http.StatusOK || out["deleted"] != float64(1) {
		t.Fatalf("flush = %d %v, want 200 deleted 1", resp.StatusCode, out)
	}
	if n, _ := rdb.Exists(ctx, keyA).Result(); n != 0 {
		t.Fatal("sandbox A's key survived its flush")
	}
	if n, _ := rdb.Exists(ctx, keyB).Result(); n != 1 {
		t.Fatal("flushing sandbox A deleted sandbox B's key")
	}
}

// After the cache is wiped, a retry of a posted key gets the original
// transfer from the Postgres backstop and money still moves once.
func TestChaosRedisWipedRetryReplaysFromPostgres(t *testing.T) {
	srv := newChaosServer(t)
	sb := uuid.NewString()
	body, payee := fundedPair(t, srv, sb, 250)
	key := uniq("wiped")

	resp, first := postIn(t, srv, sb, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first post = %d %v", resp.StatusCode, first)
	}
	postIn(t, srv, sb, "POST", "/chaos/flush-redis", "", "")
	resp, again := postIn(t, srv, sb, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get(idempotency.ReplayedHeader) != "true" || again["id"] != first["id"] {
		t.Fatalf("retry after flush = %d %v, want a 201 replay of %v", resp.StatusCode, again, first["id"])
	}
	if got := mustAccount(t, sandbox.With(context.Background(), sb), payee).Balance; got != 250 {
		t.Fatalf("payee balance = %d, want 250 (moved once)", got)
	}
}

// The crash: the transfer commits, the client gets no response, and the
// Redis claim is left in progress. A retry gets 409 until the claim expires
// (simulated here by flushing), then the original transfer.
func TestChaosCrashAfterCommit(t *testing.T) {
	ctx := context.Background()
	srv := newChaosServer(t)
	sb := uuid.NewString()
	body, payee := fundedPair(t, srv, sb, 400)
	key := uniq("crash")

	req, _ := http.NewRequest("POST", srv.URL+"/transfers", bytes.NewBufferString(body))
	req.Header.Set(sandbox.Header, sb)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set(chaos.CrashHeader, "after-commit")
	if resp, err := srv.Client().Do(req); err == nil {
		resp.Body.Close() //nolint:errcheck,gosec // unexpected path
		t.Fatalf("crash request got a response: %d", resp.StatusCode)
	}

	var committed int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM transfers WHERE idempotency_key = $1 AND status = 'posted'`,
		"sb:"+sb+":"+key).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != 1 {
		t.Fatalf("the crashed request committed %d transfers, want 1", committed)
	}

	resp, out := postIn(t, srv, sb, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusConflict || errCode(out) != "request_in_progress" {
		t.Fatalf("retry during the claim = %d %v, want 409 request_in_progress", resp.StatusCode, out)
	}

	postIn(t, srv, sb, "POST", "/chaos/flush-redis", "", "") // the claim expires
	resp, out = postIn(t, srv, sb, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get(idempotency.ReplayedHeader) != "true" {
		t.Fatalf("retry after the claim = %d %v, want a 201 replay", resp.StatusCode, out)
	}
	if got := mustAccount(t, sandbox.With(ctx, sb), payee).Balance; got != 400 {
		t.Fatalf("payee balance = %d, want 400 (moved once)", got)
	}
}

// Corruption is visible to the sandbox and hidden from the global worker;
// repair fixes it and hands the sandbox back to the worker.
func TestChaosCorruptAndRepair(t *testing.T) {
	ctx := context.Background()
	srv := newChaosServer(t)
	sb := uuid.NewString()
	body, payee := fundedPair(t, srv, sb, 1000)
	if resp, out := postIn(t, srv, sb, "POST", "/transfers", uniq("k"), body); resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup transfer = %d %v", resp.StatusCode, out)
	}

	if resp, out := postIn(t, srv, uuid.NewString(), "POST", "/chaos/corrupt-balance", "",
		fmt.Sprintf(`{"account_id":%d,"delta":700}`, payee)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("corrupting another sandbox's account = %d %v, want 404", resp.StatusCode, out)
	}
	resp, out := postIn(t, srv, sb, "POST", "/chaos/corrupt-balance", "", fmt.Sprintf(`{"account_id":%d,"delta":700}`, payee))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("corrupt = %d %v", resp.StatusCode, out)
	}
	t.Cleanup(func() { _, _ = recon.Repair(context.Background(), pool, recon.Scope{Sandbox: sb}) })

	rep, err := invariants.Snapshot(ctx, pool, recon.Scope{Sandbox: sb})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Recon.Mismatches) != 1 || rep.Recon.Mismatches[0].AccountID != payee ||
		rep.Recon.Mismatches[0].Cached-rep.Recon.Mismatches[0].Computed != 700 {
		t.Fatalf("sandbox findings = %+v, want account %d off by 700", rep.Recon.Mismatches, payee)
	}
	global, err := recon.Run(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range global.Mismatches {
		if m.AccountID == payee {
			t.Fatalf("the global run saw the demo corruption: %+v", m)
		}
	}

	resp, out = postIn(t, srv, sb, "POST", "/chaos/repair", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repair = %d %v", resp.StatusCode, out)
	}
	rep, err = invariants.Snapshot(ctx, pool, recon.Scope{Sandbox: sb})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Recon.Mismatches) != 0 {
		t.Fatalf("mismatches after repair: %+v", rep.Recon.Mismatches)
	}
	var active bool
	if err := pool.QueryRow(ctx, `SELECT chaos_active FROM sandboxes WHERE id = $1`, sb).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("repair left the sandbox excluded from global reconciliation")
	}
}

func TestChaosEndpointsNeedASandbox(t *testing.T) {
	srv := newChaosServer(t)
	for _, path := range []string{"/chaos/flush-redis", "/chaos/corrupt-balance", "/chaos/repair"} {
		resp, out := do(t, srv, "POST", path, "", `{"account_id":1,"delta":1}`)
		if resp.StatusCode != http.StatusBadRequest || errCode(out) != "missing_sandbox" {
			t.Errorf("%s without a sandbox = %d %v, want 400 missing_sandbox", path, resp.StatusCode, out)
		}
	}
}
