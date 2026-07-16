// Package jsonshape normalises a typed result body into the plain
// (map[string]any | []any | scalar) tree renderers walk — via the
// standard JSON encoder, so `json:"..."` tags drive field names and
// omitempty exactly like the json renderer's on-the-wire output.
//
// The one subtlety this package exists for: a naive
// `json.Unmarshal(b, &out)` into `any` decodes EVERY JSON number as
// float64, and float64 stringification/encoding renders large values
// in scientific notation — byte counts became `3.31480761e+08` in
// yaml/csv/tap/junit/pdf/template output, changing the value's type
// vs the committed v1 JSON schema and losing precision past 2^53.
// RoundTrip decodes with json.Number and normalises integral numbers
// to int64 (fractional to float64), so every renderer prints plain
// digits.
package jsonshape

import (
	"bytes"
	"encoding/json"
)

// RoundTrip marshals v via encoding/json and decodes it back into a
// plain tree with integer-preserving numbers.
func RoundTrip(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return normalize(out), nil
}

// normalize walks the decoded tree converting json.Number to int64
// (integral) or float64 (fractional), in place where possible.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			x[k] = normalize(vv)
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalize(x[i])
		}
		return x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	}
	return v
}
