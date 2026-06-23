package consoleapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestAPI builds an API with a hardcoded password and no DB (admin-only
// auth path). DB-backed routes are covered by integration tests.
func newTestAPI(t *testing.T, password string) *API {
	t.Helper()
	return &API{
		password: password,
		// pool/repos nil — only auth/session routes are exercised here.
	}
}

func TestSession_Unauthenticated(t *testing.T) {
	a := newTestAPI(t, "secret")
	req := httptest.NewRequest(http.MethodGet, "/__console/api/session", nil)
	w := httptest.NewRecorder()
	a.handleSession(w, req)
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"authenticated":false`) {
		t.Errorf("body: %s", w.Body.String())
	}
}

func TestLogin_CorrectPassword_SetsCookie(t *testing.T) {
	a := newTestAPI(t, "secret")
	req := httptest.NewRequest(http.MethodPost, "/__console/login", strings.NewReader(`{"password":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.handleLogin(w, req)
	if w.Code != 200 {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	// Cookie should be set.
	setCookie := w.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "CONSOLE_COOKIE_NAME=") {
		t.Errorf("no cookie set: %s", setCookie)
	}
	if !strings.Contains(setCookie, "HttpOnly") {
		t.Error("cookie should be HttpOnly")
	}
}

func TestLogin_WrongPassword_401(t *testing.T) {
	a := newTestAPI(t, "secret")
	req := httptest.NewRequest(http.MethodPost, "/__console/login", strings.NewReader(`{"password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.handleLogin(w, req)
	if w.Code != 401 {
		t.Errorf("status: %d", w.Code)
	}
}

func TestLogin_NoPasswordConfigured_503(t *testing.T) {
	a := newTestAPI(t, "")
	req := httptest.NewRequest(http.MethodPost, "/__console/login", strings.NewReader(`{"password":"anything"}`))
	w := httptest.NewRecorder()
	a.handleLogin(w, req)
	if w.Code != 503 {
		t.Errorf("status: %d", w.Code)
	}
}

func TestRequireAuth_Unauthenticated_401(t *testing.T) {
	a := newTestAPI(t, "secret")
	h := a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called when unauthenticated")
	})
	req := httptest.NewRequest(http.MethodGet, "/__console/api/providers", nil)
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != 401 {
		t.Errorf("status: %d", w.Code)
	}
}

func TestRequireAuth_Authenticated_PassesThrough(t *testing.T) {
	a := newTestAPI(t, "secret")
	called := false
	h := a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	req := httptest.NewRequest(http.MethodGet, "/__console/api/providers", nil)
	req.AddCookie(&http.Cookie{Name: "CONSOLE_COOKIE_NAME", Value: "v1:" + fnvHash("secret")})
	w := httptest.NewRecorder()
	h(w, req)
	if !called {
		t.Error("handler should be called when authenticated")
	}
}

func TestLogout_ClearsCookie(t *testing.T) {
	a := newTestAPI(t, "secret")
	req := httptest.NewRequest(http.MethodPost, "/__console/logout", nil)
	w := httptest.NewRecorder()
	a.handleLogout(w, req)
	setCookie := w.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "Max-Age=0") && !strings.Contains(setCookie, "expires=") {
		t.Errorf("cookie not cleared: %s", setCookie)
	}
}

// fnvHash replicates the FNV-1a hash so the test can build a valid cookie.
func fnvHash(s string) string {
	hash := uint32(0x811c9dc5)
	for i := 0; i < len(s); i++ {
		hash ^= uint32(s[i])
		hash *= 0x01000193
	}
	const hex = "0123456789abcdef"
	out := [8]byte{}
	for i := 7; i >= 0; i-- {
		out[i] = hex[hash&0xf]
		hash >>= 4
	}
	return string(out[:])
}
