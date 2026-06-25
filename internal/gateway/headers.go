package gateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/taozhang/llmrelay/internal/configstore"
)

// authTimeout caps how long an auth DB lookup may take. Prevents request
// pile-up if the database stalls.
const authTimeout = 5 * time.Second

// requestContext returns a context with a short timeout for auth DB lookups.
// The caller MUST defer the returned cancel func to avoid leaking timer
// resources (previously the cancel was discarded, causing timer goroutine
// buildup under high concurrency).
func requestContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), authTimeout)
}

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
