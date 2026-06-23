package gateway

import (
	"context"
	"net/http"
	"strings"

	"github.com/taozhang/llmrelay/internal/configstore"
)

// requestContext returns a background context for auth calls that don't need
// request-scoped cancellation (the auth DB lookup is fast and non-critical).
func requestContext() context.Context { return context.Background() }

// hopByHopHeaders are stripped from forwarded requests per RFC 7230 §6.1,
// plus headers we regenerate. Mirrors the original's deletion list.
var hopByHopHeaders = []string{
	"host", "content-length", "accept-encoding", "connection",
	"keep-alive", "proxy-authenticate", "proxy-authorization",
	"te", "trailer", "transfer-encoding", "upgrade",
}

// BuildForwardHeaders copies the source request headers, strips hop-by-hop
// headers, and injects the provider's configured auth credential. Mirrors
// buildForwardHeaders.
func BuildForwardHeaders(source http.Header, auth *configstore.AuthConfig) http.Header {
	out := source.Clone()
	for _, h := range hopByHopHeaders {
		out.Del(h)
	}
	if auth != nil {
		// Remove any client-provided credentials, then set the configured one.
		out.Del("authorization")
		out.Del("x-api-key")
		out.Set(strings.ToLower(auth.Header), auth.Value)
	}
	return out
}
