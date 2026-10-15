//go:build chaos

package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/michael-jw-ji/go-ledger/internal/idempotency"
	"github.com/michael-jw-ji/go-ledger/internal/recon"
	"github.com/michael-jw-ji/go-ledger/internal/sandbox"
)

// Enabled reports whether this binary was built with the chaos endpoints.
const Enabled = true

// CrashHeader, set to "after-commit" on POST /transfers, makes the server
// commit the transfer and then drop the connection without answering.
const CrashHeader = "X-Chaos-Crash"

// Register adds the chaos routes:
//
//	GET  /chaos                  which faults this server can inject
//	POST /chaos/flush-redis      delete the sandbox's idempotency keys from Redis
//	POST /chaos/corrupt-balance  {account_id, delta}: add delta to a cached balance
//	POST /chaos/repair           rebuild the sandbox's balances from entries
func Register(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /chaos", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "crash_header": CrashHeader})
	})
	mux.HandleFunc("POST /chaos/flush-redis", d.flushRedis)
	mux.HandleFunc("POST /chaos/corrupt-balance", d.corruptBalance)
	mux.HandleFunc("POST /chaos/repair", d.repair)
}

// flushRedis deletes only this sandbox's keys, never the whole database: the
// same thing a Redis restart does to it, without touching other visitors.
func (d Deps) flushRedis(w http.ResponseWriter, r *http.Request) {
	sb, ok := requireSandbox(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var deleted int64
	iter := d.Redis.Scan(ctx, 0, "idem:"+sb+":*", 100).Iterator()
	var batch []string
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := d.Redis.Del(ctx, batch...).Result()
		deleted += n
		batch = batch[:0]
		return err
	}
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) == 100 {
			if err := flush(); err != nil {
				d.fail(w, r, "flush redis", err)
				return
			}
		}
	}
	if err := iter.Err(); err != nil {
		d.fail(w, r, "scan redis", err)
		return
	}
	if err := flush(); err != nil {
		d.fail(w, r, "flush redis", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

// corruptBalance edits a cached balance behind the ledger's back, the way a
// bad migration or a manual fix would. The sandbox is marked chaos_active in
// the same transaction, so the global reconciliation worker never sees the
// drift and the real alert stays quiet; the sandbox's own invariants show it.
func (d Deps) corruptBalance(w http.ResponseWriter, r *http.Request) {
	sb, ok := requireSandbox(w, r)
	if !ok {
		return
	}
	var in struct {
		AccountID int64 `json:"account_id"`
		Delta     int64 `json:"delta"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<12))
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		err = dec.Decode(&in)
	}
	if err != nil || in.AccountID <= 0 || in.Delta == 0 {
		writeError(w, http.StatusBadRequest, "malformed_json", `body must be {"account_id": <id>, "delta": <non-zero amount>}`)
		return
	}

	ctx := r.Context()
	err = func() error {
		tx, err := d.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
		if _, err := tx.Exec(ctx, `UPDATE sandboxes SET chaos_active = true WHERE id = $1`, sb); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE accounts SET balance = balance + $1
			WHERE id = $2 AND sandbox_id = $3`, in.Delta, in.AccountID, sb)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNotFound
		}
		return tx.Commit(ctx)
	}()
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such account in this sandbox")
	case err != nil:
		d.fail(w, r, "corrupt balance", err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"account_id": in.AccountID, "delta": in.Delta})
	}
}

var errNotFound = errors.New("not found")

// repair rebuilds the sandbox's cached balances from its entries, then lets
// the global worker check the sandbox again.
func (d Deps) repair(w http.ResponseWriter, r *http.Request) {
	sb, ok := requireSandbox(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	fixed, err := recon.Repair(ctx, d.Pool, recon.Scope{Sandbox: sb})
	if err != nil {
		d.fail(w, r, "repair", err)
		return
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE sandboxes SET chaos_active = false WHERE id = $1`, sb); err != nil {
		d.fail(w, r, "clear chaos flag", err)
		return
	}
	if fixed == nil {
		fixed = []int64{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"repaired": fixed})
}

// Wrap simulates a crash between commit and response. A POST /transfers with
// CrashHeader: after-commit runs normally, so the transfer commits, but the
// response is thrown away and the connection is closed. The idempotency
// claim is left in progress, as a crashed server would leave it: retries get
// 409 until it expires, then the Postgres backstop replays the transfer.
func Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/transfers" ||
			r.Header.Get(CrashHeader) != "after-commit" || sandbox.ID(r.Context()) == "" {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(discard{header: http.Header{}}, r.WithContext(idempotency.Abandon(r.Context())))
		// net/http closes the connection without a response and without
		// logging a stack trace.
		panic(http.ErrAbortHandler)
	})
}

// discard is a ResponseWriter that sends nothing.
type discard struct{ header http.Header }

func (d discard) Header() http.Header       { return d.header }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

func requireSandbox(w http.ResponseWriter, r *http.Request) (string, bool) {
	sb := sandbox.ID(r.Context())
	if sb == "" {
		writeError(w, http.StatusBadRequest, "missing_sandbox", "chaos endpoints need the "+sandbox.Header+" header")
		return "", false
	}
	return sb, true
}

func (d Deps) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	if !errors.Is(err, context.Canceled) {
		d.Log.ErrorContext(r.Context(), "chaos "+what, "err", err)
	}
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]map[string]string{"error": {"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
