//go:build !chaos

package chaos

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The default build must not serve any chaos route or honor the crash
// header, whatever the request looks like.
func TestDefaultBuildHasNoChaos(t *testing.T) {
	if Enabled {
		t.Fatal("Enabled is true without the chaos tag")
	}
	mux := http.NewServeMux()
	Register(mux, Deps{})
	for _, path := range []string{"/chaos", "/chaos/flush-redis", "/chaos/corrupt-balance", "/chaos/repair"} {
		for _, method := range []string{"GET", "POST"} {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, path, rec.Code)
			}
		}
	}

	called := false
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))
	req := httptest.NewRequest("POST", "/transfers", nil)
	req.Header.Set("X-Chaos-Crash", "after-commit")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusCreated {
		t.Fatalf("crash header changed the response in the default build: called=%v status=%d", called, rec.Code)
	}
}
