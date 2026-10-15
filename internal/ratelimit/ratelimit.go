// Package ratelimit throttles writes per client IP, so one visitor of the
// public demo can't flood it. It is a token bucket per IP, held in memory:
// enough for one instance, and it resets on restart.
package ratelimit

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Limiter struct {
	rate  float64 // tokens per second
	burst float64
	// proxyHops is how many proxies sit in front of the server. With 0, the
	// client is the TCP peer. With n, it is the nth X-Forwarded-For entry
	// from the right: entries further left are written by the client and
	// can be forged.
	proxyHops int
	now       func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// New allows each client perSecond writes on average, in bursts of up to
// burst.
func New(perSecond float64, burst, proxyHops int) *Limiter {
	return &Limiter{
		rate:      perSecond,
		burst:     float64(burst),
		proxyHops: proxyHops,
		now:       time.Now,
		buckets:   map[string]*bucket{},
	}
}

// Allow takes one token from key's bucket. When none is left, it reports how
// long until one is.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

// sweep drops buckets that have refilled completely, at most once a minute,
// so the map doesn't grow with every IP ever seen.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.at) > full {
			delete(l.buckets, k)
		}
	}
}

// Middleware limits every request except GET, HEAD and OPTIONS. Reads stay
// unlimited, so the console's polling never counts against a visitor.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		ok, wait := l.Allow(l.ClientIP(r))
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]map[string]string{"error": {
				"code":    "rate_limited",
				"message": "too many writes from this address; slow down and retry",
			}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP is the address the limit applies to.
func (l *Limiter) ClientIP(r *http.Request) string {
	if l.proxyHops > 0 {
		var hops []string
		for _, h := range r.Header.Values("X-Forwarded-For") {
			for _, part := range strings.Split(h, ",") {
				if p := strings.TrimSpace(part); p != "" {
					hops = append(hops, p)
				}
			}
		}
		if i := len(hops) - l.proxyHops; i >= 0 && i < len(hops) {
			return hops[i]
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
