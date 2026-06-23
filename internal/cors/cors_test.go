package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestApply_SetsBaseHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	Apply(w.Header(), r)
	h := w.Header()
	if h.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("origin: %q", h.Get("Access-Control-Allow-Origin"))
	}
	if h.Get("Access-Control-Allow-Methods") != "GET, POST, PUT, PATCH, DELETE, OPTIONS" {
		t.Errorf("methods: %q", h.Get("Access-Control-Allow-Methods"))
	}
	if h.Get("Access-Control-Max-Age") != "86400" {
		t.Errorf("max-age: %q", h.Get("Access-Control-Max-Age"))
	}
}

func TestApply_EchoesRequestHeadersWhenPresent(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodOptions, "/x", nil)
	r.Header.Set("Access-Control-Request-Headers", "X-Custom, X-Other")
	Apply(w.Header(), r)
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "X-Custom, X-Other" {
		t.Errorf("expected echoed request headers, got %q", got)
	}
}

func TestApply_DefaultHeadersWhenAbsent(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	Apply(w.Header(), r)
	got := w.Header().Get("Access-Control-Allow-Headers")
	if got == "" || !contains(got, "Authorization") || !contains(got, "X-API-Key") {
		t.Errorf("expected default header list, got %q", got)
	}
}

func TestPreflightResponse(t *testing.T) {
	r := httptest.NewRequest(http.MethodOptions, "/x", nil)
	resp := PreflightResponse(r)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("missing origin header")
	}
}

func TestMiddleware_AnswersOptions(t *testing.T) {
	called := false
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	r := httptest.NewRequest(http.MethodOptions, "/x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if called {
		t.Error("handler should not be called for OPTIONS")
	}
	if w.Code != http.StatusOK {
		t.Errorf("OPTIONS status: %d", w.Code)
	}
}

func TestMiddleware_AppliesHeadersToGetResponse(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("status should pass through: %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS headers missing on GET response")
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
