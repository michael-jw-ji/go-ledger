package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBucketAllowsBurstThenRefills(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(2, 3, 0) // 2/s, bursts of 3
	l.now = func() time.Time { return now }

	for i := range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("4th request: ok=%v wait=%v, want refused with 500ms wait", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("another client shares the first client's bucket")
	}

	now = now.Add(500 * time.Millisecond)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("no token after waiting the advertised time")
	}
}

func TestSweepDropsIdleBuckets(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(10, 10, 0)
	l.now = func() time.Time { return now }
	l.Allow("a")
	now = now.Add(2 * time.Minute)
	l.Allow("b")
	if _, ok := l.buckets["a"]; ok {
		t.Fatal("idle bucket survived the sweep")
	}
}

func TestMiddlewareLimitsWritesOnly(t *testing.T) {
	l := New(1, 1, 0)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	do := func(method string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/transfers", nil))
		return rec
	}
	if do("POST").Code != http.StatusNoContent {
		t.Fatal("first write refused")
	}
	if rec := do("POST"); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second write = %d, Retry-After %q; want 429 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	for range 5 {
		if do("GET").Code != http.StatusNoContent {
			t.Fatal("a read was limited")
		}
	}
}

func TestClientIPUsesProxyHopsFromTheRight(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	r.Header.Add("X-Forwarded-For", "6.6.6.6, 1.2.3.4")
	r.Header.Add("X-Forwarded-For", "10.0.0.1")

	cases := []struct {
		hops int
		want string
	}{
		{0, "10.0.0.9"}, // no proxy: the TCP peer, headers ignored
		{1, "10.0.0.1"},
		{2, "1.2.3.4"},  // the forged 6.6.6.6 is never chosen with 2 hops
		{9, "10.0.0.9"}, // fewer entries than hops: fall back to the peer
	}
	for _, c := range cases {
		if got := New(1, 1, c.hops).ClientIP(r); got != c.want {
			t.Errorf("hops=%d: got %s, want %s", c.hops, got, c.want)
		}
	}
}
