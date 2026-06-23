package providers

import "testing"

// Direct port of test/parse-sse.test.ts. The real-SSE fixture exercises the
// hardest path: a full Anthropic streaming response with thinking blocks,
// ephemeral cache tokens, and a terminal message_delta carrying output tokens.

const realAnthropicSSE = `event: message_start
data: {"message":{"content":[],"id":"msg_01NdUkbVUDlRH9cFJI0jHF0b","model":"claude-opus-4-6","role":"assistant","stop_reason":null,"stop_sequence":null,"type":"message","usage":{"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":384},"cache_creation_input_tokens":384,"cache_read_input_tokens":0,"inference_geo":"not_available","input_tokens":65,"output_tokens":1,"service_tier":"standard"}},"type":"message_start"}

event: ping
data: {"type":"ping"}

event: content_block_start
data: {"content_block":{"signature":"","thinking":"","type":"thinking"},"index":0,"type":"content_block_start"}

event: content_block_delta
data: {"delta":{"thinking":"The user is saying hello","type":"thinking_delta"},"index":0,"type":"content_block_delta"}

event: content_block_stop
data: {"index":0,"type":"content_block_stop"}

event: content_block_start
data: {"content_block":{"text":"","type":"text"},"index":1,"type":"content_block_start"}

event: content_block_delta
data: {"delta":{"text":"你好！","type":"text_delta"},"index":1,"type":"content_block_delta"}

event: content_block_stop
data: {"index":1,"type":"content_block_stop"}

event: message_delta
data: {"delta":{"stop_reason":"end_turn","stop_sequence":null},"type":"message_delta","usage":{"cache_creation_input_tokens":384,"cache_read_input_tokens":0,"input_tokens":65,"output_tokens":76}}

event: message_stop
data: {"type":"message_stop"}`

func TestParseAnthropicUsage_RealSSE(t *testing.T) {
	u := ParseUsage(realAnthropicSSE, Anthropic)
	checks := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"model", u.Model, "claude-opus-4-6"},
		{"stop_reason", u.StopReason, "end_turn"},
		{"input_tokens", u.InputTokens, int64(65)},
		{"output_tokens", u.OutputTokens, int64(76)},
		{"total_tokens", u.TotalTokens, int64(65 + 76 + 384 + 0)},
		{"cache_creation", u.CacheCreationInputTokens, int64(384)},
		{"cache_read", u.CacheReadInputTokens, int64(0)},
		{"ephemeral_5m", u.Ephemeral5mInputTokens, int64(384)},
		{"ephemeral_1h", u.Ephemeral1hInputTokens, int64(0)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestParseAnthropicUsage_NonStreamingJSON(t *testing.T) {
	body := `{"model":"claude-sonnet-4-5-20250929","stop_reason":"max_tokens","usage":{"input_tokens":100,"output_tokens":200,"cache_creation_input_tokens":50,"cache_read_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":50,"ephemeral_1h_input_tokens":0}}}`
	u := ParseUsage(body, Anthropic)
	if u.Model != "claude-sonnet-4-5-20250929" {
		t.Errorf("model: %s", u.Model)
	}
	if u.StopReason != "max_tokens" {
		t.Errorf("stop_reason: %s", u.StopReason)
	}
	if u.InputTokens != 100 || u.OutputTokens != 200 {
		t.Errorf("tokens: in=%d out=%d", u.InputTokens, u.OutputTokens)
	}
	if u.TotalTokens != 380 {
		t.Errorf("total: %d", u.TotalTokens)
	}
	if u.CacheCreationInputTokens != 50 || u.CacheReadInputTokens != 30 {
		t.Errorf("cache: create=%d read=%d", u.CacheCreationInputTokens, u.CacheReadInputTokens)
	}
	if u.Ephemeral5mInputTokens != 50 || u.Ephemeral1hInputTokens != 0 {
		t.Errorf("ephemeral: 5m=%d 1h=%d", u.Ephemeral5mInputTokens, u.Ephemeral1hInputTokens)
	}
}

func TestParseOpenAIUsage_NonStreamingJSON(t *testing.T) {
	body := `{"model":"gpt-5-mini","choices":[{"finish_reason":"stop"}],"usage":{"completion_tokens":170,"total_tokens":117195,"prompt_tokens":117025,"prompt_tokens_details":{"cached_tokens":116224},"completion_tokens_details":{"reasoning_tokens":22}}}`
	u := ParseUsage(body, OpenAI)
	if u.Model != "gpt-5-mini" {
		t.Errorf("model: %s", u.Model)
	}
	if u.StopReason != "stop" {
		t.Errorf("stop_reason: %s", u.StopReason)
	}
	if u.InputTokens != 117025 || u.OutputTokens != 170 || u.TotalTokens != 117195 {
		t.Errorf("tokens: in=%d out=%d total=%d", u.InputTokens, u.OutputTokens, u.TotalTokens)
	}
	if u.CachedInputTokens != 116224 {
		t.Errorf("cached: %d", u.CachedInputTokens)
	}
	if u.ReasoningOutputTokens != 22 {
		t.Errorf("reasoning: %d", u.ReasoningOutputTokens)
	}
	if u.CacheCreationInputTokens != 0 || u.CacheReadInputTokens != 0 {
		t.Errorf("anthropic fields should be 0: create=%d read=%d", u.CacheCreationInputTokens, u.CacheReadInputTokens)
	}
}

func TestParseUsage_EmptyBody(t *testing.T) {
	u := ParseUsage("", Anthropic)
	if u.Model != "" || u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("expected zeros, got %+v", u)
	}
}

func TestParseUsage_ErrorBodyGraceful(t *testing.T) {
	body := `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	u := ParseUsage(body, Anthropic)
	if u.Model != "" || u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("expected zeros for error body, got %+v", u)
	}
}

func TestParseAnthropicUsage_MalformedSSE(t *testing.T) {
	malformed := "event: message_start\ndata: {invalid json}\n\nevent: message_delta\ndata: also not json"
	u := ParseUsage(malformed, Anthropic)
	if u.Model != "" || u.InputTokens != 0 {
		t.Errorf("expected zeros for malformed SSE, got %+v", u)
	}
}

func TestParseAnthropicUsage_CacheReadOnly(t *testing.T) {
	sse := `event: message_start
data: {"message":{"model":"claude-haiku-4-5-20251001","usage":{"input_tokens":200,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":1500,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}}},"type":"message_start"}

event: message_delta
data: {"delta":{"stop_reason":"end_turn"},"type":"message_delta","usage":{"output_tokens":42,"cache_creation_input_tokens":0,"cache_read_input_tokens":1500,"input_tokens":200}}

event: message_stop
data: {"type":"message_stop"}`
	u := ParseUsage(sse, Anthropic)
	if u.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model: %s", u.Model)
	}
	if u.StopReason != "end_turn" {
		t.Errorf("stop_reason: %s", u.StopReason)
	}
	if u.InputTokens != 200 || u.OutputTokens != 42 {
		t.Errorf("tokens: in=%d out=%d", u.InputTokens, u.OutputTokens)
	}
	if u.CacheCreationInputTokens != 0 || u.CacheReadInputTokens != 1500 {
		t.Errorf("cache: create=%d read=%d", u.CacheCreationInputTokens, u.CacheReadInputTokens)
	}
}
