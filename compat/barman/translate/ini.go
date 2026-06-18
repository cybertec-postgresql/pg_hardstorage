// ini.go — Barman INI parser: sections, key=value pairs, multi-line continuations, comment-aware.
package translate

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// iniSection is one [section] block from a Barman INI file.  Order
// of insertion is preserved so the YAML emitter writes settings in
// the same order the operator wrote them — diff-friendly for
// configuration-management workflows.
type iniSection struct {
	name  string
	keys  []string
	pairs map[string]string
}

// iniDoc is the full parsed file: zero or more sections, in order.
type iniDoc struct {
	sections []*iniSection
}

// parseINI reads Barman's INI dialect.  Quirks we honour:
//
//   - Comments start with ';' or '#' at the line's first non-space.
//   - Inline comments are NOT supported (Barman's parser doesn't).
//   - Section headers: [name].  Whitespace inside the brackets is
//     trimmed.
//   - key = value, key: value, key=value, key:value all accepted.
//   - Multi-line continuations (leading whitespace) are joined to
//     the previous key with a single space.
//
// Returns the parsed doc, never nil; sections may be empty.
func parseINI(r io.Reader) (*iniDoc, error) {
	doc := &iniDoc{}
	var current *iniSection
	var lastKey string

	sc := bufio.NewScanner(r)
	// Barman configs sometimes carry long retention-policy lines.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)

		// Blank.
		if trimmed == "" {
			lastKey = ""
			continue
		}
		// Comment.
		if trimmed[0] == ';' || trimmed[0] == '#' {
			lastKey = ""
			continue
		}
		// Section header.
		if trimmed[0] == '[' {
			if !strings.HasSuffix(trimmed, "]") {
				return nil, fmt.Errorf("ini: line %d: unterminated section header %q", lineNo, trimmed)
			}
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if name == "" {
				return nil, fmt.Errorf("ini: line %d: empty section name", lineNo)
			}
			current = &iniSection{name: name, pairs: map[string]string{}}
			doc.sections = append(doc.sections, current)
			lastKey = ""
			continue
		}
		// Continuation (leading whitespace + we have a previous key).
		if (line[0] == ' ' || line[0] == '\t') && lastKey != "" && current != nil {
			current.pairs[lastKey] = current.pairs[lastKey] + " " + trimmed
			continue
		}
		// key = value / key : value.
		key, value, ok := splitKV(trimmed)
		if !ok {
			return nil, fmt.Errorf("ini: line %d: not a key=value pair: %q", lineNo, trimmed)
		}
		if current == nil {
			// Lines before any [section] go in an anonymous root.
			// Barman doesn't normally emit those; treat as global.
			current = &iniSection{name: "barman", pairs: map[string]string{}}
			doc.sections = append(doc.sections, current)
		}
		key = strings.ToLower(key)
		if _, dup := current.pairs[key]; !dup {
			current.keys = append(current.keys, key)
		}
		current.pairs[key] = value
		lastKey = key
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ini: scanner: %w", err)
	}
	return doc, nil
}

// splitKV splits "k = v" or "k: v" into key + value, trimming
// whitespace from both sides.
func splitKV(s string) (string, string, bool) {
	for i, ch := range s {
		if ch == '=' || ch == ':' {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
		}
	}
	return "", "", false
}
