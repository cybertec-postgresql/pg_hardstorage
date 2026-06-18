// validate.go — ValidationError + Validate: structurally check a command string against the cobra tree.
package cmdtree

import (
	"fmt"
	"strings"
)

// ValidationError is the structured outcome of Validate.
// We don't fold this into output.NewError because cmdtree
// must stay free of CLI-output-package dependencies (it's
// imported from internal/llm/tools where the output
// package would create a layering hairball).  The LLM-tool
// adapter wraps this in an llm.* error code.
type ValidationError struct {
	// Kind is a short tag the LLM tool can switch on:
	//   "binary"          — command did not start with pg_hardstorage
	//   "unknown_command" — a path segment did not resolve
	//   "unknown_flag"    — a --flag did not exist on the resolved leaf
	//   "missing_required" — a required --flag was not supplied
	//   "arg_count"       — wrong number of positional args for a runnable
	//                       command (e.g. a hallucinated subcommand)
	//   "parse"           — shell tokenisation failed (unbalanced quotes etc.)
	Kind string
	// Message is the operator-readable explanation.
	Message string
	// Suggestion is a did-you-mean hint when one is
	// available — the closest valid sibling for an
	// unknown subcommand, or the closest known flag for
	// an unknown one.  Empty when no clear guess exists.
	Suggestion string
	// PathBeforeError is the path segments that DID
	// resolve, useful for the LLM to decide where to
	// look up help.  E.g. for `deployment create` the
	// path before error is ["deployment"].
	PathBeforeError []string
}

// Error renders the validation failure as a single line, appending the
// did-you-mean suggestion when one is present.
func (e *ValidationError) Error() string {
	if e.Suggestion != "" {
		return fmt.Sprintf("%s: %s — did you mean %q?", e.Kind, e.Message, e.Suggestion)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// Validate checks that command would parse against the
// cobra tree, returning a typed error with a did-you-mean
// hint when something doesn't resolve.  The motivating
// case is the operator who typed
//
//	pg_hardstorage deployment create --name mydb1 --connection ...
//
// and got "unknown flag: --name".  Feeding that command
// here returns:
//
//	ValidationError{
//	  Kind: "unknown_command",
//	  Message: `unknown subcommand "create" under "deployment"`,
//	  Suggestion: "add",
//	  PathBeforeError: ["deployment"],
//	}
//
// Validate does NOT execute the command — it just walks
// the tree.  Behaviour-affecting flags like --apply, --yes,
// --force are not policed here (the safety / mutation
// gate handles those one layer up).
//
// binaryName is what the command is expected to start with
// — "pg_hardstorage" in production, but tests pass the
// test binary's argv[0] when validating live transcripts.
func Validate(root *Node, command, binaryName string) error {
	tokens, err := tokenise(command)
	if err != nil {
		return &ValidationError{
			Kind:    "parse",
			Message: fmt.Sprintf("could not tokenise command: %v", err),
		}
	}
	if len(tokens) == 0 {
		return &ValidationError{Kind: "parse", Message: "empty command"}
	}
	// First token is the binary.  Accept "pg_hardstorage",
	// "./pg_hardstorage", "/path/to/pg_hardstorage", and
	// the configured binaryName (the latter is for tests
	// that build the binary with a different name).
	bin := tokens[0]
	base := bin
	if i := strings.LastIndexAny(bin, "/\\"); i >= 0 {
		base = bin[i+1:]
	}
	if base != binaryName && base != "pg_hardstorage" {
		return &ValidationError{
			Kind:    "binary",
			Message: fmt.Sprintf("command should start with %q, got %q", binaryName, bin),
		}
	}
	tokens = tokens[1:]

	// A pg_hardstorage command in an answer is often backgrounded
	// (`... &`), piped (`... | jq`), chained (`... ; echo done`), or
	// redirected (`... > out.json`). We validate only the command ITSELF —
	// everything from the first shell control operator onward is a separate
	// command / redirect, not a positional argument. Without this, `&` and
	// the tokens after `|` / `;` inflated the positional count and produced a
	// spurious arg_count error on a perfectly valid command. (Safe: the
	// tokeniser keeps quoted values intact, so an `&` inside an S3 URL like
	// `?a=1&b=2` is one token, never a standalone operator.  Trailing inline
	// `#` comments are dropped earlier, by tokenise itself.)
	for k, tok := range tokens {
		if isShellOp(tok) {
			tokens = tokens[:k]
			break
		}
	}

	// Walk the tree until we either resolve a leaf, hit
	// the first --flag, or fail.  Subcommand path
	// segments come BEFORE flags in cobra (`pg_hs
	// deployment add --connection ...`), so the first
	// token starting with `-` ends path-walking.
	cur := root
	resolved := []string{}
	i := 0
	for i < len(tokens) {
		tok := tokens[i]
		if strings.HasPrefix(tok, "-") {
			break
		}
		next := cur.findChild(tok)
		if next == nil {
			// The token isn't a child.  It's a positional arg —
			// not an unknown subcommand — in either of two cases:
			//   1. `cur` is a leaf (no children at all); or
			//   2. `cur` is ITSELF runnable, so it accepts
			//      positional args even though it also hosts
			//      subcommands.  e.g. `backup <deployment>` is
			//      runnable AND parents `backup delete`, so
			//      `backup db1` is a valid positional, not a typo.
			// Stop walking and fall through to the flag loop (which
			// still runs the required-flag check — important so a
			// `backup db1` missing --pg-connection is caught as
			// missing_required rather than masked here).
			if len(cur.Children) == 0 || cur.Runnable {
				break
			}
			return &ValidationError{
				Kind: "unknown_command",
				Message: fmt.Sprintf("unknown subcommand %q under %q",
					tok, displayPath(resolved, root.Name)),
				Suggestion:      bestMatch(tok, childNames(cur)),
				PathBeforeError: append([]string(nil), resolved...),
			}
		}
		cur = next
		resolved = append(resolved, tok)
		i++
	}

	// `--help` / `-h` is valid on EVERY cobra command and short-circuits
	// execution: cobra prints the command's help and ignores required
	// flags, positional-arg counts, and any other flags entirely. The help
	// flag is added lazily by cobra (InitDefaultHelpFlag at execute time),
	// so it is NOT in the frozen cmdtree snapshot — without this guard,
	// `agent --help`, `verify --help`, even `pg_hardstorage --help`
	// (extremely common steps in LLM answers) were wrongly flagged as
	// "unknown flag --help" or, worse, "missing required --repo". Once the
	// command PATH has resolved (so a typo'd subcommand was already caught
	// above), a help request is always valid.
	for _, tok := range tokens[i:] {
		if tok == "--help" || tok == "-h" {
			return nil
		}
	}

	// Flag loop.  Each --flag is checked against cur's
	// merged flag set (local + inherited persistents).
	// Unknown flags get a did-you-mean from the leaf's
	// flag set.
	seenFlags := map[string]bool{}
	posCount := 0 // positional args consumed past the resolved command
	for i < len(tokens) {
		tok := tokens[i]
		if !strings.HasPrefix(tok, "-") {
			// Positional arg — count it and move on.
			posCount++
			i++
			continue
		}
		// Strip leading dashes and a possible "=value" suffix.
		raw := strings.TrimLeft(tok, "-")
		if eq := strings.Index(raw, "="); eq >= 0 {
			raw = raw[:eq]
		}
		if raw == "" {
			// Bare "--" terminator — everything after is
			// positional.  Count the rest and stop scanning.
			posCount += len(tokens) - i - 1
			break
		}
		if cur.FlagByName(raw) == nil {
			return &ValidationError{
				Kind: "unknown_flag",
				Message: fmt.Sprintf("unknown flag %q on %q",
					tok, displayPath(resolved, root.Name)),
				Suggestion:      bestFlagMatch(raw, cur.Flags),
				PathBeforeError: append([]string(nil), resolved...),
			}
		}
		seenFlags[raw] = true
		// Skip the flag's value when it's a non-bool
		// space-separated form: `--connection xyz`.
		// (We use the resolved Flag.Type to decide;
		// `--key=val` already had its value consumed.)
		if !strings.Contains(tok, "=") {
			f := cur.FlagByName(raw)
			if f != nil && f.Type != "bool" && i+1 < len(tokens) {
				i++ // skip the value
			}
		}
		i++
	}

	// Required-flag check.  Cobra marks flags as required via
	// `cobra_annotations[BashCompOneRequiredFlag]`; cmdtree.Walk
	// translates that into Flag.Required.  An LLM-suggested
	// command that drops a required flag would fail at runtime
	// with a confusing message; catching it here lets the
	// validator-retry loop ask for a correction.
	for _, f := range cur.Flags {
		if f.Required && !seenFlags[f.Name] {
			return &ValidationError{
				Kind: "missing_required",
				Message: fmt.Sprintf("required flag --%s not supplied on %q",
					f.Name, displayPath(resolved, root.Name)),
				PathBeforeError: append([]string(nil), resolved...),
			}
		}
	}

	// Positional-arity check. Enforced only on RUNNABLE commands — a pure
	// parent routes to subcommands and its Use placeholder is a verb list,
	// not real args, while an unknown subcommand was already caught above.
	// This catches a hallucinated subcommand on a runnable parent, e.g.
	// `backup full db1` resolves to runnable `backup` with two positionals
	// where its `<deployment>` allows one. MaxArgs == -1 means unbounded.
	if cur.Runnable {
		path := displayPath(resolved, root.Name)
		spec := usePlaceholders(cur.Use)
		minArgs := cur.MinArgs
		// Repo-family commands (repo audit/gc, compliance report, capacity
		// preflight) take the repo URL as EITHER a <url>/<repo> positional OR
		// the --repo/--url flag.  When the flag form is used, that positional
		// is already satisfied — don't also demand it as a positional.
		if minArgs > 0 && (seenFlags["repo"] || seenFlags["url"]) && useHasRepoPositional(cur.Use) {
			minArgs--
		}
		if posCount < minArgs {
			return &ValidationError{
				Kind: "arg_count",
				Message: fmt.Sprintf("%q needs %s, got %d%s",
					path, argCountPhrase(cur.MinArgs, cur.MaxArgs), posCount, spec),
				PathBeforeError: append([]string(nil), resolved...),
			}
		}
		if cur.MaxArgs >= 0 && posCount > cur.MaxArgs {
			return &ValidationError{
				Kind: "arg_count",
				Message: fmt.Sprintf("%q accepts %s, got %d%s",
					path, argCountPhrase(cur.MinArgs, cur.MaxArgs), posCount, spec),
				PathBeforeError: append([]string(nil), resolved...),
			}
		}
	}
	return nil
}

// usePlaceholders returns the parenthesised POSITIONAL placeholders from a
// Use line ("backup <deployment>" -> " (<deployment>)"), or "" when none.
// It shares usePositionals with parseArgSpec, so the hint shows exactly the
// args the count check enforces — optional ones wrapped in brackets, flag
// values ("--repo <url>") and flag groups ("[--name <id>]") excluded.
func usePlaceholders(use string) string {
	req, opt, _ := usePositionals(use)
	ph := append([]string(nil), req...)
	for _, o := range opt {
		// Already a bracketed group (e.g. a "[id...]" variadic) → as-is;
		// otherwise mark it optional for the reader.
		if strings.HasPrefix(o, "[") {
			ph = append(ph, o)
		} else {
			ph = append(ph, "["+o+"]")
		}
	}
	if len(ph) == 0 {
		return ""
	}
	return " (" + strings.Join(ph, " ") + ")"
}

// useHasRepoPositional reports whether a Use line's positional args include a
// <url> or <repo> placeholder — the marker for commands (repo audit, repo gc,
// compliance report, capacity preflight) that accept the repo EITHER as that
// positional OR via the --repo/--url flag.  When the flag form is supplied,
// the positional is satisfied and arity must not demand it too.
func useHasRepoPositional(use string) bool {
	req, opt, _ := usePositionals(use)
	for _, p := range append(append([]string(nil), req...), opt...) {
		if strings.Contains(p, "<url>") || strings.Contains(p, "<repo>") {
			return true
		}
	}
	return false
}

// argCountPhrase renders an arg-count bound for a message.
func argCountPhrase(minArgs, maxArgs int) string {
	switch {
	case maxArgs < 0:
		return "at least " + plural(minArgs)
	case minArgs == maxArgs:
		return plural(minArgs)
	default:
		return fmt.Sprintf("%d–%d positional arguments", minArgs, maxArgs)
	}
}

func plural(n int) string {
	if n == 1 {
		return "1 positional argument"
	}
	return fmt.Sprintf("%d positional arguments", n)
}

// isShellOp reports whether tok is a standalone shell control operator — the
// boundary between a pg_hardstorage command and a separate command, pipe, or
// redirect on the same line (`cmd & `, `cmd | jq`, `cmd ; echo`, `cmd > out`).
func isShellOp(tok string) bool {
	switch tok {
	case "&", ";", "|", "&&", "||", "|&":
		return true
	}
	// Redirections, INCLUDING file-descriptor duplication forms the tokeniser
	// renders as one word: `>`, `>>`, `2>`, `&>`, and crucially `2>&1`, `1>&2`,
	// `>&2`, `2>&-`. (The plain `2>` was already covered, but `2>&1` — a stderr
	// dup the model emits with `--verbose 2>&1 | grep …` — was not, so it got
	// counted as a positional and tripped a false arg_count.) A redirect token
	// contains `<` or `>` and is built ONLY from digits and the redirection
	// metacharacters, so it never collides with a real argument — URLs, paths
	// and `<placeholder>`s all contain other characters.
	if strings.ContainsAny(tok, "<>") {
		for _, r := range tok {
			if !strings.ContainsRune("0123456789<>&-", r) {
				return false
			}
		}
		return true
	}
	return false
}

func displayPath(segments []string, rootName string) string {
	if len(segments) == 0 {
		return rootName
	}
	return rootName + " " + strings.Join(segments, " ")
}

func childNames(n *Node) []string {
	out := make([]string, 0, len(n.Children))
	for _, c := range n.Children {
		out = append(out, c.Name)
		// Aliases are equally valid suggestion targets —
		// if an operator has been told the alias is the
		// canonical form, did-you-mean should surface it.
		out = append(out, c.Aliases...)
	}
	return out
}

// bestMatch returns the closest-by-edit-distance candidate
// to s, or "" when nothing is within distance ≤ 2.  Two
// is the sweet spot for typos: catches "creat" → "create"
// (dist 1), "creaate" → "create" (dist 1), "creat" →
// "add" (dist 4, dropped).  For our motivating case
// "create" → "add" returns "" (correct — they're
// semantically related but not similar enough to
// auto-suggest, and the catalog block above the
// suggestion shows the operator the right verb anyway).
func bestMatch(s string, candidates []string) string {
	best := ""
	bestDist := 3
	for _, c := range candidates {
		d := levenshtein(s, c)
		if d < bestDist {
			best = c
			bestDist = d
		}
	}
	return best
}

func bestFlagMatch(s string, flags []Flag) string {
	names := make([]string, 0, len(flags))
	for _, f := range flags {
		names = append(names, f.Name)
	}
	return bestMatch(s, names)
}

// levenshtein is the standard Wagner-Fischer DP.  Bounded
// inputs (flag names, command names, both ≤ ~30 chars) so
// the O(n*m) cost is trivial.
func levenshtein(a, b string) int {
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

// tokenise splits a command line, respecting single and
// double quotes and backslash-escapes.  It is more
// permissive than POSIX shlex (no command substitution,
// no $variable expansion) but exactly what we need for
// validating LLM-emitted command strings: literal
// arguments, possibly with a quoted value containing
// spaces (`--connection 'postgres://hs:pass with space@host'`).
func tokenise(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inSingle, inDouble, inWord := false, false, false
	parenDepth := 0 // >0 while inside a `$(...)` command substitution
	flush := func() {
		if inWord {
			out = append(out, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case parenDepth > 0:
			// Inside `$(...)`: an opaque VALUE to the OUTER command. We keep
			// it as one token so its internals — an inner `|`, an inner
			// command's `--flag`, its own positionals — are NOT parsed as the
			// outer command's. Without this, `verify db1 $(... list ... | jq
			// -r .id) --repo r` lost everything after the inner `|` to the
			// shell-op truncation and was wrongly flagged "missing --repo".
			// Nested parens are tracked by depth; a stray `)` inside a quote
			// is rare enough in LLM one-liners to ignore.
			cur.WriteByte(ch)
			if ch == '(' {
				parenDepth++
			} else if ch == ')' {
				parenDepth--
			}
			inWord = true
		case inSingle:
			if ch == '\'' {
				inSingle = false
				continue
			}
			cur.WriteByte(ch)
			inWord = true
		case inDouble:
			if ch == '"' {
				inDouble = false
				continue
			}
			if ch == '\\' && i+1 < len(s) {
				next := s[i+1]
				if next == '"' || next == '\\' || next == '$' || next == '`' {
					cur.WriteByte(next)
					i++
					inWord = true
					continue
				}
			}
			cur.WriteByte(ch)
			inWord = true
		default:
			switch ch {
			case ' ', '\t', '\n':
				flush()
			case '#':
				// An UNQUOTED '#' at a word boundary starts a shell comment —
				// everything to end of line is ignored (`... --apply  # note`).
				// We must drop it HERE, during tokenisation, not afterwards:
				// a comment can contain an apostrophe ("use only when you're
				// sure") that would otherwise be mistaken for an opening quote
				// and fail the whole command with "unbalanced quote". Mid-word
				// '#' (`a#b`) is a literal, matching shell semantics.
				if !inWord {
					flush()
					return out, nil
				}
				cur.WriteByte(ch)
				inWord = true
			case '$':
				// `$(` opens a command substitution — consume it as one
				// opaque token (see the parenDepth>0 branch above). A bare
				// `$` (e.g. `$VAR`) is just a literal character.
				cur.WriteByte(ch)
				inWord = true
				if i+1 < len(s) && s[i+1] == '(' {
					cur.WriteByte('(')
					i++
					parenDepth = 1
				}
			case '\'':
				inSingle = true
				inWord = true
			case '"':
				inDouble = true
				inWord = true
			case '\\':
				if i+1 < len(s) {
					cur.WriteByte(s[i+1])
					i++
					inWord = true
				}
			default:
				cur.WriteByte(ch)
				inWord = true
			}
		}
	}
	// An unterminated quote at EOF is, in LLM output, almost always an
	// EXTRACTION artifact: the model wrote a multi-line quoted argument (a
	// jq filter, an SQL snippet) and only its FIRST line — ending mid-quote
	// — was extracted, and that quoted tail sits after a shell `|` we discard
	// anyway. Failing the whole command with "unbalanced quote" buried the
	// real signal (e.g. a missing --repo on the `pg_hardstorage …` part
	// BEFORE the pipe) and could reject an otherwise-valid one-liner. So we
	// flush best-effort tokens and let validation proceed; the shell-operator
	// truncation drops the tail, and the kept (pre-operator) portion — whose
	// own quotes the model does balance — validates normally.
	flush()
	return out, nil
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
