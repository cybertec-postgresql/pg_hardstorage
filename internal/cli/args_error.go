// args_error.go — rewrites cobra arg-count failures into operator-actionable messages with examples.
package cli

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// requiredFlagNameRe extracts the quoted flag names from cobra's
// ValidateRequiredFlags message: `required flag(s) "repo", "pg-connection" not set`.
var requiredFlagNameRe = regexp.MustCompile(`"([^"]+)"`)

// isRequiredFlagError reports whether msg is cobra's required-flag failure.
func isRequiredFlagError(msg string) bool {
	return strings.HasPrefix(msg, "required flag(s) ") && strings.HasSuffix(msg, " not set")
}

// enrichRequiredFlagError rewrites cobra's required-flag failure into the
// structured `usage.missing_flag` error the rest of the CLI emits. Commands
// declare required flags via MarkFlagRequired (the single source of truth
// that --help, shell completion, and the LLM command-validator all read);
// this translation makes that declarative path produce the SAME error code
// and exit (ExitMisuse, via output.ErrUsage) the older hand-written
// "X is required" RunE checks did — so shell-script callers and the
// structured-error contract are unchanged.
//
// Returns the original error untouched when it isn't a required-flag
// failure or cmd is nil.
func enrichRequiredFlagError(cmd *cobra.Command, err error) error {
	if cmd == nil || err == nil {
		return err
	}
	msg := err.Error()
	if !isRequiredFlagError(msg) {
		return err
	}
	var flags []string
	for _, m := range requiredFlagNameRe.FindAllStringSubmatch(msg, -1) {
		flags = append(flags, "--"+m[1])
	}
	// Same structured error the runtime guard (requireFlags) and the old
	// manual checks produce — one message format, one place.
	return missingFlagErr(cmd, flags...)
}

// enrichArgsError rewrites cobra's bare arg-count failures
// ("accepts 2 arg(s), received 1") into the shape an
// operator can act on without re-reading the docs:
//
//	error: pg_hardstorage restore: needs 2 arguments (got 1)
//	  expected: <deployment> <backup-id|latest>
//	  example:  pg_hardstorage restore mydb1 latest
//
// The Use line on every cobra.Command already names the
// positional placeholders ("restore <deployment> <backup-
// id|latest>") — we just surface them.  When the command
// declares an explicit Example block, the first non-empty
// line is used verbatim; otherwise we synthesise an
// example by stitching the command path with the
// placeholders, optionally substituting recognisable
// tokens (`<backup-id|latest>` → `latest`).
//
// Returns the original error untouched when it isn't an
// arg-count failure or when cmd is nil — the goal is a
// targeted UX win, not a globally rewritten error
// surface.
func enrichArgsError(cmd *cobra.Command, err error) error {
	if cmd == nil || err == nil {
		return err
	}
	msg := err.Error()
	if !isArgsCountError(msg) {
		return err
	}
	// Compose the friendlier message.  Format chosen for
	// scannability at 3am: command path on the headline
	// (so the operator knows WHICH command failed when
	// the typo was a stale shell history line), then a
	// two-line block with what was expected and what
	// you'd type for a working invocation.
	path := cmd.CommandPath()
	expected := positionalPlaceholders(cmd.Use)
	example := exampleInvocation(cmd)

	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", path, rewriteArgsHeadline(msg))
	if expected != "" {
		fmt.Fprintf(&b, "\n  expected: %s", expected)
	}
	if example != "" {
		fmt.Fprintf(&b, "\n  example:  %s", example)
	}
	// Wrap as a usage-class error so the exit code
	// matches what cobra would have returned (output.ErrUsage
	// → exit code 2), keeping shell-script callers'
	// existing handling intact.
	return output.NewError("usage.bad_args", b.String()).Wrap(output.ErrUsage)
}

// isArgsCountError detects every shape cobra's built-in
// argument validators emit.  We deliberately match on
// "arg(s)" — the substring is unique to cobra's
// arg-count messages and won't collide with other
// errors that happen to mention "args".
func isArgsCountError(msg string) bool {
	return strings.Contains(msg, "arg(s)") && (strings.Contains(msg, "accepts ") ||
		strings.Contains(msg, "requires ") ||
		strings.Contains(msg, "received "))
}

// rewriteArgsHeadline turns cobra's wording into something
// closer to natural English.  We keep the numbers (those
// are the load-bearing facts) but drop the "arg(s)"
// awkwardness.
//
// Examples:
//
//	"accepts 2 arg(s), received 1"            → "needs 2 arguments (got 1)"
//	"requires at least 1 arg(s), only received 0" → "needs at least 1 argument (got 0)"
//	"accepts at most 3 arg(s), received 5"    → "accepts at most 3 arguments (got 5)"
//	"accepts between 1 and 3 arg(s), received 0" → "needs 1–3 arguments (got 0)"
//
// Falls back to the original string when the shape isn't
// one we know about.
func rewriteArgsHeadline(msg string) string {
	var n, m int
	switch {
	case sscan(msg, "accepts %d arg(s), received %d", &n, &m):
		return fmt.Sprintf("needs %s (got %d)", argsCountWord(n), m)
	case sscan(msg, "requires at least %d arg(s), only received %d", &n, &m):
		return fmt.Sprintf("needs at least %s (got %d)", argsCountWord(n), m)
	case sscan(msg, "accepts at most %d arg(s), received %d", &n, &m):
		return fmt.Sprintf("accepts at most %s (got %d)", argsCountWord(n), m)
	}
	var lo, hi int
	if sscan3(msg, "accepts between %d and %d arg(s), received %d", &lo, &hi, &m) {
		return fmt.Sprintf("needs %d–%d arguments (got %d)", lo, hi, m)
	}
	return msg
}

func sscan(s, format string, a, b *int) bool {
	n, err := fmt.Sscanf(s, format, a, b)
	return err == nil && n == 2
}
func sscan3(s, format string, a, b, c *int) bool {
	n, err := fmt.Sscanf(s, format, a, b, c)
	return err == nil && n == 3
}

func argsCountWord(n int) string {
	if n == 1 {
		return "1 argument"
	}
	return fmt.Sprintf("%d arguments", n)
}

// positionalPlaceholders extracts the angle-bracketed
// placeholder section of a Use line.  cobra Use lines
// look like "restore <deployment> <backup-id|latest>"
// — first token is the verb, rest is what we want.
//
// Returns empty when Use is just the verb (no
// positionals declared) so the caller can omit the
// "expected:" line entirely.
func positionalPlaceholders(use string) string {
	use = strings.TrimSpace(use)
	parts := strings.Fields(use)
	if len(parts) <= 1 {
		return ""
	}
	return strings.Join(parts[1:], " ")
}

// exampleInvocation returns the command's Example field
// when set (first non-empty line, trimmed), or
// synthesises one from the Use placeholders when
// Example is empty.  The synthesised form replaces
// recognisable tokens with sample values:
//
//	<deployment>          → "mydb1"
//	<backup-id|latest>    → "latest"
//	<name>                → "mydb1"
//	<url>                 → "file:///tmp/repo"
//	<id>                  → "<id>" (no clear default)
//
// When no substitution applies we keep the placeholder
// verbatim so the operator at least sees the SHAPE.
func exampleInvocation(cmd *cobra.Command) string {
	// Honour an explicit Example block first.
	if cmd.Example != "" {
		for _, line := range strings.Split(cmd.Example, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				return trimmed
			}
		}
	}
	expected := positionalPlaceholders(cmd.Use)
	if expected == "" {
		return ""
	}
	return cmd.CommandPath() + " " + substituteExampleTokens(expected)
}

// substituteExampleTokens swaps common placeholder
// shapes for sample values that look real enough to be
// runnable copy-paste.  Conservative: we only
// substitute placeholders we recognise; everything else
// stays a placeholder.
func substituteExampleTokens(s string) string {
	repl := strings.NewReplacer(
		"<deployment>", "mydb1",
		"<name>", "mydb1",
		"<backup-id|latest>", "latest",
		"<backup-id>", "latest",
		"<url>", "file:///tmp/repo",
		"<repo>", "file:///tmp/repo",
		"<file>", "/tmp/file",
		"<path>", "/tmp/path",
		"<dir>", "/tmp/dir",
		"<lsn>", "0/3000028",
	)
	return repl.Replace(s)
}

// enrichUnknownDeploymentError rewrites the "--pg-connection, --repo are
// required" class of failure into a "deployment not found" error when the
// evidence says that's what actually happened: the command was invoked
// with a positional deployment name, a config with deployments exists,
// and the name isn't in it.
//
// Operators with a configured catalogue never pass those flags — when
// they typo the deployment (`backup db2` vs `db1`) the missing-flag
// message sends them hunting for connection strings instead of at the
// actual problem. URL-ish positionals (repo verbs) are left alone.
func enrichUnknownDeploymentError(cmd *cobra.Command, err error) error {
	if cmd == nil || err == nil {
		return err
	}
	oe, ok := output.AsOutputError(err)
	if !ok || oe.Code != "usage.missing_flag" {
		return err
	}
	if !strings.Contains(oe.Message, "--pg-connection") && !strings.Contains(oe.Message, "--repo") {
		return err
	}
	args := cmd.Flags().Args()
	if len(args) == 0 {
		return err
	}
	name := args[0]
	if strings.Contains(name, "://") || strings.Contains(name, "/") {
		return err // a URL/path positional, not a deployment name
	}
	p, perr := paths.Resolve(paths.DefaultOptions())
	if perr != nil {
		return err
	}
	loaded, lerr := config.Load(p)
	if lerr != nil || len(loaded.Config.Deployments) == 0 {
		return err
	}
	if _, exists := loaded.Config.Deployments[name]; exists {
		return err // known deployment; the flags really are missing
	}
	known := make([]string, 0, len(loaded.Config.Deployments))
	for n := range loaded.Config.Deployments {
		known = append(known, n)
	}
	sort.Strings(known)
	return output.NewError("notfound.deployment",
		fmt.Sprintf("%s: deployment %q is not in pg_hardstorage.yaml (configured: %s)",
			cmd.Name(), name, strings.Join(known, ", "))).
		WithSuggestion(&output.Suggestion{
			Human:   "check the name with `pg_hardstorage deployment list`, or pass --pg-connection/--repo explicitly for an unconfigured database",
			Command: "pg_hardstorage deployment list",
		})
}
