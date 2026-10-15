// Package sandbox scopes a request to one visitor's data. The public console
// sends a random sandbox id on every request, so visitors can't see or change
// each other's accounts. Requests without the header are unscoped, the way the
// API worked before sandboxes existed.
package sandbox

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

// Header carries the sandbox id. It must be a UUID.
const Header = "X-Sandbox-Id"

type ctxKey struct{}

// With returns a copy of ctx scoped to sandbox id. An empty id is unscoped.
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// ID returns the sandbox for ctx, or "" for an unscoped request.
func ID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Middleware reads the sandbox header and scopes the request. A malformed id
// is rejected rather than treated as unscoped, so a bad client can't quietly
// reach the unscoped data.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get(Header)
		if raw == "" {
			next.ServeHTTP(w, r)
			return
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]map[string]string{"error": {
				"code":    "invalid_sandbox",
				"message": Header + " must be a UUID",
			}})
			return
		}
		next.ServeHTTP(w, r.WithContext(With(r.Context(), id.String())))
	})
}
