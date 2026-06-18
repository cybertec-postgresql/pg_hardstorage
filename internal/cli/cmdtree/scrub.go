// scrub.go — extracts pg_hardstorage commands from assistant replies and validates each against the tree.
package cmdtree

import (
	"strings"
)

// ScrubFinding is one command extracted from an assistant
// reply, paired with the validation outcome.  The chat
// REPL renders findings whose Error is non-nil as a
// warning block after the assistant's text so the
// operator sees the model's invented command + the real
// shape side by side.
type ScrubFinding struct {
	// Command is the bare command string (no backticks).
	Command string
	// Error is the validation outcome.  nil means the
	// command parses cleanly; non-nil findings are the
	// ones the renderer surfaces.
	Error *ValidationError
}

// Scrub walks an assistant's text response, extracts every
// pg_hardstorage command embedded in backticks or fenced
// code blocks, and validates each one against the cobra
// tree.  This is Layer 4 of the LLM-grounding stack: the
// model can write a command in plain prose without
// calling `suggest_command` (the tool-call gate doesn't
// fire), and Scrub is what catches that case.
//
// Recognised forms:
//
//   - Single-backtick: “pg_hardstorage deployment add ...“
//   - Triple-backtick code blocks where any line starts
//     with `binaryName` (after optional leading `$ `, `> `,
//     or whitespace).
//
// Lines that don't start with binaryName are ignored —
// the model legitimately quotes other shell snippets
// (psql, ollama, docker run) and we don't want to flag
// those.
//
// Findings are returned in source order.  When no
// commands are found the slice is nil.
func Scrub(root *Node, text, binaryName string) []ScrubFinding {
	if root == nil || text == "" {
		return nil
	}
	var out []ScrubFinding
	for _, cmd := range extractCommands(text, binaryName) {
		err := Validate(root, cmd, binaryName)
		ve, _ := err.(*ValidationError)
		out = append(out, ScrubFinding{Command: cmd, Error: ve})
	}
	return out
}

// extractCommands pulls every plausible binary command out
// of text.  The shapes we care about all rely on
// backticks; bare-text mentions of `pg_hardstorage` in
// flowing prose are not scrubbed (too many false
// positives from descriptive sentences like "use
// pg_hardstorage to take a backup").
func extractCommands(text, binaryName string) []string {
	var out []string
	seen := map[string]bool{}
	addUnique := func(cmd string) {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" || seen[cmd] {
			return
		}
		seen[cmd] = true
		out = append(out, cmd)
	}

	// Pass 1: triple-backtick fenced code blocks.  We
	// split on the fence sequence and look inside the
	// odd-indexed segments.  A ``` fence with a language
	// hint (```bash, ```sh, ```console) is fine — the
	// language token sits on the opening line which we
	// skip past via the first newline.
	parts := strings.Split(text, "```")
	for i := 1; i < len(parts); i += 2 {
		block := parts[i]
		// Drop any language hint on the first line.
		if nl := strings.IndexByte(block, '\n'); nl >= 0 {
			block = block[nl+1:]
		}
		blockLines := strings.Split(block, "\n")
		for j := 0; j < len(blockLines); j++ {
			line := stripPromptPrefix(blockLines[j])
			// A '#'-led line is a COMMENT, not a command.  Skip it
			// rather than feed "# pg_hardstorage handles ..." to the
			// validator, which would surface a bogus "unknown
			// subcommand" warning and train operators to ignore us.
			if strings.HasPrefix(line, "#") {
				continue
			}
			cmd := matchBinary(line, binaryName)
			if cmd == "" {
				continue
			}
			// Join trailing-backslash continuations: a model often
			// wraps a long command across lines with `\`.  Validate
			// the whole command, not just its first physical line —
			// otherwise flags on the continuation lines (`--from`,
			// `--repo`, ...) are wrongly reported as missing.
			for strings.HasSuffix(cmd, "\\") && j+1 < len(blockLines) {
				cmd = strings.TrimSuffix(cmd, "\\")
				j++
				cmd += " " + strings.TrimSpace(blockLines[j])
			}
			addUnique(strings.TrimSpace(cmd))
		}
	}

	// Pass 2: single-backtick spans.  We honour them only
	// when the prose is OUTSIDE a fenced block (otherwise
	// we'd double-count).  Walking outside-only requires
	// re-stitching the even-indexed segments from Pass 1.
	var prose strings.Builder
	for i := 0; i < len(parts); i += 2 {
		prose.WriteString(parts[i])
	}
	for _, span := range backtickSpans(prose.String()) {
		span = stripPromptPrefix(span)
		if cmd := matchBinary(span, binaryName); cmd != "" {
			addUnique(cmd)
		}
	}
	return out
}

// stripPromptPrefix removes a common shell-prompt prefix
// (`$ `, `> `, leading whitespace, leading "sudo ") so a
// model that quoted an example as `$ pg_hardstorage ...`
// still gets the binary recognised.  A leading `# ` is NOT
// stripped: that marks a comment line, and un-commenting it
// would turn documentation ("# pg_hardstorage handles ...")
// into a pseudo-command.  Comment lines are skipped by the
// caller instead.
func stripPromptPrefix(s string) string {
	s = strings.TrimSpace(s)
	for {
		switch {
		case strings.HasPrefix(s, "$ "):
			s = s[2:]
		case strings.HasPrefix(s, "> "):
			s = s[2:]
		case strings.HasPrefix(s, "sudo "):
			s = s[5:]
		default:
			return strings.TrimSpace(s)
		}
	}
}

// matchBinary returns the line if it starts with the
// binary name (or `./binary` / `/path/to/binary`),
// otherwise the empty string.
func matchBinary(line, binaryName string) string {
	first := firstToken(line)
	if first == "" {
		return ""
	}
	base := first
	if i := strings.LastIndexAny(first, "/\\"); i >= 0 {
		base = first[i+1:]
	}
	if base == binaryName {
		return line
	}
	return ""
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i]
		}
	}
	return s
}

// backtickSpans returns the contents of every
// `single-backtick` span in s.  We deliberately skip
// triple-backtick fences (those are handled by Pass 1
// upstream); the easy way to do that is to require that
// the matched span is surrounded by exactly one backtick
// on each side, not three.  Walking byte-by-byte makes
// the rule trivial.
func backtickSpans(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		if s[i] != '`' {
			i++
			continue
		}
		// Skip triple-backtick fences entirely.
		if i+2 < len(s) && s[i+1] == '`' && s[i+2] == '`' {
			// Find the closing fence.
			end := strings.Index(s[i+3:], "```")
			if end < 0 {
				return out // unterminated fence; bail
			}
			i += 3 + end + 3
			continue
		}
		// Single backtick — find the next single backtick
		// that isn't part of a triple.
		j := i + 1
		for j < len(s) {
			if s[j] == '`' {
				break
			}
			j++
		}
		if j >= len(s) {
			return out // unterminated single-backtick; bail
		}
		out = append(out, s[i+1:j])
		i = j + 1
	}
	return out
}
