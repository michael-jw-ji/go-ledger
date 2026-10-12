//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michael-jw-ji/go-ledger/internal/ledger"
)

// The property tests hammer the HTTP API with random multi-leg transfers from
// many goroutines, retrying a share of requests with the same key (some
// concurrently), then check the ledger's invariants against the database.
//
// LEDGER_PROP_ITERS sets the number of transfers (default 1000).

func TestPropertyConcurrentTransfers(t *testing.T) {
	runProperty(t, false)
}

// Same run, but Redis is wiped every 100ms, so idempotency regularly falls
// through to the Postgres backstop mid-flight.
func TestPropertyConcurrentTransfersWithRedisFlush(t *testing.T) {
	runProperty(t, true)
}

const (
	propAccounts    = 10
	propWorkers     = 64
	propStartFunds  = 10_000
	propMaxTransfer = 4_000
	propRetryShare  = 0.20
)

type propOp struct {
	key  string
	legs []ledger.Leg
}

// outcome is what a client finally saw for one request.
type outcome struct {
	status     int
	transferID string
}

type propStats struct {
	requests, replayed, conflicts atomic.Int64
}

func runProperty(t *testing.T, flushRedis bool) {
	ctx := context.Background()
	iters := 1000
	if v := os.Getenv("LEDGER_PROP_ITERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("LEDGER_PROP_ITERS: %v", err)
		}
		iters = n
	}

	srv, _ := newServer(t, rdb)
	client := &http.Client{
		Transport: &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 256},
		Timeout:   30 * time.Second,
	}

	// Half the accounts are debit-normal, so both balance conventions are
	// exercised. None may go negative; funding may.
	funding := newAccount(t, true)
	accts := make([]ledger.Account, propAccounts)
	ids := make([]int64, 0, propAccounts+1)
	for i := range accts {
		normal := ledger.Credit
		if i%2 == 1 {
			normal = ledger.Debit
		}
		a, err := svc.CreateAccount(ctx, ledger.NewAccount{Name: uniq("prop"), Currency: "USD", NormalBalance: normal})
		if err != nil {
			t.Fatal(err)
		}
		accts[i] = a
		ids = append(ids, a.ID)
		// Increase the account: credit a credit-normal one, debit a debit-normal one.
		legs := []ledger.Leg{
			{AccountID: a.ID, Direction: normal, Amount: propStartFunds},
			{AccountID: funding.ID, Direction: normal.Opposite(), Amount: propStartFunds},
		}
		transfer(t, uniq("prop-fund"), legs)
	}
	ids = append(ids, funding.ID)
	startSum := signedSum(t, accts)

	ops := make([]propOp, iters)
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
	for i := range ops {
		ops[i] = propOp{key: uniq("prop"), legs: randomLegs(rng, accts)}
	}
	t.Logf("running %d transfers, %d workers, flushRedis=%v", iters, propWorkers, flushRedis)

	stopFlush := make(chan struct{})
	var flushes atomic.Int64
	var flushWG sync.WaitGroup
	if flushRedis {
		flushWG.Add(1)
		go func() {
			defer flushWG.Done()
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stopFlush:
					return
				case <-tick.C:
					if err := rdb.FlushAll(ctx).Err(); err == nil {
						flushes.Add(1)
					}
				}
			}
		}()
	}

	var stats propStats
	results := make([][]outcome, len(ops))
	var mu sync.Mutex
	record := func(i int, o outcome) {
		mu.Lock()
		results[i] = append(results[i], o)
		mu.Unlock()
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for w := range propWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), uint64(time.Now().UnixNano())))
			for i := range jobs {
				body := legsBody(ops[i].legs)
				send := func() {
					o, err := sendUntilSettled(client, srv.URL, ops[i].key, body, &stats)
					if err != nil {
						t.Error(err)
						return
					}
					record(i, o)
				}
				switch p := r.Float64(); {
				case p < propRetryShare/2: // duplicate in flight at the same time
					var dup sync.WaitGroup
					dup.Add(2)
					go func() { defer dup.Done(); send() }()
					go func() { defer dup.Done(); send() }()
					dup.Wait()
				case p < propRetryShare: // client retries after a response
					send()
					send()
				default:
					send()
				}
			}
		}()
	}
	for i := range ops {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)
	close(stopFlush)
	flushWG.Wait()
	if t.Failed() {
		t.FailNow()
	}

	// Every response for one key must agree.
	posted, rejected := 0, 0
	keys := make([]string, len(ops))
	for i, seen := range results {
		keys[i] = ops[i].key
		for _, o := range seen[1:] {
			if o != seen[0] {
				t.Fatalf("key %s got different answers: %+v", ops[i].key, seen)
			}
		}
		if seen[0].status == http.StatusCreated {
			posted++
		} else {
			rejected++
		}
	}

	// Each key maps to exactly one recorded transfer: the one the client saw,
	// with a full set of entries if posted and none if rejected.
	rows, err := pool.Query(ctx, `
		SELECT t.idempotency_key, t.id::text, t.status::text, count(e.id)
		FROM transfers t LEFT JOIN entries e ON e.transfer_id = t.id
		WHERE t.idempotency_key = ANY($1)
		GROUP BY t.id`, keys)
	if err != nil {
		t.Fatal(err)
	}
	type stored struct {
		id, status string
		entries    int
	}
	db := map[string]stored{}
	for rows.Next() {
		var k string
		var s stored
		if err := rows.Scan(&k, &s.id, &s.status, &s.entries); err != nil {
			t.Fatal(err)
		}
		db[k] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(db) != len(ops) {
		t.Fatalf("%d transfers recorded for %d keys", len(db), len(ops))
	}
	for i, op := range ops {
		s, o := db[op.key], results[i][0]
		if s.id != o.transferID {
			t.Fatalf("key %s: client saw transfer %s, db has %s", op.key, o.transferID, s.id)
		}
		switch {
		case o.status == http.StatusCreated && (s.status != "posted" || s.entries != len(op.legs)):
			t.Fatalf("key %s: posted with %d legs, db has %s with %d entries", op.key, len(op.legs), s.status, s.entries)
		case o.status != http.StatusCreated && (s.status != "rejected" || s.entries != 0):
			t.Fatalf("key %s: rejected, db has %s with %d entries", op.key, s.status, s.entries)
		}
	}

	// No transfer anywhere is unbalanced.
	var unbalanced int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT transfer_id FROM entries GROUP BY transfer_id
			HAVING sum(amount) FILTER (WHERE direction = 'debit')
			    <> sum(amount) FILTER (WHERE direction = 'credit')) u`).Scan(&unbalanced); err != nil {
		t.Fatal(err)
	}
	if unbalanced != 0 {
		t.Fatalf("%d unbalanced transfers", unbalanced)
	}

	// Money is conserved, and every cached balance matches its entries.
	if got := signedSum(t, accts); got != startSum {
		t.Fatalf("signed balance sum drifted: %d -> %d", startSum, got)
	}
	assertBalancesMatchEntries(t, ids)

	t.Logf("%d transfers in %s: %d posted, %d rejected; %d requests, %d replayed, %d got 409; %d redis flushes",
		len(ops), elapsed.Round(time.Millisecond), posted, rejected,
		stats.requests.Load(), stats.replayed.Load(), stats.conflicts.Load(), flushes.Load())
	if posted == 0 || rejected == 0 {
		t.Errorf("want both posted and rejected transfers, got %d and %d", posted, rejected)
	}
}

// randomLegs builds a balanced 2-4 leg transfer over distinct accounts.
func randomLegs(r *rand.Rand, accts []ledger.Account) []ledger.Leg {
	k := 2 + r.IntN(3)
	debits := 1 + r.IntN(k-1)
	total := max(int64(k), 1+r.Int64N(propMaxTransfer))
	perm := r.Perm(len(accts))[:k]

	debitAmts := split(r, total, debits)
	creditAmts := split(r, total, k-debits)
	legs := make([]ledger.Leg, 0, k)
	for i, idx := range perm {
		if i < debits {
			legs = append(legs, ledger.Leg{AccountID: accts[idx].ID, Direction: ledger.Debit, Amount: debitAmts[i]})
		} else {
			legs = append(legs, ledger.Leg{AccountID: accts[idx].ID, Direction: ledger.Credit, Amount: creditAmts[i-debits]})
		}
	}
	return legs
}

// split divides total into n positive parts.
func split(r *rand.Rand, total int64, n int) []int64 {
	parts := make([]int64, n)
	for i := range parts {
		parts[i] = 1
	}
	for rest := total - int64(n); rest > 0; {
		chunk := 1 + r.Int64N(rest)
		parts[r.IntN(n)] += chunk
		rest -= chunk
	}
	return parts
}

func legsBody(legs []ledger.Leg) []byte {
	b, err := json.Marshal(map[string]any{"entries": legs})
	if err != nil {
		panic(err)
	}
	return b
}

// sendUntilSettled posts until the answer is final, retrying 409 (another
// request with this key is in flight) like a well-behaved client.
func sendUntilSettled(c *http.Client, base, key string, body []byte, stats *propStats) (outcome, error) {
	for attempt := 0; attempt < 3000; attempt++ {
		req, err := http.NewRequest("POST", base+"/transfers", bytes.NewReader(body))
		if err != nil {
			return outcome{}, err
		}
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			return outcome{}, err
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return outcome{}, err
		}
		stats.requests.Add(1)
		if resp.Header.Get("Idempotent-Replayed") == "true" {
			stats.replayed.Add(1)
		}

		var out struct {
			ID    string `json:"id"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
			Transfer struct {
				ID string `json:"id"`
			} `json:"transfer"`
		}
		_ = json.Unmarshal(raw, &out)
		switch {
		case resp.StatusCode == http.StatusCreated:
			return outcome{status: resp.StatusCode, transferID: out.ID}, nil
		case resp.StatusCode == http.StatusUnprocessableEntity && out.Error.Code == string(ledger.CodeInsufficientFunds):
			return outcome{status: resp.StatusCode, transferID: out.Transfer.ID}, nil
		case resp.StatusCode == http.StatusConflict:
			stats.conflicts.Add(1)
			time.Sleep(5 * time.Millisecond)
		default:
			return outcome{}, fmt.Errorf("key %s: unexpected %d %s", key, resp.StatusCode, raw)
		}
	}
	return outcome{}, fmt.Errorf("key %s: still in progress after many retries", key)
}

// signedSum is Σ credit-normal balances − Σ debit-normal balances. Every
// balanced transfer between these accounts leaves it unchanged.
func signedSum(t *testing.T, accts []ledger.Account) int64 {
	t.Helper()
	var sum int64
	for _, a := range accts {
		b := balance(t, a.ID)
		if a.NormalBalance == ledger.Debit {
			b = -b
		}
		sum += b
	}
	return sum
}

func assertBalancesMatchEntries(t *testing.T, ids []int64) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT a.id, a.balance, a.lock_version,
		       COALESCE(sum(CASE WHEN e.direction = a.normal_balance THEN e.amount ELSE -e.amount END), 0),
		       count(e.id)
		FROM accounts a LEFT JOIN entries e ON e.account_id = a.id
		WHERE a.id = ANY($1)
		GROUP BY a.id`, ids)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id, bal, version, fromEntries, count int64
		if err := rows.Scan(&id, &bal, &version, &fromEntries, &count); err != nil {
			t.Fatal(err)
		}
		if bal != fromEntries {
			t.Errorf("account %d: cached balance %d, entries sum to %d", id, bal, fromEntries)
		}
		if version != count {
			t.Errorf("account %d: lock_version %d, %d entries", id, version, count)
		}
		if bal < 0 && id != ids[len(ids)-1] {
			t.Errorf("account %d went negative: %d", id, bal)
		}
		n++
	}
	if n != len(ids) {
		t.Errorf("checked %d accounts, want %d", n, len(ids))
	}
}
