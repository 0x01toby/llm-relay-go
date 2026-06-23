// Package providers contains the Anthropic/OpenAI adapter logic ported from the
// original src/providers/*.ts. For the spike we port the usage parsers
// (parseUsage) since they are the second-most-exercised streaming code path
// and validate that we correctly parse real SSE responses from both providers.
package providers

import "strings"

// UpstreamType identifies the provider protocol family.
type UpstreamType string

const (
	Anthropic UpstreamType = "anthropic"
	OpenAI    UpstreamType = "openai"
)

// UsageData is the normalized token-usage shape extracted from a provider
// response. Mirrors providers/types.ts UsageData. All token fields are int64;
// the JSON-in-text source uses JS numbers which are all doubles, but token
// counts are integers so we truncate to int64 on assignment.
type UsageData struct {
	Model                      string `json:"model"`
	StopReason                 string `json:"stop_reason"`
	InputTokens                int64  `json:"input_tokens"`
	OutputTokens               int64  `json:"output_tokens"`
	TotalTokens                int64  `json:"total_tokens"`
	CacheCreationInputTokens   int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens       int64  `json:"cache_read_input_tokens"`
	CachedInputTokens          int64  `json:"cached_input_tokens"`
	ReasoningOutputTokens      int64  `json:"reasoning_output_tokens"`
	Ephemeral5mInputTokens     int64  `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens     int64  `json:"ephemeral_1h_input_tokens"`
	Estimated                  bool   `json:"estimated,omitempty"`
}

func emptyUsage() UsageData { return UsageData{} }

// toInt safely converts a float64 (JSON number) to int64. Non-numbers yield 0.
func toInt(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

func isNum(v interface{}) bool {
	switch v.(type) {
	case float64, int, int64:
		return true
	}
	return false
}

func isObj(v interface{}) bool {
	_, ok := v.(map[string]interface{})
	return ok
}

func obj(v interface{}) map[string]interface{} {
	o, _ := v.(map[string]interface{})
	return o
}

func str(v interface{}) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// finalizeUsageTotals for Anthropic includes the cache token fields in the
// total (unlike OpenAI). Mirrors providers/anthropic.ts finalizeUsageTotals.
func finalizeUsageTotalsAnthropic(u *UsageData) {
	u.TotalTokens = u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// ParseUsage dispatches to the provider-specific parser.
func ParseUsage(body string, t UpstreamType) UsageData {
	if t == OpenAI {
		return parseOpenAIUsage(body)
	}
	return parseAnthropicUsage(body)
}

// --- Anthropic ---

func parseAnthropicUsage(body string) UsageData {
	result := emptyUsage()
	if body == "" {
		return result
	}

	if !strings.HasPrefix(body, "event:") {
		// Non-streaming JSON.
		v, err := decode(body)
		if err != nil {
			return result
		}
		j := obj(v)
		if j == nil {
			return result
		}
		if m, ok := str(j["model"]); ok {
			result.Model = m
		}
		if sr, ok := str(j["stop_reason"]); ok {
			result.StopReason = sr
		}
		if u := obj(j["usage"]); u != nil {
			result.InputTokens = toInt(u["input_tokens"])
			result.OutputTokens = toInt(u["output_tokens"])
			result.CacheCreationInputTokens = toInt(u["cache_creation_input_tokens"])
			result.CacheReadInputTokens = toInt(u["cache_read_input_tokens"])
			if cc := obj(u["cache_creation"]); cc != nil {
				result.Ephemeral5mInputTokens = toInt(cc["ephemeral_5m_input_tokens"])
				result.Ephemeral1hInputTokens = toInt(cc["ephemeral_1h_input_tokens"])
			}
			finalizeUsageTotalsAnthropic(&result)
		}
		return result
	}

	// Streaming SSE.
	for _, event := range strings.Split(body, "\n\n") {
		var eventType, data string
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimSpace(line[len("event: "):])
			}
			if strings.HasPrefix(line, "data: ") {
				data = line[len("data: "):]
			}
		}
		if data == "" {
			continue
		}
		v, err := decode(data)
		if err != nil {
			continue
		}
		j := obj(v)
		if j == nil {
			continue
		}
		if eventType == "message_start" {
			if msg := obj(j["message"]); msg != nil {
				if m, ok := str(msg["model"]); ok {
					result.Model = m
				}
				if u := obj(msg["usage"]); u != nil {
					result.InputTokens = toInt(u["input_tokens"])
					result.CacheCreationInputTokens = toInt(u["cache_creation_input_tokens"])
					result.CacheReadInputTokens = toInt(u["cache_read_input_tokens"])
					if cc := obj(u["cache_creation"]); cc != nil {
						result.Ephemeral5mInputTokens = toInt(cc["ephemeral_5m_input_tokens"])
						result.Ephemeral1hInputTokens = toInt(cc["ephemeral_1h_input_tokens"])
					}
					finalizeUsageTotalsAnthropic(&result)
				}
			}
		}
		if eventType == "message_delta" {
			if d := obj(j["delta"]); d != nil {
				if sr, ok := str(d["stop_reason"]); ok && sr != "" {
					result.StopReason = sr
				}
			}
			if u := obj(j["usage"]); u != nil {
				if v, ok := u["output_tokens"]; ok && v != nil {
					result.OutputTokens = toInt(v)
				}
				if v, ok := u["cache_creation_input_tokens"]; ok && v != nil {
					result.CacheCreationInputTokens = toInt(v)
				}
				if v, ok := u["cache_read_input_tokens"]; ok && v != nil {
					result.CacheReadInputTokens = toInt(v)
				}
				finalizeUsageTotalsAnthropic(&result)
			}
		}
	}
	return result
}

// --- OpenAI ---

func parseOpenAIUsage(body string) UsageData {
	result := emptyUsage()
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return result
	}

	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		v, err := decode(trimmed)
		if err != nil {
			return result
		}
		applyOpenAIPayload(v, &result)
		return result
	}

	for _, event := range strings.Split(trimmed, "\n\n") {
		for _, line := range strings.Split(event, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimSpace(line[len("data: "):])
			if data == "" || data == "[DONE]" {
				continue
			}
			if v, err := decode(data); err == nil {
				applyOpenAIPayload(v, &result)
			}
		}
	}

	if result.TotalTokens == 0 && (result.InputTokens > 0 || result.OutputTokens > 0) {
		result.TotalTokens = result.InputTokens + result.OutputTokens
	}
	return result
}

func applyOpenAIPayload(parsed interface{}, result *UsageData) {
	j := obj(parsed)
	if j == nil {
		return
	}
	payload := j
	if r := obj(j["response"]); r != nil {
		payload = r
	}

	if m, ok := str(payload["model"]); ok && m != "" {
		result.Model = m
	}
	if sr, ok := str(payload["stop_reason"]); ok && sr != "" {
		result.StopReason = sr
	}
	if result.StopReason == "" {
		if payload["status"] == "incomplete" {
			if details := obj(payload["incomplete_details"]); details != nil {
				if reason, ok := str(details["reason"]); ok {
					result.StopReason = reason
				}
			}
		}
	}
	if choices, ok := payload["choices"].([]interface{}); ok {
		for _, c := range choices {
			choice := obj(c)
			if choice == nil {
				continue
			}
			if fr, ok := str(choice["finish_reason"]); ok && fr != "" {
				result.StopReason = fr
				break
			}
		}
	}

	usage := obj(payload["usage"])
	if usage == nil {
		return
	}
	if isNum(usage["input_tokens"]) {
		result.InputTokens = toInt(usage["input_tokens"])
	}
	if isNum(usage["output_tokens"]) {
		result.OutputTokens = toInt(usage["output_tokens"])
	}
	if isNum(usage["prompt_tokens"]) {
		result.InputTokens = toInt(usage["prompt_tokens"])
	}
	if isNum(usage["completion_tokens"]) {
		result.OutputTokens = toInt(usage["completion_tokens"])
	}
	if isNum(usage["total_tokens"]) {
		result.TotalTokens = toInt(usage["total_tokens"])
	}

	inputDetails := firstObj(usage, "input_tokens_details", "prompt_tokens_details")
	if inputDetails != nil && isNum(inputDetails["cached_tokens"]) {
		result.CachedInputTokens = toInt(inputDetails["cached_tokens"])
	}
	outputDetails := firstObj(usage, "output_tokens_details", "completion_tokens_details")
	if outputDetails != nil && isNum(outputDetails["reasoning_tokens"]) {
		result.ReasoningOutputTokens = toInt(outputDetails["reasoning_tokens"])
	}

	if result.TotalTokens == 0 && (result.InputTokens > 0 || result.OutputTokens > 0) {
		result.TotalTokens = result.InputTokens + result.OutputTokens
	}
}

func firstObj(o map[string]interface{}, names ...string) map[string]interface{} {
	for _, n := range names {
		if rec := obj(o[n]); rec != nil {
			return rec
		}
	}
	return nil
}
