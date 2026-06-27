package responsesconv

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// parseSseEvent mirrors the test helper in the TS suite: find an SSE block
// containing "event: <name>", concatenate its data: lines, and JSON-parse.
func parseSseEvent(t *testing.T, text, eventName string) Obj {
	t.Helper()
	blocks := strings.Split(text, "\n\n")
	for _, block := range blocks {
		if !strings.Contains(block, "event: "+eventName) {
			continue
		}
		var dataParts []string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				dataParts = append(dataParts, line[len("data: "):])
			}
		}
		data := strings.Join(dataParts, "\n")
		v, err := jsonDecode([]byte(data))
		if err != nil {
			t.Fatalf("parse data for %s: %v", eventName, err)
		}
		return toObj(v)
	}
	t.Fatalf("Missing SSE event %s", eventName)
	return nil
}

func jsonStr(t *testing.T, v interface{}) string {
	t.Helper()
	s, err := jsonEncode(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func decodeObj(t *testing.T, s string) Obj {
	t.Helper()
	v, err := jsonDecode([]byte(s))
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return toObj(v)
}

// decodeAny parses a JSON string into a generic interface{} for structural
// comparison. Used for "want" values where the top-level type may be an array
// or object.
func decodeAny(t *testing.T, s string) interface{} {
	t.Helper()
	v, err := jsonDecode([]byte(s))
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return v
}

// ---------- Request conversion ----------

func TestConvertResponsesRequest_BasicFlow(t *testing.T) {
	input := jsonStr(t, Obj{
		"model":             "gpt-test",
		"instructions":      "Be concise.",
		"max_output_tokens": 42,
		"input": Arr{
			Obj{"role": "user", "content": Arr{Obj{"type": "input_text", "text": "Return JSON."}}},
			Obj{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"query":"status"}`},
			Obj{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		},
		"text": Obj{"format": Obj{
			"type":   "json_schema",
			"name":   "Answer",
			"strict": true,
			"schema": Obj{
				"type":       "object",
				"properties": Obj{"answer": Obj{"type": "string"}},
				"required":   Arr{"answer"},
			},
		}},
		"tools":       Arr{Obj{"type": "function", "name": "lookup", "parameters": Obj{"type": "object"}}},
		"tool_choice": Obj{"type": "function", "name": "lookup"},
		"stream":      true,
	})

	res := ConvertResponsesRequestToChatCompletions(input, nil)
	if !res.OK {
		t.Fatalf("expected OK, got error: %s", res.Error.Message)
	}
	body := decodeObj(t, res.Body)

	if body["model"] != "gpt-test" {
		t.Errorf("model: got %v", body["model"])
	}
	if body["stream"] != true {
		t.Errorf("stream: got %v", body["stream"])
	}
	if body["max_tokens"] != float64(42) {
		t.Errorf("max_tokens: got %v", body["max_tokens"])
	}

	wantMessages := decodeAny(t, compactJSON(`[
	  {"role":"system","content":"Be concise."},
	  {"role":"user","content":"Return JSON."},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"status\"}"}}]},
	  {"role":"tool","tool_call_id":"call_1","content":"ok"}
	]`))
	if !equalJSON(body["messages"], wantMessages) {
		t.Errorf("messages mismatch:\n got: %s", jsonStr(t, body["messages"]))
	}

	wantRF := decodeAny(t, `{"type":"json_schema","json_schema":{"name":"Answer","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}}}`)
	if !equalJSON(body["response_format"], wantRF) {
		t.Errorf("response_format mismatch:\n got: %s", jsonStr(t, body["response_format"]))
	}

	wantTools := decodeAny(t, `[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`)
	if !equalJSON(body["tools"], wantTools) {
		t.Errorf("tools mismatch:\n got: %s", jsonStr(t, body["tools"]))
	}

	wantChoice := decodeAny(t, `{"type":"function","function":{"name":"lookup"}}`)
	if !equalJSON(body["tool_choice"], wantChoice) {
		t.Errorf("tool_choice mismatch:\n got: %s", jsonStr(t, body["tool_choice"]))
	}
}

func TestConvertResponsesRequest_DropsBuiltinTools(t *testing.T) {
	input := jsonStr(t, Obj{
		"model": "gpt-test",
		"input": "hello",
		"tools": Arr{
			Obj{"type": "web_search_preview"},
			Obj{"type": "function", "name": "lookup", "parameters": Obj{"type": "object"}},
		},
		"tool_choice": Obj{"type": "web_search_preview"},
	})
	res := ConvertResponsesRequestToChatCompletions(input, nil)
	if !res.OK {
		t.Fatal(res.Error.Message)
	}
	body := decodeObj(t, res.Body)
	wantTools := decodeAny(t, `[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`)
	if !equalJSON(body["tools"], wantTools) {
		t.Errorf("tools: got %s", jsonStr(t, body["tools"]))
	}
	if _, present := body["tool_choice"]; present {
		t.Errorf("tool_choice should be absent, got %v", body["tool_choice"])
	}
}

func TestConvertResponsesRequest_OmitsToolFieldsWhenAllBuiltin(t *testing.T) {
	input := jsonStr(t, Obj{
		"model":       "gpt-test",
		"input":       "hello",
		"tools":       Arr{Obj{"type": "web_search"}},
		"tool_choice": "required",
	})
	res := ConvertResponsesRequestToChatCompletions(input, nil)
	if !res.OK {
		t.Fatal(res.Error.Message)
	}
	body := decodeObj(t, res.Body)
	if _, present := body["tools"]; present {
		t.Errorf("tools should be absent: %v", body["tools"])
	}
	if _, present := body["tool_choice"]; present {
		t.Errorf("tool_choice should be absent: %v", body["tool_choice"])
	}
}

func TestConvertResponsesRequest_MiniMaxSanitization(t *testing.T) {
	input := jsonStr(t, Obj{
		"model":        "MiniMax-M2.7",
		"instructions": "Follow the system rules.",
		"input": Arr{
			Obj{"type": "message", "role": "developer", "content": Arr{Obj{"type": "input_text", "text": "Use the workspace tools carefully."}}},
			Obj{"type": "message", "role": "user", "content": Arr{Obj{"type": "input_text", "text": "hello"}}},
		},
		"tools": Arr{
			Obj{"type": "function", "name": "exec_command", "strict": false, "parameters": Obj{
				"type":                 "object",
				"properties":           Obj{"cmd": Obj{"type": "string"}},
				"required":             Arr{"cmd"},
				"additionalProperties": false,
			}},
			Obj{"type": "web_search"},
		},
		"tool_choice":         "auto",
		"store":               false,
		"metadata":            Obj{"user_id": "debug"},
		"service_tier":        "auto",
		"parallel_tool_calls": false,
		"logprobs":            true,
		"stream":              true,
		"max_output_tokens":   128,
		"temperature":         0,
		"top_p":               0.9,
	})
	res := ConvertResponsesRequestToChatCompletions(input, &RequestOptions{TargetURL: "https://api.minimaxi.com/v1/chat/completions"})
	if !res.OK {
		t.Fatal(res.Error.Message)
	}

	want := compactJSON(`{
	  "model":"MiniMax-M2.7",
	  "messages":[
	    {"role":"system","content":"Follow the system rules.\n\nUse the workspace tools carefully."},
	    {"role":"user","content":"hello"}
	  ],
	  "stream":true,
	  "max_completion_tokens":128,
	  "top_p":0.9,
	  "tools":[{"type":"function","function":{"name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"],"additionalProperties":false}}}]
	}`)
	// Field order: Go maps are unordered, so compare by re-decoding and checking
	// key-by-key rather than exact string equality.
	body := decodeObj(t, res.Body)
	wantObj := decodeObj(t, want)
	for _, k := range []string{"model", "stream", "max_completion_tokens", "top_p"} {
		if !equalJSON(body[k], wantObj[k]) {
			t.Errorf("key %s: got %v, want %v", k, body[k], wantObj[k])
		}
	}
	if !equalJSON(body["messages"], wantObj["messages"]) {
		t.Errorf("messages:\n got: %s\n want: %s", jsonStr(t, body["messages"]), jsonStr(t, wantObj["messages"]))
	}
	if !equalJSON(body["tools"], wantObj["tools"]) {
		t.Errorf("tools:\n got: %s\n want: %s", jsonStr(t, body["tools"]), jsonStr(t, wantObj["tools"]))
	}
	// temperature=0 is dropped by MiniMax sanitizer (range is exclusive of 0).
	if _, present := body["temperature"]; present {
		t.Errorf("temperature should be dropped for MiniMax, got %v", body["temperature"])
	}
}

// ---------- Non-streaming response conversion ----------

func TestConvertChatCompletion_NonStreaming(t *testing.T) {
	cc := Obj{
		"id":      "chatcmpl_123",
		"object":  "chat.completion",
		"created": float64(123),
		"model":   "gpt-test",
		"choices": Arr{Obj{
			"index": float64(0),
			"message": Obj{
				"role":    "assistant",
				"content": "Hello",
				"tool_calls": Arr{Obj{
					"id":       "call_1",
					"type":     "function",
					"function": Obj{"name": "lookup", "arguments": `{"q":"x"}`},
				}},
			},
			"finish_reason": "stop",
		}},
		"usage": Obj{
			"prompt_tokens":             float64(10),
			"completion_tokens":         float64(4),
			"total_tokens":              float64(14),
			"prompt_tokens_details":     Obj{"cached_tokens": float64(6)},
			"completion_tokens_details": Obj{"reasoning_tokens": float64(2)},
		},
	}
	out, err := ConvertChatCompletionToResponsePayload(cc)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeObj(t, out)

	if resp["id"] != "resp_chatcmpl_123" {
		t.Errorf("id: got %v", resp["id"])
	}
	if resp["object"] != "response" {
		t.Errorf("object: got %v", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Errorf("status: got %v", resp["status"])
	}
	if resp["output_text"] != "Hello" {
		t.Errorf("output_text: got %v", resp["output_text"])
	}
	output := toArr(resp["output"])
	firstMsg := toObj(toArr(toObj(output[0])["content"])[0])
	if firstMsg["text"] != "Hello" {
		t.Errorf("output[0].content[0].text: got %v", firstMsg["text"])
	}
	second := toObj(output[1])
	if second["type"] != "function_call" || second["call_id"] != "call_1" || second["name"] != "lookup" || second["arguments"] != `{"q":"x"}` {
		t.Errorf("output[1] mismatch: %s", jsonStr(t, second))
	}
	wantUsage := decodeAny(t, `{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":6},"output_tokens_details":{"reasoning_tokens":2}}`)
	if !equalJSON(resp["usage"], wantUsage) {
		t.Errorf("usage:\n got: %s", jsonStr(t, resp["usage"]))
	}
}

func TestConvertChatCompletion_ThinkTagSplitting(t *testing.T) {
	cc := Obj{
		"id":      "chatcmpl_think",
		"object":  "chat.completion",
		"created": float64(321),
		"model":   "gpt-test",
		"choices": Arr{Obj{
			"message": Obj{
				"role":    "assistant",
				"content": "<think>Plan the answer first.</think>Hello",
			},
			"finish_reason": "stop",
		}},
		"usage": Obj{"prompt_tokens": float64(10), "completion_tokens": float64(8), "total_tokens": float64(18)},
	}
	out, err := ConvertChatCompletionToResponsePayload(cc)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeObj(t, out)
	if resp["output_text"] != "Hello" {
		t.Errorf("output_text: got %v", resp["output_text"])
	}
	output := toArr(resp["output"])
	wantFirst := decodeAny(t, `{"id":"rs_chatcmpl_think_0","type":"reasoning","content":[{"type":"reasoning_text","text":"Plan the answer first."}],"summary":[]}`)
	if !equalJSON(output[0], wantFirst) {
		t.Errorf("output[0]:\n got: %s", jsonStr(t, output[0]))
	}
	second := toObj(output[1])
	if second["id"] != "msg_chatcmpl_think_1" || second["type"] != "message" || second["role"] != "assistant" {
		t.Errorf("output[1]: %s", jsonStr(t, second))
	}
}

func TestConvertChatCompletion_ReasoningContentFallback(t *testing.T) {
	cc := Obj{
		"id":      "chatcmpl_kimi",
		"created": float64(123),
		"model":   "kimi-for-coding",
		"choices": Arr{Obj{
			"message": Obj{
				"role":              "assistant",
				"content":           "",
				"reasoning_content": "1+1等于2。",
			},
			"finish_reason": "stop",
		}},
	}
	out, err := ConvertChatCompletionToResponsePayload(cc)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeObj(t, out)
	if resp["output_text"] != "1+1等于2。" {
		t.Errorf("output_text: got %v", resp["output_text"])
	}
	first := toObj(toArr(resp["output"])[0])
	part := toObj(toArr(first["content"])[0])
	if part["text"] != "1+1等于2。" {
		t.Errorf("fallback text: got %v", part["text"])
	}
}

// ---------- Streaming conversion ----------

func TestTransformResponse_StreamingSSE(t *testing.T) {
	chatSSE := strings.Join([]string{
		`data: {"id":"chatcmpl_789","object":"chat.completion.chunk","created":789,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_789","object":"chat.completion.chunk","created":789,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_789","object":"chat.completion.chunk","created":789,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_789","object":"chat.completion.chunk","created":789,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	resp := TransformResponse(&http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(chatSSE)),
	})

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)

	if !strings.Contains(text, "event: response.output_text.delta") {
		t.Error("missing output_text.delta event")
	}
	if !strings.Contains(text, `"delta":"Hel"`) {
		t.Error("missing Hel delta")
	}
	if !strings.Contains(text, `"delta":"lo"`) {
		t.Error("missing lo delta")
	}

	completed := parseSseEvent(t, text, "response.completed")
	respObj := toObj(completed["response"])
	if respObj["object"] != "response" {
		t.Errorf("response.object: %v", respObj["object"])
	}
	if respObj["output_text"] != "Hello" {
		t.Errorf("response.output_text: %v", respObj["output_text"])
	}
	wantUsage := decodeAny(t, `{"input_tokens":5,"output_tokens":2,"total_tokens":7,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`)
	if !equalJSON(respObj["usage"], wantUsage) {
		t.Errorf("usage:\n got: %s", jsonStr(t, respObj["usage"]))
	}
}

// TestTransformResponse_ThinkStreaming is the hardest case: <think> tags split
// across SSE chunks must be reassembled into reasoning/text events without
// leaking the tags or mis-splitting partial tags.
func TestTransformResponse_ThinkStreaming(t *testing.T) {
	chatSSE := strings.Join([]string{
		`data: {"id":"chatcmpl_think_stream","object":"chat.completion.chunk","created":790,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_think_stream","object":"chat.completion.chunk","created":790,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"<think>pla"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_think_stream","object":"chat.completion.chunk","created":790,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"n</think>Hel"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_think_stream","object":"chat.completion.chunk","created":790,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_think_stream","object":"chat.completion.chunk","created":790,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":6,"total_tokens":11}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	resp := TransformResponse(&http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(chatSSE)),
	})
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, "event: response.reasoning_text.delta") {
		t.Error("missing reasoning_text.delta")
	}
	if !strings.Contains(text, "event: response.output_text.delta") {
		t.Error("missing output_text.delta")
	}
	if strings.Contains(text, "<think>") || strings.Contains(text, "</think>") {
		t.Errorf("think tags leaked into output:\n%s", text)
	}

	completed := parseSseEvent(t, text, "response.completed")
	respObj := toObj(completed["response"])
	if respObj["output_text"] != "Hello" {
		t.Errorf("output_text: got %v", respObj["output_text"])
	}
	output := toArr(respObj["output"])
	wantFirst := decodeAny(t, `{"id":"rs_chatcmpl_think_stream_0","type":"reasoning","content":[{"type":"reasoning_text","text":"plan"}],"summary":[]}`)
	if !equalJSON(output[0], wantFirst) {
		t.Errorf("output[0]:\n got: %s", jsonStr(t, output[0]))
	}
	second := toObj(output[1])
	if second["id"] != "msg_chatcmpl_think_stream_1" || second["type"] != "message" || second["role"] != "assistant" {
		t.Errorf("output[1]: %s", jsonStr(t, second))
	}
}

func TestTransformResponse_ReasoningContentStreamingFallback(t *testing.T) {
	chatSSE := strings.Join([]string{
		`data:{"id":"chatcmpl_kimi_stream","object":"chat.completion.chunk","created":789,"model":"kimi-for-coding","choices":[{"index":0,"delta":{"role":"assistant","content":null},"finish_reason":null}]}`,
		``,
		`data:{"id":"chatcmpl_kimi_stream","object":"chat.completion.chunk","created":789,"model":"kimi-for-coding","choices":[{"index":0,"delta":{"reasoning_content":"1+1"},"finish_reason":null}]}`,
		``,
		`data:{"id":"chatcmpl_kimi_stream","object":"chat.completion.chunk","created":789,"model":"kimi-for-coding","choices":[{"index":0,"delta":{"reasoning_content":"等于2。"},"finish_reason":null}]}`,
		``,
		`data:{"id":"chatcmpl_kimi_stream","object":"chat.completion.chunk","created":789,"model":"kimi-for-coding","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	resp := TransformResponse(&http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(chatSSE)),
	})
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, `"delta":"1+1"`) || !strings.Contains(text, `"delta":"等于2。"`) {
		t.Errorf("missing reasoning_content fallback deltas:\n%s", text)
	}
	completed := parseSseEvent(t, text, "response.completed")
	respObj := toObj(completed["response"])
	if respObj["output_text"] != "1+1等于2。" {
		t.Errorf("output_text: got %v", respObj["output_text"])
	}
}

func TestTransformResponse_NonStreamingJSON(t *testing.T) {
	body := `{"id":"chatcmpl_456","created":456,"model":"gpt-test","choices":[{"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	resp := TransformResponse(&http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "Content-Length": []string{"999"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	})
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q", ct)
	}
	if resp.Header.Get("Content-Length") != "" {
		t.Errorf("content-length should be stripped")
	}
	data, _ := io.ReadAll(resp.Body)
	obj := decodeObj(t, string(data))
	if obj["object"] != "response" {
		t.Errorf("object: %v", obj["object"])
	}
	first := toObj(toArr(obj["output"])[0])
	if toObj(toArr(first["content"])[0])["text"] != "Done" {
		t.Errorf("text: %v", toObj(toArr(first["content"])[0])["text"])
	}
}
