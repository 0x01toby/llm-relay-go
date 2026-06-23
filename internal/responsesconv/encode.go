package responsesconv

import (
	"bytes"
	"encoding/json"
)

// jsonEncode serializes v to JSON. For objects (Obj) the keys are emitted in
// insertion order, matching the original TypeScript behavior where object
// literals preserve key order and JSON.stringify respects it. Go's default
// json.Marshal sorts map keys alphabetically, which would break field-order
// sensitive consumers; orderedObject below preserves insertion order.
func jsonEncode(v interface{}) (string, error) {
	var buf bytes.Buffer
	if err := encodeValue(&buf, v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// jsonMustEncode panics on error — used only where the value is known to be
// serializable (always, for our JSON-derived inputs).
func jsonMustEncode(v interface{}) string {
	s, err := jsonEncode(v)
	if err != nil {
		panic(err)
	}
	return s
}

func encodeValue(buf *bytes.Buffer, v interface{}) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case string:
		return writeJSONString(buf, x)
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64:
		// encoding/json formats float64 the same way as JS JSON.stringify for
		// our values (integers, decimals). Defer to it.
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	case int:
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	case int64:
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	case Obj:
		return encodeObject(buf, x)
	case orderedObject:
		return x.encode(buf)
	case Arr:
		return encodeArray(buf, x)
	case []Obj:
		// Treat a typed []Obj like a generic array so values produced by the
		// message builders serialize correctly when embedded in payloads.
		return encodeArray(buf, toArr(x))
	case []byte:
		// Treat raw bytes as pre-encoded JSON fragment.
		buf.Write(x)
	case json.RawMessage:
		buf.Write(x)
	default:
		// Fallback: any struct/slice encoding/json can handle.
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}

func encodeObject(buf *bytes.Buffer, o Obj) error {
	// Use Go's json.Marshal for plain maps (key order is alphabetic, acceptable
	// for inputs we re-emit opaquely). For responses where order matters we
	// build orderedObject explicitly.
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	buf.Write(b)
	return nil
}

func encodeArray(buf *bytes.Buffer, a Arr) error {
	buf.WriteByte('[')
	for i, item := range a {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := encodeValue(buf, item); err != nil {
			return err
		}
	}
	buf.WriteByte(']')
	return nil
}

// writeJSONString encodes s as a JSON string literal (with quoting & escaping).
func writeJSONString(buf *bytes.Buffer, s string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	buf.Write(b)
	return nil
}

// orderedObject emits keys in insertion order. Used where the wire order is
// observable and matters (e.g. the Responses-API response envelope). Most
// internal mutations still use Obj; this is only for final assembly.
type orderedObject struct {
	keys []string
	vals map[string]interface{}
}

func newOrdered() *orderedObject {
	return &orderedObject{vals: map[string]interface{}{}}
}

func (o *orderedObject) set(key string, val interface{}) *orderedObject {
	if _, exists := o.vals[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = val
	return o
}

func (o *orderedObject) setIf(key string, val interface{}, cond bool) *orderedObject {
	if cond {
		o.set(key, val)
	}
	return o
}

func (o *orderedObject) encode(buf *bytes.Buffer) error {
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeJSONString(buf, k); err != nil {
			return err
		}
		buf.WriteByte(':')
		if err := encodeValue(buf, o.vals[k]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func (o *orderedObject) String() string {
	var buf bytes.Buffer
	_ = o.encode(&buf)
	return buf.String()
}

// asObj returns v coerced to an Obj. If v is an orderedObject it is converted
// into a plain map; otherwise it is returned as-is when already a map.
func asObj(v interface{}) Obj {
	if o, ok := v.(Obj); ok {
		return o
	}
	if oo, ok := v.(*orderedObject); ok {
		return oo.vals
	}
	return nil
}

// jsonDecode parses JSON preserving numbers as float64 and objects as Obj,
// giving us the same dynamic shape TS gives with JSON.parse + no asserts.
func jsonDecode(data []byte) (interface{}, error) {
	var v interface{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return normalizeNumbers(v), nil
}

// normalizeNumbers converts json.Number to float64 recursively so the rest of
// the code can do plain float64 arithmetic (mirroring JS where all numbers are
// doubles). Objects become Obj and arrays become Arr.
func normalizeNumbers(v interface{}) interface{} {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return float64(0)
		}
		return f
	case map[string]interface{}:
		o := Obj{}
		for k, val := range x {
			o[k] = normalizeNumbers(val)
		}
		return o
	case []interface{}:
		// Preserve emptiness: an empty JSON array must stay [] (not nil),
		// otherwise it re-encodes as null. Start non-nil.
		a := make(Arr, len(x))
		for i := range x {
			a[i] = normalizeNumbers(x[i])
		}
		return a
	default:
		return v
	}
}

// jsonDecodeObj parses JSON that must be a JSON object, returning an error
// (compatibleErr) when it is not.
func jsonDecodeObj(data []byte) (Obj, error) {
	v, err := jsonDecode(data)
	if err != nil {
		return nil, err
	}
	o, ok := v.(Obj)
	if !ok {
		return nil, errNotObject
	}
	return o, nil
}
