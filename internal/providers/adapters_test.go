package providers

import (
	"net/http"
	"testing"
)

func TestBuildForwardHeaders_StripsHopByHop(t *testing.T) {
	src := http.Header{}
	src.Set("Host", "client.example")
	src.Set("Content-Length", "123")
	src.Set("Accept-Encoding", "gzip")
	src.Set("X-Custom", "keep-me")
	out := BuildForwardHeaders(src, nil)
	if out.Get("Host") != "" || out.Get("Content-Length") != "" || out.Get("Accept-Encoding") != "" {
		t.Error("hop-by-hop headers should be stripped")
	}
	if out.Get("X-Custom") != "keep-me" {
		t.Error("custom header should be preserved")
	}
}

func TestBuildForwardHeaders_InjectsAuth(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer client-token")
	src.Set("X-API-Key", "client-key")
	out := BuildForwardHeaders(src, &AuthConfig{Header: "authorization", Value: "Bearer sk-upstream"})
	if out.Get("Authorization") != "Bearer sk-upstream" {
		t.Errorf("auth: %q", out.Get("Authorization"))
	}
	if out.Get("X-API-Key") != "" {
		t.Error("client x-api-key should be removed")
	}
}

func TestPrepareRequest_AnthropicSystemInjection(t *testing.T) {
	// String system → prepend.
	body := []byte(`{"model":"claude-3","system":"existing","messages":[]}`)
	res := PrepareRequest(PrepareRequestOptions{
		UpstreamType: Anthropic, Method: http.MethodPost, RawBody: body, RouteSystem: "route-prompt",
	})
	if res.RequestModel != "claude-3" {
		t.Errorf("model: %s", res.RequestModel)
	}
	// In JSON the newline is escaped as \n, so check the escaped form.
	if !contains(string(res.Body), `route-prompt\n\nexisting`) {
		t.Errorf("system not merged: %s", string(res.Body))
	}

	// No system → set directly.
	body2 := []byte(`{"model":"claude-3","messages":[]}`)
	res2 := PrepareRequest(PrepareRequestOptions{
		UpstreamType: Anthropic, Method: http.MethodPost, RawBody: body2, RouteSystem: "route-prompt",
	})
	if !contains(string(res2.Body), `"system":"route-prompt"`) {
		t.Errorf("system not set: %s", string(res2.Body))
	}
}

func TestPrepareRequest_OpenAIDoesNotInject(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[]}`)
	res := PrepareRequest(PrepareRequestOptions{
		UpstreamType: OpenAI, Method: http.MethodPost, RawBody: body, RouteSystem: "route-prompt",
	})
	// OpenAI does not inject system prompt; body should not mention route-prompt.
	if contains(string(res.Body), "route-prompt") {
		t.Errorf("OpenAI should not inject system: %s", string(res.Body))
	}
}

func TestPrepareRequest_NonPostReturnsUnknown(t *testing.T) {
	res := PrepareRequest(PrepareRequestOptions{Method: http.MethodGet, RawBody: nil})
	if res.RequestModel != "unknown" || res.Body != nil {
		t.Errorf("non-POST should return unknown model + nil body")
	}
}

func TestHasTextualSignal_AnthropicTextDelta(t *testing.T) {
	block := "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"},\"type\":\"content_block_delta\"}"
	if !HasTextualSignal(block, Anthropic) {
		t.Error("text_delta with text should signal")
	}
	blockEmpty := "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"\"}}"
	if HasTextualSignal(blockEmpty, Anthropic) {
		t.Error("empty text_delta should not signal")
	}
}

func TestHasTextualSignal_OpenAIChatDelta(t *testing.T) {
	block := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}"
	if !HasTextualSignal(block, OpenAI) {
		t.Error("chat delta with content should signal")
	}
}

func TestHasTextualSignal_Empty(t *testing.T) {
	if HasTextualSignal("", OpenAI) {
		t.Error("empty block should not signal")
	}
}

func TestDetectRequestKind(t *testing.T) {
	if DetectRequestKind([]byte(`{"a":1}`)) != "generic" {
		t.Error("valid JSON should be generic")
	}
	if DetectRequestKind([]byte(`not json`)) != "unknown" {
		t.Error("invalid JSON should be unknown")
	}
	if DetectRequestKind(nil) != "unknown" {
		t.Error("nil body should be unknown")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
