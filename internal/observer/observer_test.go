package observer

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/taozhang/llmrelay/internal/configstore"
)

func TestCapturer_CapturesBodyAndUsage_JSON(t *testing.T) {
	// A non-streaming JSON response (OpenAI-style) with usage.
	body := `{"id":"x","model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	src := strings.NewReader(body)
	c := NewCapturer(src, configstore.OpenAI, "application/json", time.Now().UnixMilli())

	// Drain the client body (simulates the gateway reading it).
	clientBody := c.ClientBody()
	read, _ := io.ReadAll(clientBody)
	c.Close()

	if string(read) != body {
		t.Errorf("client body mismatch: got %q", string(read))
	}

	res, ok := awaitResult(c, 2*time.Second)
	if !ok {
		t.Fatal("observer did not produce a result")
	}
	if res.Body != body {
		t.Errorf("captured body mismatch")
	}
	if res.BodyBytes != len(body) {
		t.Errorf("body bytes: %d, want %d", res.BodyBytes, len(body))
	}
	if res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 5 || res.Usage.TotalTokens != 15 {
		t.Errorf("usage: %+v", res.Usage)
	}
	if res.Usage.Model != "gpt-4o" {
		t.Errorf("model: %s", res.Usage.Model)
	}
	if res.FirstChunkAt == nil || res.CompletedAt == nil {
		t.Error("timing not captured")
	}
	if res.HasStreaming {
		t.Error("JSON response should not be marked streaming")
	}
}

func TestCapturer_CapturesSSE_AnthropicUsage(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"message":{"model":"claude-3","usage":{"input_tokens":50,"output_tokens":1}},"type":"message_start"}`,
		``,
		`event: content_block_delta`,
		`data: {"delta":{"type":"text_delta","text":"Hi there"},"index":0,"type":"content_block_delta"}`,
		``,
		`event: message_delta`,
		`data: {"delta":{"stop_reason":"end_turn"},"type":"message_delta","usage":{"output_tokens":20}}`,
		``,
	}, "\n")
	src := strings.NewReader(sse)
	c := NewCapturer(src, configstore.Anthropic, "text/event-stream", time.Now().UnixMilli())

	clientBody := c.ClientBody()
	_, _ = io.ReadAll(clientBody)
	c.Close()

	res, ok := awaitResult(c, 2*time.Second)
	if !ok {
		t.Fatal("observer did not produce a result")
	}
	if !res.HasStreaming {
		t.Error("SSE should be marked streaming")
	}
	if res.Usage.InputTokens != 50 {
		t.Errorf("input tokens: %d", res.Usage.InputTokens)
	}
	if res.Usage.OutputTokens != 20 {
		t.Errorf("output tokens: %d", res.Usage.OutputTokens)
	}
	if res.Usage.Model != "claude-3" {
		t.Errorf("model: %s", res.Usage.Model)
	}
	if res.Usage.StopReason != "end_turn" {
		t.Errorf("stop reason: %s", res.Usage.StopReason)
	}
	// First token should be detected (the text_delta has content).
	if res.FirstTokenAt == nil {
		t.Error("first token not detected for SSE")
	}
}

func TestCapturer_TruncatesLargeBody(t *testing.T) {
	// A body larger than MaxCaptureBytes should be truncated.
	large := strings.Repeat("x", MaxCaptureBytes+1000)
	src := strings.NewReader(large)
	c := NewCapturer(src, configstore.OpenAI, "application/json", time.Now().UnixMilli())

	clientBody := c.ClientBody()
	_, _ = io.ReadAll(clientBody)
	c.Close()

	res, ok := awaitResult(c, 2*time.Second)
	if !ok {
		t.Fatal("observer did not produce a result")
	}
	if !res.Truncated {
		t.Error("large body should be truncated")
	}
	if len(res.Body) != MaxCaptureBytes {
		t.Errorf("captured body length: %d, want %d", len(res.Body), MaxCaptureBytes)
	}
	if res.BodyBytes != len(large) {
		t.Errorf("body bytes should reflect full size: %d, want %d", res.BodyBytes, len(large))
	}
}

func TestCapturer_DoesNotBlockClient(t *testing.T) {
	// The client should receive all data even if no one reads ObserveDone.
	body := "quick response"
	src := strings.NewReader(body)
	c := NewCapturer(src, configstore.OpenAI, "application/json", time.Now().UnixMilli())

	clientBody := c.ClientBody()
	read, _ := io.ReadAll(clientBody)
	c.Close()

	if string(read) != body {
		t.Errorf("client did not receive full body: %q", string(read))
	}
}
