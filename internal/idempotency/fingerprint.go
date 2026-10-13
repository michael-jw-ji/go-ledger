package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
)

// Fingerprint identifies a request body for idempotency checks. Both layers
// use it: the middleware compares it in Redis, and the handler passes it to
// the ledger, which stores it in Postgres. One function means a retry gets
// the same verdict whether Redis still remembers the key or not.
//
// A JSON body is re-encoded first (object keys sorted, insignificant
// whitespace dropped, numbers kept as written), so formatting differences
// don't count as a different request. Anything else is hashed as is.
func Fingerprint(body []byte) []byte {
	canonical := body
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err == nil {
		if _, err := dec.Token(); errors.Is(err, io.EOF) {
			if b, err := json.Marshal(v); err == nil {
				canonical = b
			}
		}
	}
	sum := sha256.Sum256(canonical)
	return sum[:]
}

type recordedKey struct{}

// MarkRecorded tells the middleware that the response being written is an
// outcome Postgres has recorded under the request's key (posted, rejected,
// or replayed). Only those are cached in Redis. A response that never
// reached the ledger, such as malformed JSON or unbalanced legs, leaves the
// key free, because Postgres doesn't remember it either. Without a claim in
// the context (no middleware, or Redis down) it does nothing.
func MarkRecorded(ctx context.Context) {
	if f, ok := ctx.Value(recordedKey{}).(*atomic.Bool); ok {
		f.Store(true)
	}
}
