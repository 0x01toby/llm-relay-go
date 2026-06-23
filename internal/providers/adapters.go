package providers

import (
	"encoding/json"
	"net/http"
	"strings"
)

// This file ports the non-usage provider adapter methods: buildForwardHeaders,
// prepareRequest (with Anthropic system-prompt injection), and hasTextualSignal.
// transformResponse is identity for both providers in the original (the real
// Anthropic thinking-block filter exists but is not wired through), so we omit
// it here; it can be added when needed.

// AuthConfig is the provider's configured credential (mirrors RouteAuthConfig).
type AuthConfig struct {
	Header string
	Value  string
}

// PreparedRequest is the output of PrepareRequest.
type PreparedRequest struct {
	RequestModel string
	Body         []byte // nil when the body should pass through unchanged
}

// PrepareRequestOptions are the inputs to PrepareRequest.
type PrepareRequestOptions struct {
	UpstreamType UpstreamType
	Method       string
	RawBody      []byte
	RouteSystem  string
}

// hopByHopHeaders are stripped from forwarded requests.
var hopByHopHeaders = []string{
	"host", "content-length", "accept-encoding", "connection",
	"keep-alive", "proxy-authenticate", "proxy-authorization",
	"te", "trailer", "transfer-encoding", "upgrade",
}

// BuildForwardHeaders copies source headers, strips hop-by-hop headers, and
// injects the provider's auth credential. Provider-agnostic (identical for
// anthropic and openai in the original).
func BuildForwardHeaders(source http.Header, auth *AuthConfig) http.Header {
	out := source.Clone()
	for _, h := range hopByHopHeaders {
		out.Del(h)
	}
	if auth != nil {
		out.Del("authorization")
		out.Del("x-api-key")
		out.Set(strings.ToLower(auth.Header), auth.Value)
	}
	return out
}

// PrepareRequest transforms a request body for forwarding. For Anthropic it
// injects the route's system prompt into the `system` field (merging with any
// existing system). For OpenAI it extracts the model and returns the body
// unchanged. Mirrors prepareRequest in both adapters.
func PrepareRequest(opts PrepareRequestOptions) PreparedRequest {
	if opts.Method != http.MethodPost || len(opts.RawBody) == 0 {
		return PreparedRequest{RequestModel: "unknown"}
	}

	var body map[string]interface{}
	if err := json.Unmarshal(opts.RawBody, &body); err != nil {
		return PreparedRequest{RequestModel: "unknown"}
	}

	model, _ := body["model"].(string)
	if model == "" {
		model = "unknown"
	}

	// Anthropic: inject route system prompt.
	if opts.UpstreamType == Anthropic && opts.RouteSystem != "" {
		injectRouteSystem(body, opts.RouteSystem)
	}

	out, err := json.Marshal(body)
	if err != nil {
		return PreparedRequest{RequestModel: model}
	}
	return PreparedRequest{RequestModel: model, Body: out}
}

// injectRouteSystem merges routeSystem into the request's `system` field. If
// system is a string, routeSystem is prepended. If it's an array, a text block
// is unshifted. If absent, routeSystem becomes the string system. Mirrors
// injectRouteSystemIntoSystem.
func injectRouteSystem(body map[string]interface{}, routeSystem string) {
	sys, exists := body["system"]
	if !exists || sys == nil {
		body["system"] = routeSystem
		return
	}
	switch s := sys.(type) {
	case string:
		body["system"] = routeSystem + "\n\n" + s
	case []interface{}:
		// Prepend a text block.
		block := map[string]interface{}{"type": "text", "text": routeSystem}
		body["system"] = append([]interface{}{block}, s...)
	default:
		// Non-string/non-array system: overwrite defensively.
		body["system"] = routeSystem
	}
}

// HasTextualSignal reports whether an SSE event block carries visible content,
// used for first-token detection. Mirrors hasTextualSignal for both providers.
func HasTextualSignal(eventBlock string, t UpstreamType) bool {
	if eventBlock == "" {
		return false
	}
	// Extract data: lines and parse as JSON.
	for _, line := range strings.Split(eventBlock, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimSpace(line[len("data: "):])
		if data == "" || data == "[DONE]" {
			continue
		}
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			continue
		}
		if providerHasText(v, t) {
			return true
		}
	}
	return false
}

// providerHasText inspects a parsed SSE data payload for visible content.
func providerHasText(v map[string]interface{}, t UpstreamType) bool {
	if t == Anthropic {
		// content_block_start with text block, or content_block_delta with text_delta.
		switch v["type"] {
		case "content_block_start":
			if cb, ok := v["content_block"].(map[string]interface{}); ok {
				if cb["type"] == "text" {
					if txt, _ := cb["text"].(string); txt != "" {
						return true
					}
				}
			}
		case "content_block_delta":
			if d, ok := v["delta"].(map[string]interface{}); ok {
				if d["type"] == "text_delta" {
					if txt, _ := d["text"].(string); txt != "" {
						return true
					}
				}
			}
		}
		return false
	}
	// OpenAI: Chat Completions choices[].delta.content, or a top-level delta
	// (some providers), or Responses-API events.
	if choices, ok := v["choices"].([]interface{}); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			if d, ok := choice["delta"].(map[string]interface{}); ok {
				if content, ok := d["content"].(string); ok && content != "" {
					return true
				}
				if tcs, ok := d["tool_calls"].([]interface{}); ok && len(tcs) > 0 {
					return true
				}
			}
		}
	}
	// Top-level delta (some providers emit it directly).
	if d, ok := v["delta"].(map[string]interface{}); ok {
		if c, ok := d["content"].(string); ok && c != "" {
			return true
		}
	}
	// Responses API: response.output_text.delta.
	if v["type"] == "response.output_text.delta" {
		if d, _ := v["delta"].(string); d != "" {
			return true
		}
	}
	return false
}

// DetectRequestKind mirrors detectRequestKind: returns "generic" if the body
// parses as JSON, else "unknown".
func DetectRequestKind(rawBody []byte) string {
	if len(rawBody) == 0 {
		return "unknown"
	}
	var v interface{}
	if err := json.Unmarshal(rawBody, &v); err != nil {
		return "unknown"
	}
	return "generic"
}
