// Package responsesconv implements the bidirectional OpenAI Responses API ↔
// Chat Completions converter.
//
// This is a direct Go port of the original TypeScript module
// src/openai-responses-chat-compat.ts. It handles three transformations:
//
//   - Request: Responses API request → Chat Completions request
//   - Response (non-streaming): Chat Completions JSON → Responses API JSON
//   - Response (streaming): Chat Completions SSE → Responses API SSE
//
// The streaming path is a true incremental state machine; it transforms
// chunk-by-chunk and replicates the <think>-tag partial-buffer holdback logic
// so reasoning blocks split across SSE chunks are emitted correctly.
package responsesconv

// JSON maps mirror the original "JsonRecord" (Record<string, unknown>).
// Using interface{} keeps the code a faithful 1:1 port of the dynamic JSON
// manipulation in the TS source, which is simpler and less error-prone than
// declaring a full type model for these loosely-typed provider payloads.
type (
	Obj = map[string]interface{}
	Arr = []interface{}
)

// isRecord reports whether v is a JSON object (map[string]interface{}).
func isRecord(v interface{}) bool {
	_, ok := v.(Obj)
	return ok
}

// asString returns v as a string, or "" if it is not a string.
func asString(v interface{}) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// asNumber returns v as a float64. Accepts float64 and int (defensive: callers
// sometimes pass Go-native ints). Returns (0,false) otherwise.
func asNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// asBool returns v as a bool plus whether it was a bool.
func asBool(v interface{}) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// toObj asserts v to an Obj, returning nil if it is not an object.
func toObj(v interface{}) Obj {
	o, _ := v.(Obj)
	return o
}

// toArr asserts v to an Arr, returning nil if it is not an array slice. It
// accepts both Arr ([]interface{}) and []Obj (the typed slice produced by the
// message builders) so callers don't have to convert between the two.
func toArr(v interface{}) Arr {
	switch a := v.(type) {
	case Arr:
		return a
	case []Obj:
		out := make(Arr, len(a))
		for i, item := range a {
			out[i] = item
		}
		return out
	case nil:
		return nil
	}
	// Not a recognized array type.
	return nil
}

// strField returns obj[key] if it is a string, else ("", false).
func strField(obj Obj, key string) (string, bool) {
	if obj == nil {
		return "", false
	}
	return asString(obj[key])
}

// numberField returns obj[key] if it is a number, else (0, false).
func numberField(obj Obj, key string) (float64, bool) {
	if obj == nil {
		return 0, false
	}
	return asNumber(obj[key])
}

// normalizeFunctionArguments mirrors the TS helper: strings pass through,
// nil → "", objects are JSON-encoded, anything else is JSON-encoded too.
func normalizeFunctionArguments(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	out, err := jsonMarshal(v)
	if err != nil {
		return ""
	}
	return out
}

// jsonMarshal is a thin wrapper kept here so call sites stay short and the
// error handling is centralized (all our values are JSON-serializable).
func jsonMarshal(v interface{}) (string, error) {
	return jsonEncode(v)
}
