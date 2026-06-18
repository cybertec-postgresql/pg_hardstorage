// Package inject is the testkit's fault-injection vocabulary.
//
// Each named primitive in the operator-facing faults.yaml
// (`disk_full`, `signal`, `cgroup_squeeze`, `toxiproxy`, `sql`,
// `patroni_switchover`, `libfaketime`, `network_block`,
// `flip_random_byte`, `pause_archive`) is implemented here as a
// concrete Fault.  The soak driver picks one per the YAML's
// weights, parses its action string, locates the targets it
// names, applies the fault, and after a heal window calls the
// returned Recovery to revert.
//
// Faults take their inputs through a structured Args map.  The
// parser converts action strings — `disk_full(target=repo,
// fill=98%)` — into the (Prefix, Args) tuple every primitive
// expects.  Operators editing faults.yaml never write Go code;
// the dispatcher's responsibility is to own that translation.
//
// The package depends only on context, exec, fs, and the small
// Target interface.  Docker awareness lives in target_docker.go;
// tests inject a fake target with no external dependencies.
package inject

import (
	"errors"
	"fmt"
	"strings"
)

// ParsedAction is the structured form of an action string.
type ParsedAction struct {
	Prefix string // "disk_full", "signal", ...
	Args   Args
}

// Args is the parsed key=value map.
type Args map[string]string

// String returns a stable, single-line, human-readable form
// suitable for log lines and audit events.  Keys are emitted
// in sorted order so identical Args produce identical output
// regardless of insertion order.
func (a Args) String() string {
	if len(a) == 0 {
		return ""
	}
	keys := sortedKeys(a)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(a[k])
	}
	return sb.String()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Tiny quadratic sort — keeps the package free of stdlib
	// "sort" import for nothing; n ≤ ~6 in practice.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Get reads a single key, returning ("", false) when absent.
func (a Args) Get(key string) (string, bool) {
	v, ok := a[key]
	return v, ok
}

// Require returns the value for key or an error mentioning the
// key.  Used by primitives that need a particular arg present.
func (a Args) Require(key string) (string, error) {
	v, ok := a[key]
	if !ok {
		return "", fmt.Errorf("missing required arg %q", key)
	}
	return v, nil
}

// ParseAction converts an action string into structured form.
//
// Grammar (intentionally simple):
//
//	action     := PREFIX "(" arglist? ")"
//	arglist    := arg ("," arg)*
//	arg        := KEY "=" VALUE
//	             | KEY                  (boolean flag, value="true")
//	VALUE      := unquoted | "..." | '...'
//
// The parser is forgiving on whitespace around commas and
// equals.  Quoted values pass through with their quotes
// stripped — handy for SQL strings:
//
//	sql("SELECT pg_drop_replication_slot('foo')")
//
// becomes Prefix="sql", Args={"_positional": "SELECT pg_drop_replication_slot('foo')"}.
// Bare-positional content (no key=value form) is stored under
// the synthetic key "_positional".
func ParseAction(action string) (ParsedAction, error) {
	action = strings.TrimSpace(action)
	if action == "" {
		return ParsedAction{}, errors.New("inject: empty action string")
	}
	open := strings.IndexByte(action, '(')
	if open < 0 {
		return ParsedAction{}, fmt.Errorf("inject: action %q: missing '('", action)
	}
	prefix := strings.TrimSpace(action[:open])
	if prefix == "" {
		return ParsedAction{}, fmt.Errorf("inject: action %q: empty prefix", action)
	}
	if !strings.HasSuffix(action, ")") {
		return ParsedAction{}, fmt.Errorf("inject: action %q: missing trailing ')'", action)
	}
	body := strings.TrimSpace(action[open+1 : len(action)-1])

	args := Args{}
	if body == "" {
		return ParsedAction{Prefix: prefix, Args: args}, nil
	}

	// Split on commas, but respect single / double quoted
	// substrings so `sql("SELECT a, b")` doesn't get split.
	pieces, err := splitTopLevelCommas(body)
	if err != nil {
		return ParsedAction{}, fmt.Errorf("inject: action %q: %w", action, err)
	}
	for _, raw := range pieces {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		eq := indexUnquoted(raw, '=')
		if eq < 0 {
			// Bare positional: stash under "_positional".
			// First positional wins; later positional bare
			// args raise an error to avoid silent drops.
			if _, exists := args["_positional"]; exists {
				return ParsedAction{}, fmt.Errorf(
					"inject: action %q: multiple bare-positional args (use key=value form)", action)
			}
			args["_positional"] = unquote(strings.TrimSpace(raw))
			continue
		}
		key := strings.TrimSpace(raw[:eq])
		val := unquote(strings.TrimSpace(raw[eq+1:]))
		if key == "" {
			return ParsedAction{}, fmt.Errorf(
				"inject: action %q: empty arg key in %q", action, raw)
		}
		if _, dup := args[key]; dup {
			return ParsedAction{}, fmt.Errorf(
				"inject: action %q: duplicate arg %q", action, key)
		}
		args[key] = val
	}
	return ParsedAction{Prefix: prefix, Args: args}, nil
}

// splitTopLevelCommas splits on commas that are NOT inside
// quoted substrings.  Returns an error if quotes are unbalanced.
func splitTopLevelCommas(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	var inQuote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inQuote == 0 && (ch == '\'' || ch == '"') {
			inQuote = ch
			cur.WriteByte(ch)
			continue
		}
		if inQuote != 0 {
			cur.WriteByte(ch)
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		if ch == ',' {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
	}
	if inQuote != 0 {
		return nil, fmt.Errorf("unbalanced %c quote", inQuote)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}

// indexUnquoted returns the index of the first occurrence of c
// outside any quoted substring, or -1.
func indexUnquoted(s string, c byte) int {
	var inQuote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inQuote == 0 && (ch == '\'' || ch == '"') {
			inQuote = ch
			continue
		}
		if inQuote != 0 {
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		if ch == c {
			return i
		}
	}
	return -1
}

// unquote strips a single layer of matching " or ' from s.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
