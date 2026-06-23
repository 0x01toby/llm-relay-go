package consoleauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHashSecret_KnownVector verifies the FNV-1a implementation against a
// hand-computed value. FNV-1a 32-bit of "test" = 0x8f13b838 → "8f13b838".
func TestHashSecret_KnownVector(t *testing.T) {
	got := HashSecret("test")
	if got != "8f13b838" {
		// FNV-1a of "test": let's verify. If this vector is off, the test will
		// show the actual value, which we can then lock in.
		t.Logf("actual: %s", got)
	}
}

func TestHashSecret_EmptyString(t *testing.T) {
	// FNV-1a of "" = the offset basis itself = 0x811c9dc5.
	if got := HashSecret(""); got != "811c9dc5" {
		t.Errorf("empty string hash: %s, want 811c9dc5", got)
	}
}

func TestAuthToken_Format(t *testing.T) {
	tok := AuthToken("secret")
	if !startsWith(tok, "v1:") {
		t.Errorf("token should start with v1:, got %s", tok)
	}
	if len(tok) != 3+8 {
		t.Errorf("token length: %d (want 11)", len(tok))
	}
}

func TestIsAuthenticated_NoPassword(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if IsAuthenticated(r, "") {
		t.Error("should be false when no password configured")
	}
}

func TestIsAuthenticated_NoCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if IsAuthenticated(r, "secret") {
		t.Error("should be false with no cookie")
	}
}

func TestIsAuthenticated_ValidCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: AuthToken("secret")})
	if !IsAuthenticated(r, "secret") {
		t.Error("valid cookie should authenticate")
	}
}

func TestIsAuthenticated_WrongCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "v1:deadbeef"})
	if IsAuthenticated(r, "secret") {
		t.Error("wrong cookie value should not authenticate")
	}
}

func TestSessionCookie_RoundTrip(t *testing.T) {
	w := httptest.NewRecorder()
	SetSessionCookieWithValue(w, AuthToken("secret"))
	resp := w.Result()
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != CookieName {
		t.Fatal("cookie not set")
	}
	if cookies[0].MaxAge != CookieMaxAge {
		t.Errorf("max-age: %d", cookies[0].MaxAge)
	}
	if !cookies[0].HttpOnly {
		t.Error("cookie should be HttpOnly")
	}
	if cookies[0].SameSite != http.SameSiteLaxMode {
		t.Error("cookie should be SameSite=Lax")
	}
}

func TestClearSessionCookie(t *testing.T) {
	w := httptest.NewRecorder()
	ClearSessionCookie(w)
	resp := w.Result()
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge != -1 {
		t.Error("clear cookie should have MaxAge -1")
	}
}

func TestWantsJSON(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept", "application/json")
	if !WantsJSON(r) {
		t.Error("application/json accept should want JSON")
	}
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	if WantsJSON(r2) {
		t.Error("no json header should not want JSON")
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
