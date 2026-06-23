package responsesconv

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
)

// compactJSON removes insignificant whitespace from a JSON document so two
// documents can be compared structurally. Used in tests where the "want" value
// is written readably across multiple lines.
func compactJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return s
	}
	return buf.String()
}

// equalJSON compares two interface{} values decoded from JSON for structural
// equality. Object keys are compared regardless of order (Go map iteration is
// randomized, so order-insensitive comparison is required for the MiniMax test
// where the sanitized payload's key order is unspecified).
func equalJSON(a, b interface{}) bool {
	return reflect.DeepEqual(normalizeForCompare(a), normalizeForCompare(b))
}

// normalizeForCompare re-sorts object keys so map comparison is order-
// insensitive. Numbers are kept as float64 (already normalized by jsonDecode).
func normalizeForCompare(v interface{}) interface{} {
	switch x := v.(type) {
	case Obj:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([][2]interface{}, 0, len(keys))
		for _, k := range keys {
			out = append(out, [2]interface{}{k, normalizeForCompare(x[k])})
		}
		return out
	case Arr:
		out := make([]interface{}, len(x))
		for i, item := range x {
			out[i] = normalizeForCompare(item)
		}
		return out
	default:
		return v
	}
}
