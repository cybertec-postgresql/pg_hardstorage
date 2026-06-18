// Package configcheck validates YAML config snippets embedded in LLM answers
// against the REAL pg_hardstorage config schema (internal/config.Config).
//
// It is the config-side counterpart to internal/cli/cmdtree, which validates
// CLI commands. The command-validator never sees config YAML, so a model that
// tells an operator to add a hand-edited key to pg_hardstorage.yaml — e.g. the
// invented flat `backup_schedule:` instead of the real nested
// `schedule.backup.every` — slipped through. This package walks the parsed
// YAML against the config struct (by reflection on its `yaml:` tags) and
// reports keys that don't exist, with a did-you-mean.
//
// It is deliberately CONSERVATIVE to avoid the false positives that erode
// operator trust: it only inspects a block that is recognisably a full
// pg_hardstorage config (a top-level `deployments:` map, or a
// `schema: pg_hardstorage.config…` marker). Bare fragments shown for
// illustration are skipped. Map keys (deployment names, sink names) are
// wildcards and never flagged; only STRUCT field names are checked.
package configcheck

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// Finding kinds.
const (
	KindUnknownKey = "unknown_key" // a key with no matching schema field
	KindType       = "type"        // a value whose shape contradicts the field type
	KindOneOf      = "one_of"      // mutually-exclusive keys both set
	KindEnum       = "enum"        // a string value outside the allowed set
)

// Finding is one config problem found in an answer.
type Finding struct {
	// Kind is one of the Kind* constants above.
	Kind string
	// Path is the dotted location of the PARENT map/struct, with `*` for a
	// map level (e.g. "deployments.*.schedule"). Empty means the config root.
	Path string
	// Key is the offending key (for unknown_key / type / enum). Empty for
	// one_of, which is a constraint on the whole map.
	Key string
	// Suggestion is the closest valid sibling field for an unknown_key.
	Suggestion string
	// Message is a human explanation for type / one_of / enum findings.
	Message string
}

// oneOfByType: structs where AT MOST ONE of the listed yaml keys may be set.
// Keyed by the struct type so it tracks the schema rather than a path string.
var oneOfByType = map[reflect.Type][]string{
	reflect.TypeOf(config.ScheduleSpec{}): {"every", "daily_at", "at"},
}

// enumByField: (struct type, yaml key) → allowed lowercase string values.
// Kept tiny and tied to STABLE, documented sets so it doesn't false-positive
// as the schema grows. retention.policy's set is the three retention.Policy
// implementations (gfs/simple/count); see internal/backup/retention.
var enumByField = map[enumKey][]string{
	{reflect.TypeOf(config.RetentionConfig{}), "policy"}: {"gfs", "simple", "count"},
}

type enumKey struct {
	t   reflect.Type
	key string
}

// Scrub extracts every fenced YAML block from text, and for those that look
// like a pg_hardstorage config, reports keys absent from the schema.
func Scrub(text string) []Finding {
	if text == "" {
		return nil
	}
	cfgType := reflect.TypeOf(config.Config{})
	var out []Finding
	for _, block := range yamlBlocks(text) {
		var m map[string]interface{}
		if err := yaml.Unmarshal([]byte(block), &m); err != nil || m == nil {
			continue
		}
		if !looksLikeConfig(m) {
			continue
		}
		walkMap(m, cfgType, "", &out)
	}
	return dedupe(out)
}

// yamlBlocks returns the contents of every triple-backtick fenced block,
// dropping the opening ```lang line. (Inline single-backtick spans are not
// config blocks, so we don't look at them.)
func yamlBlocks(text string) []string {
	parts := strings.Split(text, "```")
	var out []string
	for i := 1; i < len(parts); i += 2 {
		b := parts[i]
		if nl := strings.IndexByte(b, '\n'); nl >= 0 {
			b = b[nl+1:]
		}
		out = append(out, b)
	}
	return out
}

// looksLikeConfig reports whether a parsed YAML map is plausibly a
// pg_hardstorage config (vs an unrelated YAML fragment shown for context).
func looksLikeConfig(m map[string]interface{}) bool {
	if _, ok := m["deployments"]; ok {
		return true
	}
	if s, ok := m["schema"].(string); ok && strings.HasPrefix(s, "pg_hardstorage.config") {
		return true
	}
	return false
}

// walkMap checks every key in m against the yaml fields of struct type t,
// recording unknown keys, value-shape mismatches and enum violations, then
// recursing into known sub-structures. After the per-key pass it enforces any
// at-most-one (one-of) constraint on the struct as a whole.
func walkMap(m map[string]interface{}, t reflect.Type, path string, out *[]Finding) {
	t = deref(t)
	if t.Kind() != reflect.Struct {
		return
	}
	fields := yamlFields(t)
	// Stable iteration so findings are deterministic.
	for _, key := range sortedKeys(m) {
		val := m[key]
		f, ok := fields[key]
		if !ok {
			*out = append(*out, Finding{Kind: KindUnknownKey, Path: path, Key: key, Suggestion: suggest(key, fieldNames(fields))})
			continue
		}
		// Value-SHAPE check: a value whose YAML shape (scalar / block / list)
		// contradicts the field's declared type. Don't recurse a mismatched
		// value — its inner keys would be validated against the wrong type.
		if msg := shapeMismatch(val, f.Type); msg != "" {
			*out = append(*out, Finding{Kind: KindType, Path: path, Key: key, Message: msg})
			continue
		}
		// Enum check: a string value outside a known allowed set.
		if allowed, ok := enumByField[enumKey{t, key}]; ok {
			if s, isStr := val.(string); isStr && !containsFold(allowed, s) {
				*out = append(*out, Finding{Kind: KindEnum, Path: path, Key: key,
					Message: fmt.Sprintf("%q is not a valid %s — use one of %s", s, key, strings.Join(allowed, " | "))})
				continue
			}
		}
		walkValue(val, f.Type, join(path, key), out)
	}
	// One-of: at most one of a mutually-exclusive key group may be present.
	if group, ok := oneOfByType[t]; ok {
		var present []string
		for _, k := range group {
			if _, has := m[k]; has {
				present = append(present, k)
			}
		}
		if len(present) > 1 {
			loc := path
			if loc == "" {
				loc = "(config root)"
			}
			*out = append(*out, Finding{Kind: KindOneOf, Path: path,
				Message: fmt.Sprintf("set at most one of %s — found %s together at %s",
					strings.Join(group, " / "), strings.Join(present, " + "), loc)})
		}
	}
}

// shapeMismatch returns a message when val's YAML shape contradicts the
// field's declared kind, or "" when the shape is acceptable. A nil/empty value
// is always acceptable (an empty block, an unset key).
func shapeMismatch(val interface{}, ft reflect.Type) string {
	if val == nil {
		return ""
	}
	ft = deref(ft)
	_, isMap := val.(map[string]interface{})
	_, isSeq := val.([]interface{})
	isScalar := !isMap && !isSeq
	switch ft.Kind() {
	case reflect.Struct, reflect.Map:
		if isScalar {
			return "expected a nested block, got a single value"
		}
		if isSeq {
			return "expected a nested block, got a list"
		}
	case reflect.Slice:
		if !isSeq {
			return "expected a list"
		}
	default: // scalar field (string / int / bool / duration / ...)
		if isMap {
			return "expected a single value, got a nested block"
		}
		if isSeq {
			return "expected a single value, got a list"
		}
		// A numeric field given a non-numeric string is a real mistake
		// (`keep_daily: "six"`); a numeric-looking string still coerces.
		if k := ft.Kind(); k >= reflect.Int && k <= reflect.Float64 {
			if s, ok := val.(string); ok {
				if _, err := strconv.ParseFloat(s, 64); err != nil {
					return "expected a number"
				}
			}
		}
	}
	return ""
}

func sortedKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsFold(set []string, s string) bool {
	for _, v := range set {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// walkValue recurses into a value according to its SCHEMA type. A struct
// expects a map (recurse its fields); a map (deployments, etc.) treats every
// key as a wildcard and recurses the element type; a slice recurses each
// element. Scalars and `interface{}`/free-form maps are leaves — anything
// goes, so we never false-positive on them.
func walkValue(val interface{}, t reflect.Type, path string, out *[]Finding) {
	t = deref(t)
	switch t.Kind() {
	case reflect.Struct:
		if sub, ok := val.(map[string]interface{}); ok {
			walkMap(sub, t, path, out)
		}
	case reflect.Map:
		// A map[string]T with a STRUCT element is schema-checked under a `*`
		// wildcard. A map with a scalar/interface element (free-form) is a
		// leaf — its keys are operator data, not schema fields.
		if deref(t.Elem()).Kind() != reflect.Struct {
			return
		}
		if sub, ok := val.(map[string]interface{}); ok {
			for _, v := range sub {
				walkValue(v, t.Elem(), path+".*", out)
			}
		}
	case reflect.Slice:
		if deref(t.Elem()).Kind() != reflect.Struct {
			return
		}
		if seq, ok := val.([]interface{}); ok {
			for _, e := range seq {
				walkValue(e, t.Elem(), path+"[]", out)
			}
		}
	}
}

// yamlFields maps a struct's yaml key names to their fields.
func yamlFields(t reflect.Type) map[string]reflect.StructField {
	out := make(map[string]reflect.StructField, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			continue
		}
		out[name] = f
	}
	return out
}

func fieldNames(fields map[string]reflect.StructField) []string {
	out := make([]string, 0, len(fields))
	for n := range fields {
		out = append(out, n)
	}
	return out
}

// suggest returns the closest valid field name: nearest by edit distance
// (≤3), else a field that is a substring of the bad key (or vice versa) —
// which catches "schedule" for the invented "backup_schedule".
func suggest(key string, names []string) string {
	best, bestD := "", 4
	for _, n := range names {
		if d := lev(key, n); d < bestD {
			best, bestD = n, d
		}
	}
	if best != "" {
		return best
	}
	for _, n := range names {
		if len(n) >= 4 && (strings.Contains(key, n) || strings.Contains(n, key)) {
			return n
		}
	}
	return ""
}

func join(path, seg string) string {
	if path == "" {
		return seg
	}
	return path + "." + seg
}

func deref(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

func dedupe(in []Finding) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for _, f := range in {
		k := f.Kind + "\x00" + f.Path + "\x00" + f.Key + "\x00" + f.Message
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	return out
}

// lev is the standard Wagner-Fischer edit distance, bounded inputs.
func lev(a, b string) int {
	if a == b {
		return 0
	}
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
