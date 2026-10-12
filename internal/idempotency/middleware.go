package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

const (
	Header         = "Idempotency-Key"
	ReplayedHeader = "Idempotent-Replayed"
	maxBodyBytes   = 1 << 20
)

// Lookup results, reported through Middleware.OnLookup.
const (
	ResultMiss       = "miss"
	ResultHit        = "hit"
	ResultInProgress = "in_progress"
	ResultMismatch   = "mismatch"
	ResultRedisError = "redis_error"
)

type Middleware struct {
	store *Store
	log   *slog.Logger
	// OnLookup, if set, is called once per keyed request with its result.
	OnLookup func(result string)
}

func NewMiddleware(store *Store, log *slog.Logger) *Middleware {
	return &Middleware{store: store, log: log}
}

// Wrap guards a POST handler:
//
//   - key free: claim it, run the handler, store the response (or release
//     the claim on 5xx so the client can retry).
//   - key done with the same request: replay the stored response.
//   - key in progress: 409, the client should retry shortly.
//   - key used with a different request: 422.
//   - Redis unavailable: run the handler anyway; Postgres still guarantees
//     the transfer posts at most once.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientKey := r.Header.Get(Header)
		if clientKey == "" {
			next.ServeHTTP(w, r) // the handler rejects it
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		// Scope the key to the endpoint, so one client key can be used for
		// different transfers' reversals.
		key := "idem:" + r.Method + " " + r.URL.Path + ":" + clientKey
		sum := sha256.Sum256(body)
		hash := sum[:]

		ctx := r.Context()
		existing, token, err := m.store.Claim(ctx, key, hash)
		if err != nil {
			m.report(ResultRedisError)
			m.log.WarnContext(ctx, "idempotency store unavailable, falling back to postgres", "err", err)
			next.ServeHTTP(w, r)
			return
		}
		if existing != nil {
			switch {
			case !bytes.Equal(existing.Hash, hash):
				m.report(ResultMismatch)
				writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused",
					"idempotency key was already used with a different request")
			case existing.State == stateInProgress:
				m.report(ResultInProgress)
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusConflict, "request_in_progress",
					"a request with this idempotency key is still being processed")
			default:
				m.report(ResultHit)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(ReplayedHeader, "true")
				w.WriteHeader(existing.Status)
				_, _ = w.Write(existing.Body)
			}
			return
		}

		m.report(ResultMiss)
		rec := &recorder{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Finish even if the client hung up: the transfer may have committed.
		storeCtx := context.WithoutCancel(ctx)
		if rec.status >= 500 {
			if err := m.store.Release(storeCtx, key, token); err != nil {
				m.log.WarnContext(ctx, "idempotency release failed", "err", err)
			}
		} else if err := m.store.Complete(storeCtx, key, hash, rec.status, rec.body.Bytes()); err != nil {
			// The in-progress claim expires on its own, then retries reach
			// the Postgres backstop.
			m.log.WarnContext(ctx, "idempotency complete failed", "err", err)
		}

		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())
	})
}

func (m *Middleware) report(result string) {
	if m.OnLookup != nil {
		m.OnLookup(result)
	}
}

// recorder buffers the handler's response so it can be stored before it is
// sent.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{"error": {"code": code, "message": msg}})
}
