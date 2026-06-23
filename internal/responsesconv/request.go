package responsesconv

import (
	"math"
	"net/url"
	"regexp"
	"strings"
)

// requestDirectFields are copied verbatim from the Responses request into the
// Chat Completions payload when present.
var requestDirectFields = []string{
	"model", "temperature", "top_p", "stop", "presence_penalty", "frequency_penalty",
	"logit_bias", "user", "seed", "stream", "stream_options", "store", "metadata",
	"service_tier", "parallel_tool_calls", "logprobs", "top_logprobs",
}

func convertResponsesToolsToChatTools(tools interface{}) ([]Obj, error) {
	if tools == nil {
		return nil, nil
	}
	arr := toArr(tools)
	if arr == nil {
		return nil, newError("tools must be an array.", "tools", "")
	}
	var converted []Obj
	for i, tool := range arr {
		rec := toObj(tool)
		if rec == nil {
			return nil, newError("Each tool must be an object.", sprintf("tools[%d]", i), "")
		}
		if rec["type"] != "function" {
			continue
		}
		if isRecord(rec["function"]) {
			converted = append(converted, Obj{"type": "function", "function": rec["function"]})
			continue
		}
		name, ok := strField(rec, "name")
		if !ok {
			return nil, newError("Function tool requires a name.", sprintf("tools[%d].name", i), "")
		}
		fn := Obj{"name": name}
		if d, ok := strField(rec, "description"); ok {
			fn["description"] = d
		}
		if isRecord(rec["parameters"]) {
			fn["parameters"] = rec["parameters"]
		}
		if b, ok := asBool(rec["strict"]); ok {
			fn["strict"] = b
		}
		converted = append(converted, Obj{"type": "function", "function": fn})
	}
	if len(converted) == 0 {
		return nil, nil
	}
	return converted, nil
}

func convertResponsesToolChoiceToChat(toolChoice interface{}, hasChatTools bool) interface{} {
	if toolChoice == nil || !hasChatTools {
		return nil
	}
	if s, ok := asString(toolChoice); ok {
		return s
	}
	rec := toObj(toolChoice)
	if rec == nil {
		return nil
	}

	var name string
	if n, ok := strField(rec, "name"); ok {
		name = n
	} else if fn := toObj(rec["function"]); fn != nil {
		if n, ok := strField(fn, "name"); ok {
			name = n
		}
	}

	if rec["type"] == "function" && name != "" {
		return Obj{"type": "function", "function": Obj{"name": name}}
	}
	return nil
}

func convertResponsesTextFormatToChatResponseFormat(text interface{}) (interface{}, error) {
	t := toObj(text)
	if t == nil || !isRecord(t["format"]) {
		return nil, nil
	}
	format := toObj(t["format"])

	switch format["type"] {
	case "text":
		return nil, nil
	case "json_object":
		return Obj{"type": "json_object"}, nil
	case "json_schema":
		// fallthrough handled below
	default:
		return nil, newErrorf("text.format.type", `Unsupported Responses text.format type "%v".`, format["type"])
	}

	schemaSource := toObj(format["json_schema"])
	if schemaSource == nil {
		schemaSource = format
	}
	name, ok := strField(schemaSource, "name")
	if !ok {
		name = "Output"
	}
	var schema interface{} = Obj{"type": "object"}
	if isRecord(schemaSource["schema"]) {
		schema = schemaSource["schema"]
	}
	js := Obj{"name": name}
	if b, ok := asBool(schemaSource["strict"]); ok {
		js["strict"] = b
	}
	js["schema"] = schema
	return Obj{"type": "json_schema", "json_schema": js}, nil
}

// --- MiniMax compatibility special-casing ---

var minimaxModelRe = regexp.MustCompile(`(?i)^(codex-)?minimax-`)

func isMiniMaxChatCompatTarget(body Obj, opts *RequestOptions) bool {
	if model, ok := strField(body, "model"); ok && minimaxModelRe.MatchString(model) {
		return true
	}
	if opts == nil || opts.TargetURL == "" {
		return false
	}
	if u, err := url.Parse(opts.TargetURL); err == nil && u.Hostname() != "" {
		return strings.Contains(strings.ToLower(u.Hostname()), "minimax")
	}
	return strings.Contains(strings.ToLower(opts.TargetURL), "minimax")
}

func sanitizeMiniMaxTools(tools interface{}) []Obj {
	arr := toArr(tools)
	if arr == nil {
		return nil
	}
	var out []Obj
	for _, tool := range arr {
		rec := toObj(tool)
		if rec == nil || rec["type"] != "function" || !isRecord(rec["function"]) {
			continue
		}
		fn := Obj{}
		for k, v := range rec["function"].(Obj) {
			if k == "strict" {
				continue
			}
			fn[k] = v
		}
		out = append(out, Obj{"type": "function", "function": fn})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func numberInRange(v interface{}, minExclusive, maxInclusive float64) (float64, bool) {
	n, ok := asNumber(v)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	if n <= minExclusive || n > maxInclusive {
		return 0, false
	}
	return n, true
}

func positiveInteger(v interface{}) (int, bool) {
	n, ok := asNumber(v)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 1 {
		return 0, false
	}
	return int(math.Trunc(n)), true
}

func sanitizeMiniMaxChatPayload(chatPayload Obj) Obj {
	out := Obj{}
	if s, ok := strField(chatPayload, "model"); ok {
		out["model"] = s
	}
	if a := toArr(chatPayload["messages"]); a != nil {
		out["messages"] = a
	}
	if b, ok := asBool(chatPayload["stream"]); ok {
		out["stream"] = b
	}
	mct, ok := positiveInteger(chatPayload["max_completion_tokens"])
	if !ok {
		mct, ok = positiveInteger(chatPayload["max_tokens"])
	}
	if ok {
		out["max_completion_tokens"] = mct
	}
	if t, ok := numberInRange(chatPayload["temperature"], 0, 1); ok {
		out["temperature"] = t
	}
	if p, ok := numberInRange(chatPayload["top_p"], 0, 1); ok {
		out["top_p"] = p
	}
	if tools := sanitizeMiniMaxTools(chatPayload["tools"]); tools != nil {
		out["tools"] = tools
	}
	return out
}

// ConvertResponsesRequestToChatCompletions is the top-level request translator.
// Mirrors convertResponsesRequestToChatCompletions in the TS source.
func ConvertResponsesRequestToChatCompletions(rawBodyText string, opts *RequestOptions) RequestResult {
	body, err := jsonDecodeObj([]byte(rawBodyText))
	if err != nil {
		if err == errNotObject {
			return RequestResult{OK: false, Error: newError("Request body must be a JSON object.", "", "")}
		}
		return RequestResult{OK: false, Error: newError("Request body must be valid JSON.", "", "")}
	}

	chatPayload, rerr := buildChatPayload(body, opts)
	if rerr != nil {
		ce, _ := AsCompatError(rerr)
		return RequestResult{OK: false, Error: ce}
	}

	return RequestResult{
		OK:           true,
		Body:         jsonMustEncode(chatPayload),
		RequestModel: requestModelOf(chatPayload),
	}
}

func requestModelOf(payload Obj) string {
	if s, ok := strField(payload, "model"); ok {
		return s
	}
	return "unknown"
}

// buildChatPayload performs the guarded payload construction, returning a
// structured error (via compatErrorSentinel) for any validation failure.
func buildChatPayload(body Obj, opts *RequestOptions) (Obj, error) {
	defer func() {}() // no panic recovery needed; errors flow normally

	if err := guardUnsupported(body); err != nil {
		return nil, err
	}

	chatPayload := Obj{}
	for _, field := range requestDirectFields {
		if v, ok := body[field]; ok {
			// TS: body[field] !== undefined. JSON decode never sets "undefined";
			// presence in the map is the equivalent check.
			chatPayload[field] = v
		}
	}

	messages, err := convertResponsesInputToChatMessages(body["input"])
	if err != nil {
		return nil, err
	}
	if instr, ok := strField(body, "instructions"); ok && instr != "" {
		// unshift: prepend.
		messages = append([]Obj{{"role": "system", "content": instr}}, messages...)
	}
	chatPayload["messages"] = mergeLeadingSystemMessages(messages)

	if v, ok := body["max_output_tokens"]; ok {
		chatPayload["max_tokens"] = v
	}
	if v, ok := body["max_completion_tokens"]; ok {
		chatPayload["max_completion_tokens"] = v
	}

	tools, err := convertResponsesToolsToChatTools(body["tools"])
	if err != nil {
		return nil, err
	}
	hasChatTools := tools != nil
	if hasChatTools {
		chatPayload["tools"] = tools
	}

	toolChoice := convertResponsesToolChoiceToChat(body["tool_choice"], hasChatTools)
	if toolChoice != nil {
		chatPayload["tool_choice"] = toolChoice
	}

	responseFormat, err := convertResponsesTextFormatToChatResponseFormat(body["text"])
	if err != nil {
		return nil, err
	}
	if responseFormat != nil {
		chatPayload["response_format"] = responseFormat
	}
	if v, ok := body["response_format"]; ok {
		if _, set := chatPayload["response_format"]; !set {
			chatPayload["response_format"] = v
		}
	}

	if reasoning := toObj(body["reasoning"]); reasoning != nil {
		if effort, ok := strField(reasoning, "effort"); ok {
			chatPayload["reasoning_effort"] = effort
		}
	}

	if isMiniMaxChatCompatTarget(body, opts) {
		return sanitizeMiniMaxChatPayload(chatPayload), nil
	}
	return chatPayload, nil
}

// guardUnsupported rejects Responses-only features that Chat Completions
// cannot represent. Each returns a compatErrorSentinel so the caller can
// surface it as a structured 400 error.
func guardUnsupported(body Obj) error {
	if v, ok := body["previous_response_id"]; ok && v != nil {
		return newError(
			"previous_response_id is not supported by Chat Completions compatibility; pass prior turns explicitly in input.",
			"previous_response_id", "")
	}
	if v, ok := body["conversation"]; ok && v != nil {
		return newError(
			"conversation is not supported by Chat Completions compatibility; pass prior turns explicitly in input.",
			"conversation", "")
	}
	if v, ok := body["n"]; ok {
		if n, ok := asNumber(v); ok && n != 1 {
			return newError(
				"Responses API does not support n > 1; Chat Completions compatibility only supports one generation.",
				"n", "")
		}
	}
	return nil
}
