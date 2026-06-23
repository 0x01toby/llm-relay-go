// Package consoleauth implements the console cookie authentication. It is a
// byte-for-byte port of the hashSecret/isAuthenticated logic in console-ui.ts
// so that cookies issued by the original service remain valid across the
// rewrite (or, if invalidated, users simply re-login).
//
// The hash is FNV-1a 32-bit (offset 0x811c9dc5, prime 0x01000193) using
// Math.imul semantics (32-bit unsigned multiply), rendered as 8-char lowercase
// hex. The auth token is "v1:" + that hash of the GATEWAY_API_KEY.
package consoleauth

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"
)

// CookieName is the console session cookie name. The original used the literal
// string "CONSOLE_COOKIE_NAME".
const CookieName = "CONSOLE_COOKIE_NAME"

// CookieMaxAge is the session lifetime (1 year), matching the original.
const CookieMaxAge = 365 * 24 * 60 * 60

// HashSecret computes the FNV-1a 32-bit hash of secret as 8-char lowercase hex.
// This is a byte-for-byte port of hashSecret: Math.imul becomes uint32
// multiplication, >>> 0 is implicit in uint32.
func HashSecret(secret string) string {
	hash := uint32(0x811c9dc5)
	for i := 0; i < len(secret); i++ {
		hash ^= uint32(secret[i])
		hash *= 0x01000193 // wraps mod 2^32 in uint32
	}
	// Render as 8-char lowercase hex (the original uses padStart(8, '0')).
	hex := [8]byte{}
	const d = "0123456789abcdef"
	for i := 7; i >= 0; i-- {
		hex[i] = d[hash&0xf]
		hash >>= 4
	}
	return string(hex[:])
}

// AuthToken returns "v1:" + HashSecret(password). Mirrors getAuthToken.
func AuthToken(password string) string {
	return "v1:" + HashSecret(password)
}

// IsPasswordConfigured reports whether a console password is set.
func IsPasswordConfigured(password string) bool { return len(password) > 0 }

// IsAuthenticated reports whether the request carries a valid console session
// cookie for the configured password. Uses constant-time comparison.
// Mirrors isAuthenticated.
func IsAuthenticated(r *http.Request, password string) bool {
	if !IsPasswordConfigured(password) {
		return false
	}
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	want := AuthToken(password)
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(want)) == 1
}

// SetSessionCookie writes the console session cookie onto the response.
func SetSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "", // set by the login handler after computing the token
		Path:     "/",
		MaxAge:   CookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// SetSessionCookieWithValue writes the console cookie with a precomputed token.
func SetSessionCookieWithValue(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   CookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie expires the console cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0),
	})
}

// WantsJSON reports whether the client expects a JSON response (the React
// dashboard always sends Accept: application/json). Mirrors wantsJson.
func WantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	contentType := r.Header.Get("Content-Type")
	return strings.Contains(accept, "application/json") || strings.Contains(contentType, "application/json")
}
