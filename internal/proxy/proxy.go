// Package proxy (spike edition) wires the responsesconv transformer into an
// httputil.ReverseProxy. This is a minimal, database-free proof that the
// streaming conversion works end-to-end against a real OpenAI-compatible
// upstream. The full gateway engine (auth, routing, failover, logging) is
// built in a later phase.
package proxy

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/taozhang/llmrelay/internal/responsesconv"
)

// SpikeConfig configures the minimal proxy.
type SpikeConfig struct {
	// UpstreamBaseURL is the target OpenAI-compatible base (e.g.
	// "https://api.openai.com/v1"). The /v1/responses request is rewritten to
	// {base}/chat/completions.
	UpstreamBaseURL string
	// UpstreamAuthHeader/Value replace the client's credential when forwarding
	// (e.g. "Authorization" / "Bearer sk-...").
	UpstreamAuthHeader string
	UpstreamAuthValue  string
}

// New builds an http.Handler that:
//  1. Accepts POST /v1/responses (Responses-API format)
//  2. Converts the request body to Chat Completions
//  3. Forwards to the upstream {base}/chat/completions
//  4. Converts the response (streaming or JSON) back to Responses-API format
//
// Everything else is proxied untouched so you can still hit /v1/models etc.
func New(cfg SpikeConfig) (http.Handler, error) {
	if _, err := url.Parse(cfg.UpstreamBaseURL); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		handleResponses(w, r, cfg)
	})
	// Pass-through for anything else (e.g. /v1/models) so the spike is usable.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		passThrough(w, r, cfg)
	})
	return mux, nil
}

func handleResponses(w http.ResponseWriter, r *http.Request, cfg SpikeConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	result := responsesconv.ConvertResponsesRequestToChatCompletions(
		string(body),
		&responsesconv.RequestOptions{TargetURL: cfg.UpstreamBaseURL + "/chat/completions"},
	)
	if !result.OK {
		responsesconv.WriteErrorResponse(w, result.Error)
		return
	}

	// Build the upstream request to {base}/chat/completions.
	target := strings.TrimRight(cfg.UpstreamBaseURL, "/") + "/chat/completions"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, strings.NewReader(result.Body))
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	// Forward the client auth header, then override with the configured upstream
	// credential if provided.
	if h := cfg.UpstreamAuthHeader; h != "" && cfg.UpstreamAuthValue != "" {
		upReq.Header.Set(h, cfg.UpstreamAuthValue)
	}

	resp, err := http.DefaultTransport.RoundTrip(upReq)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Convert the upstream Chat Completions response to Responses-API format.
	converted := responsesconv.TransformResponse(resp)

	// Copy status and headers.
	for k, vs := range converted.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(converted.StatusCode)

	// Stream the converted body to the client, flushing after each write so SSE
	// deltas reach the client incrementally.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, rerr := converted.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
}

// passThrough forwards the request verbatim to the upstream, only swapping the
// auth header. Used for /v1/models and other non-/v1/responses paths.
func passThrough(w http.ResponseWriter, r *http.Request, cfg SpikeConfig) {
	target := strings.TrimRight(cfg.UpstreamBaseURL, "/") + r.URL.Path
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	upReq.Header = r.Header.Clone()
	if h := cfg.UpstreamAuthHeader; h != "" && cfg.UpstreamAuthValue != "" {
		upReq.Header.Set(h, cfg.UpstreamAuthValue)
	}
	resp, err := http.DefaultTransport.RoundTrip(upReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
