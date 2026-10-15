//go:build !chaos

package chaos

import "net/http"

// Enabled reports whether this binary was built with the chaos endpoints.
const Enabled = false

// Register does nothing without the chaos build tag.
func Register(*http.ServeMux, Deps) {}

// Wrap returns next unchanged without the chaos build tag.
func Wrap(next http.Handler) http.Handler { return next }
