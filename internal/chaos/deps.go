// Package chaos holds the demo's fault-injection endpoints: wipe a sandbox's
// idempotency cache, corrupt one of its balances (and repair it), and crash
// right after a transfer commits. They exist only in binaries built with
// `-tags chaos`. The default build compiles the no-op stubs in chaos_off.go,
// so production code can't reach them by configuration alone.
//
// Every endpoint is scoped to the caller's sandbox and refuses requests
// without one.
package chaos

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Deps are what the endpoints act on.
type Deps struct {
	Pool  *pgxpool.Pool
	Redis *redis.Client
	Log   *slog.Logger
}
