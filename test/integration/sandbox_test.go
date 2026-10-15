//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/michael-jw-ji/go-ledger/internal/httpapi"
	"github.com/michael-jw-ji/go-ledger/internal/idempotency"
	"github.com/michael-jw-ji/go-ledger/internal/invariants"
	"github.com/michael-jw-ji/go-ledger/internal/ledger"
	"github.com/michael-jw-ji/go-ledger/internal/recon"
	"github.com/michael-jw-ji/go-ledger/internal/sandbox"
)

// newSandbox returns a context scoped to a fresh sandbox, and its id.
func newSandbox() (context.Context, string) {
	id := uuid.NewString()
	return sandbox.With(context.Background(), id), id
}

func inSandboxAccount(t *testing.T, ctx context.Context, allowNegative bool) ledger.Account {
	t.Helper()
	a, err := svc.CreateAccount(ctx, ledger.NewAccount{
		Name:          uniq(t.Name()),
		Currency:      "USD",
		NormalBalance: ledger.Credit,
		AllowNegative: allowNegative,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return a
}

func TestSandboxesSeeOnlyTheirOwnAccounts(t *testing.T) {
	ctxA, _ := newSandbox()
	ctxB, _ := newSandbox()
	a := inSandboxAccount(t, ctxA, false)
	b := inSandboxAccount(t, ctxB, false)
	legacy := newAccount(t, false)

	listA, err := svc.ListAccounts(ctxA)
	if err != nil {
		t.Fatal(err)
	}
	var sawA bool
	for _, acct := range listA {
		if acct.ID == b.ID || acct.ID == legacy.ID {
			t.Fatalf("sandbox A lists account %d from another sandbox or the unscoped data", acct.ID)
		}
		sawA = sawA || acct.ID == a.ID
	}
	if !sawA {
		t.Fatalf("sandbox A does not list its own account %d", a.ID)
	}

	if _, err := svc.GetAccount(ctxB, a.ID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("sandbox B reads sandbox A's account: err = %v", err)
	}
	if _, err := svc.GetAccount(ctxA, legacy.ID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("sandbox A reads unscoped account %d: err = %v", legacy.ID, err)
	}
	if _, err := svc.GetAccount(context.Background(), a.ID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("unscoped request reads sandbox A's account: err = %v", err)
	}
}

func TestCrossSandboxTransferIsRejected(t *testing.T) {
	ctxA, _ := newSandbox()
	ctxB, _ := newSandbox()
	src := inSandboxAccount(t, ctxA, true) // overdraft allowed, so only the sandbox check can reject
	dst := inSandboxAccount(t, ctxB, false)

	tr, err := svc.CreateTransfer(ctxA, ledger.TransferRequest{IdempotencyKey: uniq("k"), Legs: move(src.ID, dst.ID, 100)})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != ledger.StatusRejected || tr.ErrorCode != ledger.CodeAccountNotFound {
		t.Fatalf("cross-sandbox transfer = %s/%s, want rejected/account_not_found", tr.Status, tr.ErrorCode)
	}
	if got := mustAccount(t, ctxB, dst.ID).Balance; got != 0 {
		t.Fatalf("destination moved to %d despite the rejection", got)
	}
}

func mustAccount(t *testing.T, ctx context.Context, id int64) ledger.Account {
	t.Helper()
	a, err := svc.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("get account %d: %v", id, err)
	}
	return a
}

// The same client key in two sandboxes must not collide, in Postgres (the
// transfers' unique key) or in Redis (the cached response). Each sandbox
// keeps its own cached response.
func TestSameKeyInTwoSandboxesPostsBoth(t *testing.T) {
	ctx := context.Background()
	idem := idempotency.NewMiddleware(idempotency.NewStore(rdb), quietLog())
	srv := httptest.NewServer(sandbox.Middleware(httpapi.New(svc, quietLog(), idem)))
	t.Cleanup(srv.Close)

	key := uniq("shared")
	transferIDs := map[string]string{}
	sandboxes := []string{uuid.NewString(), uuid.NewString()}
	for _, sb := range sandboxes {
		_, fund := postIn(t, srv, sb, "POST", "/accounts", "",
			`{"name":"Funding","currency":"USD","normal_balance":"credit","allow_negative":true}`)
		_, alice := postIn(t, srv, sb, "POST", "/accounts", "",
			`{"name":"Alice","currency":"USD","normal_balance":"credit"}`)
		body := fmt.Sprintf(`{"debit_account_id":%d,"credit_account_id":%d,"amount":500}`, idOf(fund), idOf(alice))
		resp, out := postIn(t, srv, sb, "POST", "/transfers", key, body)
		if resp.StatusCode != http.StatusCreated || resp.Header.Get(idempotency.ReplayedHeader) == "true" {
			t.Fatalf("sandbox %s: transfer = %d replayed=%q, want a fresh 201",
				sb, resp.StatusCode, resp.Header.Get(idempotency.ReplayedHeader))
		}
		transferIDs[sb], _ = out["id"].(string)
	}
	if transferIDs[sandboxes[0]] == transferIDs[sandboxes[1]] {
		t.Fatalf("both sandboxes got transfer %s", transferIDs[sandboxes[0]])
	}
	for _, sb := range sandboxes {
		n, err := rdb.Exists(ctx, "idem:"+sb+":POST /transfers:"+key).Result()
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("sandbox %s has no cached response under its own key", sb)
		}
		rdb.Del(ctx, "idem:"+sb+":POST /transfers:"+key) //nolint:errcheck // cleanup
	}
}

func TestInvalidSandboxHeaderIsRejected(t *testing.T) {
	srv := httptest.NewServer(sandbox.Middleware(httpapi.New(svc, quietLog(), nil)))
	t.Cleanup(srv.Close)
	resp, out := postIn(t, srv, "not-a-uuid", "GET", "/accounts", "", "")
	if resp.StatusCode != http.StatusBadRequest || errCode(out) != "invalid_sandbox" {
		t.Fatalf("bad sandbox header = %d %v, want 400 invalid_sandbox", resp.StatusCode, out)
	}
}

// Corrupting a balance in one sandbox must show up in that sandbox's report
// and nowhere else. The restore runs in cleanup, so the global reconciliation
// checks in the other tests stay clean.
func TestScopedReconIgnoresOtherSandboxes(t *testing.T) {
	ctx := context.Background()
	ctxA, sbA := newSandbox()
	ctxB, sbB := newSandbox()
	src := inSandboxAccount(t, ctxA, true)
	dst := inSandboxAccount(t, ctxA, false)
	inSandboxAccount(t, ctxB, false)
	if tr, err := svc.CreateTransfer(ctxA, ledger.TransferRequest{IdempotencyKey: uniq("k"), Legs: move(src.ID, dst.ID, 1000)}); err != nil || tr.Status != ledger.StatusPosted {
		t.Fatalf("setup transfer: %v %s", err, tr.Status)
	}

	if _, err := pool.Exec(ctx, `UPDATE accounts SET balance = balance + 7 WHERE id = $1`, dst.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE accounts SET balance = balance - 7 WHERE id = $1`, dst.ID)
	})

	repA, err := invariants.Snapshot(ctx, pool, recon.Scope{Sandbox: sbA})
	if err != nil {
		t.Fatal(err)
	}
	if len(repA.Recon.Mismatches) != 1 {
		t.Fatalf("sandbox A mismatches = %+v, want exactly one", repA.Recon.Mismatches)
	}
	m := repA.Recon.Mismatches[0]
	if m.Kind != recon.KindAccount || m.AccountID != dst.ID || m.Cached-m.Computed != 7 {
		t.Fatalf("sandbox A mismatch = %+v, want account %d drifted by 7", m, dst.ID)
	}

	repB, err := invariants.Snapshot(ctx, pool, recon.Scope{Sandbox: sbB})
	if err != nil {
		t.Fatal(err)
	}
	if len(repB.Recon.Mismatches) != 0 {
		t.Fatalf("sandbox B mismatches = %+v, want none", repB.Recon.Mismatches)
	}
}

// A snapshot reports the sandbox's figures and writes nothing to
// reconciliation_runs, so the console's polling leaves the global history
// alone.
func TestInvariantsSnapshotIsScopedAndRecordsNothing(t *testing.T) {
	ctx := context.Background()
	ctxA, sbA := newSandbox()
	src := inSandboxAccount(t, ctxA, true)
	dst := inSandboxAccount(t, ctxA, false)
	if tr, err := svc.CreateTransfer(ctxA, ledger.TransferRequest{IdempotencyKey: uniq("k"), Legs: move(src.ID, dst.ID, 1000)}); err != nil || tr.Status != ledger.StatusPosted {
		t.Fatalf("setup transfer: %v %s", err, tr.Status)
	}
	runs := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM reconciliation_runs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := runs()

	rep, err := invariants.Snapshot(ctx, pool, recon.Scope{Sandbox: sbA})
	if err != nil {
		t.Fatal(err)
	}
	if after := runs(); after != before {
		t.Fatalf("snapshot recorded %d reconciliation runs", after-before)
	}
	if rep.Accounts != 2 {
		t.Fatalf("accounts = %d, want 2", rep.Accounts)
	}
	for _, tb := range rep.Trial {
		if tb.Difference != 0 {
			t.Fatalf("trial balance %s is off by %d", tb.Currency, tb.Difference)
		}
	}
	if len(rep.Held) != 1 || rep.Held[0].Amount != 1000 {
		t.Fatalf("held = %+v, want 1000 in USD (the funding account is excluded)", rep.Held)
	}
	if rep.KeysWithMultipleTransfers != 0 || len(rep.Recon.Mismatches) != 0 {
		t.Fatalf("clean sandbox reports problems: %+v", rep)
	}
}

// postIn is do with a sandbox header. An empty key sends no Idempotency-Key.
func postIn(t *testing.T, srv *httptest.Server, sb, method, path, key, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(sandbox.Header, sb)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func idOf(out map[string]any) int64 {
	f, _ := out["id"].(float64)
	return int64(f)
}
