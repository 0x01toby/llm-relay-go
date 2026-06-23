package responsesconv

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// This file ports the non-streaming response translator: Chat Completions
// JSON → Responses API JSON object.

func randomUUIDish() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	// RFC 4122 v4 variant bits, mirroring crypto.randomUUID's format.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// toResponseId prefixes with "resp_" when missing. Falls back to a random UUID.
func toResponseID(chatID interface{}) string {
	id, ok := asString(chatID)
	if !ok || id == "" {
		id = randomUUIDish()
	}
	if !strings.HasPrefix(id, "resp_") {
		id = "resp_" + id
	}
	return id
}

// generatedItemId mirrors the TS ID builder: prefix + sanitized responseId + index.
func generatedItemID(prefix, responseID string, index int) string {
	stripped := strings.TrimPrefix(responseID, "resp_")
	safe := sanitizeID(stripped)
	return sprintf("%s_%s_%d", prefix, safe, index)
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func convertChatUsageToResponsesUsage(usage interface{}) interface{} {
	u := toObj(usage)
	if u == nil {
		return nil
	}

	inputTokens := numberOrZero(u, "input_tokens", "prompt_tokens")
	outputTokens := numberOrZero(u, "output_tokens", "completion_tokens")
	totalTokens, ok := numberField(u, "total_tokens")
	if !ok {
		totalTokens = inputTokens + outputTokens
	}

	promptDetails := firstRecord(u, "input_tokens_details", "prompt_tokens_details")
	completionDetails := firstRecord(u, "output_tokens_details", "completion_tokens_details")

	return Obj{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  totalTokens,
		"input_tokens_details": Obj{
			"cached_tokens": recordNumberOrZero(promptDetails, "cached_tokens"),
		},
		"output_tokens_details": Obj{
			"reasoning_tokens": recordNumberOrZero(completionDetails, "reasoning_tokens"),
		},
	}
}

// numberOrZero returns the first present numeric field among names.
func numberOrZero(o Obj, names ...string) float64 {
	for _, n := range names {
		if v, ok := numberField(o, n); ok {
			return v
		}
	}
	return 0
}

func firstRecord(o Obj, names ...string) Obj {
	for _, n := range names {
		if rec := toObj(o[n]); rec != nil {
			return rec
		}
	}
	return Obj{}
}

func recordNumberOrZero(o Obj, name string) float64 {
	if v, ok := numberField(o, name); ok {
		return v
	}
	return 0
}

type finishStatus struct {
	status            string
	incompleteDetails interface{}
}

func statusFromFinishReason(finishReason interface{}) finishStatus {
	s, _ := asString(finishReason)
	switch s {
	case "length":
		return finishStatus{"incomplete", Obj{"reason": "max_output_tokens"}}
	case "content_filter":
		return finishStatus{"incomplete", Obj{"reason": "content_filter"}}
	default:
		return finishStatus{"completed", nil}
	}
}

func extractChatMessageTextContent(content interface{}) string {
	if s, ok := asString(content); ok {
		return s
	}
	if arr := toArr(content); arr != nil {
		var b strings.Builder
		for _, part := range arr {
			if s, ok := asString(part); ok {
				b.WriteString(s)
				continue
			}
			if isRecord(part) {
				o := part.(Obj)
				if o["type"] == "text" {
					if text, ok := strField(o, "text"); ok {
						b.WriteString(text)
					}
				}
			}
		}
		return b.String()
	}
	return ""
}

// convertChatMessageToResponseItems builds the output items for a single chat
// message, splitting <think> tags into reasoning/message items.
func convertChatMessageToResponseItems(message Obj, responseID string) []Obj {
	if refusal, ok := strField(message, "refusal"); ok && refusal != "" {
		return []Obj{{
			"id":     generatedItemID("msg", responseID, 0),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": Arr{
				Obj{"type": "refusal", "refusal": refusal},
			},
		}}
	}

	annotations := toArr(message["annotations"])
	if annotations == nil {
		annotations = Arr{}
	}
	segments := splitThinkTaggedText(extractChatMessageTextContent(message["content"]))

	out := make([]Obj, 0, len(segments))
	for i, seg := range segments {
		if seg.kind == thinkReasoning {
			out = append(out, Obj{
				"id":      generatedItemID("rs", responseID, i),
				"type":    "reasoning",
				"content": Arr{Obj{"type": "reasoning_text", "text": seg.text}},
				"summary": Arr{},
			})
			continue
		}
		out = append(out, Obj{
			"id":     generatedItemID("msg", responseID, i),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": Arr{
				Obj{"type": "output_text", "text": seg.text, "annotations": annotations},
			},
		})
	}
	return out
}

func convertChatToolCallsToResponseItems(toolCalls interface{}, responseID string, startIndex int) []Obj {
	arr := toArr(toolCalls)
	if arr == nil {
		return nil
	}
	var out []Obj
	for offset, tc := range arr {
		rec := toObj(tc)
		if rec == nil {
			continue
		}
		fn := toObj(rec["function"])
		if fn == nil {
			fn = Obj{}
		}
		name, ok := strField(fn, "name")
		if !ok || name == "" {
			continue
		}
		callID, hasID := strField(rec, "id")
		if !hasID {
			callID = generatedItemID("call", responseID, startIndex+offset)
		}
		out = append(out, Obj{
			"id":        generatedItemID("fc", responseID, startIndex+offset),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   callID,
			"name":      name,
			"arguments": normalizeFunctionArguments(fn["arguments"]),
		})
	}
	return out
}

func collectOutputText(output []Obj) string {
	var b strings.Builder
	for _, item := range output {
		arr := toArr(item["content"])
		if arr == nil {
			continue
		}
		for _, part := range arr {
			o := toObj(part)
			if o == nil {
				continue
			}
			if o["type"] == "output_text" {
				if text, ok := strField(o, "text"); ok {
					b.WriteString(text)
				}
			}
		}
	}
	return b.String()
}

// ConvertChatCompletionToResponsePayload builds a full Responses-API response
// object from a Chat Completions response. Returns an ordered JSON string.
// Mirrors convertChatCompletionToResponsePayload.
func ConvertChatCompletionToResponsePayload(chatCompletion interface{}) (string, error) {
	cc := toObj(chatCompletion)
	if cc == nil {
		return "", errNotObject
	}

	responseID := toResponseID(cc["id"])
	choices := toArr(cc["choices"])
	var firstChoice Obj
	if len(choices) > 0 {
		firstChoice = toObj(choices[0])
		if firstChoice == nil {
			firstChoice = Obj{}
		}
	} else {
		firstChoice = Obj{}
	}
	message := toObj(firstChoice["message"])
	if message == nil {
		message = Obj{}
	}
	fs := statusFromFinishReason(firstChoice["finish_reason"])

	output := convertChatMessageToResponseItems(message, responseID)
	output = append(output, convertChatToolCallsToResponseItems(message["tool_calls"], responseID, len(output))...)

	resp := newOrdered().
		set("id", responseID).
		set("object", "response").
		set("created_at", createdAtOf(cc)).
		set("status", fs.status).
		set("error", nil).
		set("incomplete_details", fs.incompleteDetails).
		set("model", modelOf(cc)).
		set("output", output).
		set("parallel_tool_calls", true).
		set("previous_response_id", nil).
		set("store", false).
		set("usage", convertChatUsageToResponsesUsage(cc["usage"]))

	if text := collectOutputText(output); text != "" {
		resp.set("output_text", text)
	}
	return resp.String(), nil
}

func createdAtOf(cc Obj) float64 {
	if n, ok := numberField(cc, "created"); ok {
		return n
	}
	return float64(time.Now().Unix())
}

func modelOf(cc Obj) string {
	if s, ok := strField(cc, "model"); ok {
		return s
	}
	return ""
}
