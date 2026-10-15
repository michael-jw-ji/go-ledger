//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/michael-jw-ji/go-ledger/internal/httpapi"
	"github.com/michael-jw-ji/go-ledger/internal/idempotency"
	"github.com/michael-jw-ji/go-ledger/internal/invariants"
	"github.com/michael-jw-ji/go-ledger/internal/ledger"
	"github.com/michael-jw-ji/go-ledger/internal/recon"
	"github.com/michael-jw-ji/go-ledger/internal/sandbox"
)

func TestSandboxAccountLimit(t *testing.T) {
	limited := svc.WithSandboxLimits(ledger.SandboxLimits{Accounts: 2})
	ctxA, _ := newSandbox()
	ctxB, _ := newSandbox()
	in := ledger.NewAccount{Name: "a", Currency: "USD", NormalBalance: ledger.Credit}
	for i := range 2 {
		if _, err := limited.CreateAccount(ctxA, in); err != nil {
			t.Fatalf("account %d: %v", i+1, err)
		}
	}
	if _, err := limited.CreateAccount(ctxA, in); !errors.Is(err, ledger.ErrSandboxLimit) {
		t.Fatalf("3rd account: err = %v, want ErrSandboxLimit", err)
	}
	if _, err := limited.CreateAccount(ctxB, in); err != nil {
		t.Fatalf("another sandbox hit the first one's limit: %v", err)
	}
	if _, err := limited.CreateAccount(context.Background(), in); err != nil {
		t.Fatalf("unscoped request was limited: %v", err)
	}
}

// At the transfer limit a new key is refused with 429 and stays free in
// both layers, while a retry of an existing key still replays.
func TestSandboxTransferLimit(t *testing.T) {
	ctx := context.Background()
	limited := svc.WithSandboxLimits(ledger.SandboxLimits{Transfers: 2})
	idem := idempotency.NewMiddleware(idempotency.NewStore(rdb), quietLog())
	srv := httptest.NewServer(sandbox.Middleware(httpapi.New(limited, quietLog(), idem)))
	t.Cleanup(srv.Close)

	sb := uuid.NewString()
	_, fund := postIn(t, srv, sb, "POST", "/accounts", "", `{"name":"F","currency":"USD","normal_balance":"credit","allow_negative":true}`)
	_, payee := postIn(t, srv, sb, "POST", "/accounts", "", `{"name":"P","currency":"USD","normal_balance":"credit"}`)
	body := fmt.Sprintf(`{"debit_account_id":%d,"credit_account_id":%d,"amount":100}`, idOf(fund), idOf(payee))

	first := uniq("limit")
	for _, key := range []string{first, uniq("limit")} {
		if resp, out := postIn(t, srv, sb, "POST", "/transfers", key, body); resp.StatusCode != http.StatusCreated {
			t.Fatalf("transfer under the limit = %d %v", resp.StatusCode, out)
		}
	}
	over := uniq("limit")
	resp, out := postIn(t, srv, sb, "POST", "/transfers", over, body)
	if resp.StatusCode != http.StatusTooManyRequests || errCode(out) != "sandbox_limit" {
		t.Fatalf("transfer over the limit = %d %v, want 429 sandbox_limit", resp.StatusCode, out)
	}
	if n, _ := rdb.Exists(ctx, "idem:"+sb+":POST /transfers:"+over).Result(); n != 0 {
		t.Fatal("the refused key was cached in Redis")
	}
	var recorded int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM transfers WHERE idempotency_key = $1`, "sb:"+sb+":"+over).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Fatal("the refused key was recorded in Postgres")
	}

	rdb.Del(ctx, "idem:"+sb+":POST /transfers:"+first) // force the Postgres path
	resp, _ = postIn(t, srv, sb, "POST", "/transfers", first, body)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get(idempotency.ReplayedHeader) != "true" {
		t.Fatalf("retry of an existing key at the limit = %d, want a 201 replay", resp.StatusCode)
	}
}

func TestSandboxReset(t *testing.T) {
	srv := httptest.NewServer(sandbox.Middleware(httpapi.New(svc, quietLog(), nil)))
	t.Cleanup(srv.Close)

	ctx, sb := newSandbox()
	inSandboxAccount(t, ctx, false)
	if resp, out := postIn(t, srv, sb, "POST", "/sandbox/reset", "", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %d %v", resp.StatusCode, out)
	}
	var archived bool
	if err := pool.QueryRow(context.Background(),
		`SELECT archived_at IS NOT NULL FROM sandboxes WHERE id = $1`, sb).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if !archived {
		t.Fatal("reset did not archive the sandbox")
	}

	resp, out := do(t, srv, "POST", "/sandbox/reset", "", "")
	if resp.StatusCode != http.StatusBadRequest || errCode(out) != "missing_sandbox" {
		t.Fatalf("reset without a sandbox = %d %v, want 400 missing_sandbox", resp.StatusCode, out)
	}
}

// Repair rebuilds a corrupted balance from entries. On a hot account the
// rebuilt cached balance must leave room for the pending rows, so the
// effective balance (cached + pending) equals the entries.
func TestRepairRebuildsBalancesIncludingHotPending(t *testing.T) {
	ctx := context.Background()
	ctxA, sbA := newSandbox()
	src := inSandboxAccount(t, ctxA, true)
	hot, err := svc.CreateAccount(ctxA, ledger.NewAccount{Name: uniq("hot"), Currency: "USD", NormalBalance: ledger.Credit, Hot: true})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if tr, err := svc.CreateTransfer(ctxA, ledger.TransferRequest{IdempotencyKey: uniq("k"), Legs: move(src.ID, hot.ID, 100)}); err != nil || tr.Status != ledger.StatusPosted {
			t.Fatalf("credit hot account: %v %s", err, tr.Status)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE accounts SET balance = balance + 5 WHERE id = $1`, hot.ID); err != nil {
		t.Fatal(err)
	}
	scope := recon.Scope{Sandbox: sbA}
	if rep, _ := invariants.Snapshot(ctx, pool, scope); len(rep.Recon.Mismatches) != 1 {
		t.Fatalf("corruption not detected: %+v", rep.Recon.Mismatches)
	}

	fixed, err := recon.Repair(ctx, pool, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fixed, []int64{hot.ID}) {
		t.Fatalf("repaired %v, want only %d", fixed, hot.ID)
	}
	rep, err := invariants.Snapshot(ctx, pool, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Recon.Mismatches) != 0 {
		t.Fatalf("still mismatched after repair: %+v", rep.Recon.Mismatches)
	}
	if got := mustAccount(t, ctxA, hot.ID).Balance; got != 300 {
		t.Fatalf("hot account effective balance = %d, want 300", got)
	}
	again, err := recon.Repair(ctx, pool, scope)
	if err != nil || len(again) != 0 {
		t.Fatalf("second repair changed %v (err %v), want nothing", again, err)
	}
}

// A sandbox marked chaos_active is invisible to the global worker's scope
// but not to its own, so a deliberate corruption never fires the real alert.
func TestGlobalReconSkipsChaosSandboxes(t *testing.T) {
	ctx := context.Background()
	ctxA, sbA := newSandbox()
	acct := inSandboxAccount(t, ctxA, false)
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET chaos_active = true WHERE id = $1`, sbA); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE accounts SET balance = balance + 9 WHERE id = $1`, acct.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE accounts SET balance = balance - 9 WHERE id = $1`, acct.ID)
	})

	global, err := recon.Run(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range global.Mismatches {
		if m.AccountID == acct.ID {
			t.Fatalf("global run reported the chaos sandbox's account: %+v", m)
		}
	}
	rep, err := invariants.Snapshot(ctx, pool, recon.Scope{Sandbox: sbA})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Recon.Mismatches) != 1 || rep.Recon.Mismatches[0].AccountID != acct.ID {
		t.Fatalf("sandbox's own check = %+v, want its one corrupted account", rep.Recon.Mismatches)
	}
}
