//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/michael-jw-ji/go-ledger/internal/httpapi"
)

func do(t *testing.T, srv *httptest.Server, method, path, key, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
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
	if err := json.Unmarshal(raw, &out); err != nil && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("%s %s: bad JSON %q", method, path, raw)
	}
	return resp, out
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func TestHTTPTransferLifecycle(t *testing.T) {
	srv := httptest.NewServer(httpapi.New(svc, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()
	funding, alice := newAccount(t, true), newAccount(t, false)

	body := fmt.Sprintf(`{"debit_account_id": %d, "credit_account_id": %d, "amount": 700}`, funding.ID, alice.ID)
	key := uniq("http")

	resp, first := do(t, srv, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("first POST: %d %v", resp.StatusCode, first)
	}
	resp, second := do(t, srv, "POST", "/transfers", key, body)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("retry: %d replayed=%q", resp.StatusCode, resp.Header.Get("Idempotent-Replayed"))
	}
	if first["id"] != second["id"] {
		t.Fatalf("retry returned a different transfer: %v vs %v", first["id"], second["id"])
	}
	wantBalance(t, alice.ID, 700)

	resp, out := do(t, srv, "POST", "/transfers", key, fmt.Sprintf(
		`{"debit_account_id": %d, "credit_account_id": %d, "amount": 1}`, funding.ID, alice.ID))
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(out) != "idempotency_key_reused" {
		t.Fatalf("key reuse: %d %v", resp.StatusCode, out)
	}

	resp, out = do(t, srv, "POST", "/transfers", uniq("http"), fmt.Sprintf(
		`{"debit_account_id": %d, "credit_account_id": %d, "amount": 701}`, alice.ID, funding.ID))
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(out) != "insufficient_funds" {
		t.Fatalf("overdraft: %d %v", resp.StatusCode, out)
	}

	resp, out = do(t, srv, "POST", "/transfers", "", body)
	if resp.StatusCode != http.StatusBadRequest || errCode(out) != "missing_idempotency_key" {
		t.Fatalf("no key: %d %v", resp.StatusCode, out)
	}

	resp, out = do(t, srv, "POST", "/transfers", uniq("http"), fmt.Sprintf(
		`{"entries": [{"account_id": %d, "direction": "debit", "amount": 5},
		              {"account_id": %d, "direction": "credit", "amount": 4}]}`, funding.ID, alice.ID))
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(out) != "unbalanced" {
		t.Fatalf("unbalanced: %d %v", resp.StatusCode, out)
	}

	resp, out = do(t, srv, "POST", fmt.Sprintf("/transfers/%s/reverse", first["id"]), uniq("http"), "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("reverse: %d %v", resp.StatusCode, out)
	}
	wantBalance(t, alice.ID, 0)

	resp, out = do(t, srv, "GET", fmt.Sprintf("/accounts/%d/entries?limit=1", alice.ID), "", "")
	if resp.StatusCode != http.StatusOK || out["next_cursor"] == nil {
		t.Fatalf("entries page 1: %d %v", resp.StatusCode, out)
	}
	resp, out = do(t, srv, "GET", fmt.Sprintf("/accounts/%d/entries?limit=1&cursor=%s", alice.ID, out["next_cursor"]), "", "")
	if resp.StatusCode != http.StatusOK || out["next_cursor"] != nil || len(out["entries"].([]any)) != 1 {
		t.Fatalf("entries page 2: %d %v", resp.StatusCode, out)
	}
}
