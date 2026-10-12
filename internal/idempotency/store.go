// Package idempotency is the fast path for Idempotency-Key handling.
//
// Redis remembers each key's state (in progress, or done with the stored
// response). It is an optimization, not the source of truth: if Redis loses a
// key, the request falls through to Postgres, whose UNIQUE idempotency_key
// returns the original result. See DESIGN.md.
package idempotency

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	stateInProgress = "in_progress"
	stateDone       = "done"
)

// Record is what Redis holds for a key.
type Record struct {
	State string `json:"state"`
	Hash  []byte `json:"hash"`
	// Token identifies the request holding an in-progress key, so it only
	// releases its own claim.
	Token  string `json:"token,omitempty"`
	Status int    `json:"status,omitempty"`
	Body   []byte `json:"body,omitempty"`
}

type Store struct {
	rdb *redis.Client
	// InProgressTTL bounds how long a crashed request can wedge its key
	// (clients get 409 until it expires, then fall through to Postgres).
	InProgressTTL time.Duration
	// DoneTTL is how long a finished response is replayed from Redis.
	DoneTTL time.Duration
}

func NewStore(rdb *redis.Client) *Store {
	return &Store{rdb: rdb, InProgressTTL: 30 * time.Second, DoneTTL: 24 * time.Hour}
}

// Claim marks the key in progress if it is free. If it is taken, it returns
// the existing record instead. The returned token releases the claim.
func (s *Store) Claim(ctx context.Context, key string, hash []byte) (existing *Record, token string, err error) {
	token = newToken()
	val, err := json.Marshal(Record{State: stateInProgress, Hash: hash, Token: token})
	if err != nil {
		return nil, "", err
	}
	// Two tries: the existing key can expire between SET NX and GET.
	for range 2 {
		ok, err := s.rdb.SetNX(ctx, key, val, s.InProgressTTL).Result()
		if err != nil {
			return nil, "", err
		}
		if ok {
			return nil, token, nil
		}
		raw, err := s.rdb.Get(ctx, key).Bytes()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, "", err
		}
		return &rec, "", nil
	}
	return nil, "", errors.New("idempotency: key churned during claim")
}

// Complete stores the final response for replay.
func (s *Store) Complete(ctx context.Context, key string, hash []byte, status int, body []byte) error {
	val, err := json.Marshal(Record{State: stateDone, Hash: hash, Status: status, Body: body})
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, key, val, s.DoneTTL).Err()
}

// releaseScript deletes the key only if it still holds our in-progress claim,
// so a slow request whose claim expired can't delete someone else's.
var releaseScript = redis.NewScript(`
local v = redis.call("GET", KEYS[1])
if not v then return 0 end
local rec = cjson.decode(v)
if rec.state == "in_progress" and rec.token == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

// Release frees an in-progress claim so the client can retry, used when the
// request failed in a way that is safe to retry (5xx).
func (s *Store) Release(ctx context.Context, key, token string) error {
	return releaseScript.Run(ctx, s.rdb, []string{key}, token).Err()
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
