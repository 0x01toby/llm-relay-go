// Package cors implements the permissive CORS handling used by the gateway.
// Ported from src/cors.ts.
package cors

import (
	"net/http"
	"strings"
)

// defaultAllowHeaders mirrors DEFAULT_CORS_ALLOW_HEADERS. The list covers the
// headers Anthropic/OpenAI SDKs send.
var defaultAllowHeaders = strings.Join([]string{
	"Content-Type",
	"Authorization",
	"X-API-Key",
	"Anthropic-Version",
	"Anthropic-Beta",
	"Anthropic-Dangerous-Direct-Access",
	"OpenAI-Beta",
	"OpenAI-Organization",
	"OpenAI-Project",
	"X-Request-ID",
}, ", ")

// baseHeaders are applied to every response (including errors).
const (
	baseAllowOrigin  = "*"
	baseAllowMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	baseMaxAge       = "86400"
)

// allowedRequestHeaders echoes the client's Access-Control-Request-Headers when
// present, otherwise falls back to the default list. Mirrors
// getAllowedRequestHeaders.
func allowedRequestHeaders(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("Access-Control-Request-Headers")); v != "" {
		return v
	}
	return defaultAllowHeaders
}

// Apply writes the CORS headers onto an outgoing response. It is additive: it
// does not remove existing headers. Use this on every response (including
// errors) so browser clients can read them.
func Apply(h http.Header, r *http.Request) {
	h.Set("Access-Control-Allow-Origin", baseAllowOrigin)
	h.Set("Access-Control-Allow-Methods", baseAllowMethods)
	h.Set("Access-Control-Max-Age", baseMaxAge)
	h.Set("Access-Control-Allow-Headers", allowedRequestHeaders(r))
}

// PreflightResponse builds the response to an OPTIONS preflight request.
func PreflightResponse(r *http.Request) *http.Response {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       http.NoBody,
	}
	Apply(resp.Header, r)
	return resp
}

// Middleware wraps a handler so every response carries CORS headers and
// OPTIONS requests are answered directly. This is the standard way to mount
// CORS on a router.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			Apply(w.Header(), r)
			w.WriteHeader(http.StatusOK)
			return
		}
		// Defer header application so even handlers that WriteHeader early get
		// the CORS headers. (http.Header is a map; setting values before the
		// first Write works even if the handler changes the status.)
		Apply(w.Header(), r)
		next.ServeHTTP(w, r)
	})
}
