//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michael-jw-ji/go-ledger/internal/httpapi"
	"github.com/michael-jw-ji/go-ledger/internal/idempotency"
	"github.com/michael-jw-ji/go-ledger/internal/metrics"
)

// The metrics are wired exactly as in cmd/ledger, then read back from the
// /metrics endpoint after a post, a Redis replay and a rejection.
func TestMetricsEndpoint(t *testing.T) {
	m := metrics.New(pool)
	idem := idempotency.NewMiddleware(idempotency.NewStore(rdb), quietLog())
	idem.OnLookup = m.OnIdempotencyLookup
	mux := httpapi.New(svc.WithHooks(m.LedgerHooks()), quietLog(), idem)
	mux.Handle("GET /metrics", m.Handler())
	srv := httptest.NewServer(m.Instrument(mux))
	t.Cleanup(srv.Close)

	src, dst := newAccount(t, true), newAccount(t, false)
	body := fmt.Sprintf(`{"debit_account_id":%d,"credit_account_id":%d,"amount":100}`, src.ID, dst.ID)
	key := uniq("k")
	do(t, srv, "POST", "/transfers", key, body) // posted
	do(t, srv, "POST", "/transfers", key, body) // replayed by Redis
	overdraw := fmt.Sprintf(`{"debit_account_id":%d,"credit_account_id":%d,"amount":999999}`, dst.ID, src.ID)
	do(t, srv, "POST", "/transfers", uniq("k"), overdraw) // rejected
	do(t, srv, "GET", fmt.Sprintf("/accounts/%d", dst.ID), "", "")

	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)

	for _, want := range []string{
		`ledger_transfers_total{status="posted"} 1`,
		`ledger_transfers_total{status="rejected"} 1`,
		// The replay never reached Postgres.
		`ledger_transfers_total{status="replayed"} 0`,
		`idempotency_lookups_total{result="miss"} 2`,
		`idempotency_lookups_total{result="hit"} 1`,
		`http_request_duration_seconds_count{code="201",method="POST",route="/transfers"} 2`,
		`http_request_duration_seconds_count{code="422",method="POST",route="/transfers"} 1`,
		// The route label is the pattern, not the raw path.
		`http_request_duration_seconds_count{code="200",method="GET",route="/accounts/{id}"} 1`,
		`ledger_lock_wait_seconds_count 2`,
		`pgxpool_max_conns `,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if t.Failed() {
		t.Log(grep(text, "ledger_", "idempotency_", "http_request"))
	}
}

func grep(text string, prefixes ...string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				out = append(out, line)
			}
		}
	}
	return strings.Join(out, "\n")
}
