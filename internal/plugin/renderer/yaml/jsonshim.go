// jsonshim.go — jsonRoundTrip: marshal-via-JSON to flatten typed structs into map[string]any for YAML.
package yaml

import (
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/jsonshape"
)

// jsonRoundTrip marshals v with the standard JSON encoder (so the
// `json:"..."` tags drive field names and omitempty), then decodes
// into a plain tree. Delegates to the shared jsonshape helper, which
// preserves integral numbers as int64 — a plain Unmarshal-into-any
// made every number float64 and yaml.v3 rendered byte counts as
// scientific notation (`3.31480761e+08`).
func jsonRoundTrip(v any) (any, error) {
	return jsonshape.RoundTrip(v)
}
