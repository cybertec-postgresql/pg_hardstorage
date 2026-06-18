// jsonshim.go — jsonRoundTrip: marshal-via-JSON to flatten typed structs into map[string]any for YAML.
package yaml

import (
	stdjson "encoding/json"
)

// jsonRoundTrip marshals v with the standard JSON encoder (so the
// `json:"..."` tags drive field names and omitempty), then
// unmarshals into a plain map[string]any tree the YAML encoder
// can render with the same names.
func jsonRoundTrip(v any) (any, error) {
	b, err := stdjson.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := stdjson.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
