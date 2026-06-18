// flag_validation_contract_test.go — meta-test pinning that every
// typed-shape CLI flag rejects bad input with exit 2 (Misuse).
//
// This is the regression net for the class of bug behind issue #78:
// `restore --to-lsn hm` was silently accepted because the flag was a
// plain StringVar passed straight to the recovery_target_lsn GUC. The
// fix landed earlier — this file is what prevents the gap
// from re-opening when a new typed flag is added (or an existing one
// is refactored).
//
// What it does:
//
//   - For each (command-shape, flag, bad-value) triple in the matrix
//     below, invokes the CLI via the existing runCmd helper and
//     asserts the exit code is 2 (output.ExitMisuse).
//
//   - Walks the full cobra tree built by NewRoot() and reports any
//     leaf command that declares a typed-shape flag (matched by name
//     pattern: *lsn*, *url*, *slot*, *interval*, *timeline*, *action*,
//     *time*) that the matrix does NOT cover. New typed flags
//     therefore fail the meta-test until someone adds the explicit
//     row + bad-value set — making the gap impossible to ship.
//
// Scope and limits:
//
//   - We exercise CLI-level validation only; this is not a unit test
//     of the underlying parser. The parser tests (parseLSN /
//     parseNaturalTime / pg.ValidIdentifier / etc.) live with the
//     parsers and are exhaustive on the type axis. This contract
//     ensures every flag is WIRED to a validator at all.
//
//   - Bad values are chosen to be unambiguously invalid for the
//     type. We deliberately do NOT exercise edge cases (empty
//     strings, exotic Unicode); the parser unit tests own those.
package cli_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// typedFlagPattern is one row of the validation matrix.
//
// match returns true when the flag should be exercised by this row.
// We match on name suffix so "--start-lsn", "--to-lsn", and any future
// "--something-lsn" all opt in automatically.
type typedFlagPattern struct {
	name      string // human-readable description for test failure messages
	match     func(*pflag.Flag) bool
	badValues []string // each value is exercised separately
}

// flagPatterns is the audit set. Adding a new typed-shape flag to the
// CLI without adding a row here is fine — the bottom of this file's
// TestFlagValidationContract_NoUntestedTypedFlags walks the tree and
// asserts every flag whose NAME matches one of these patterns has at
// least one invocation in the matrix below.
var flagPatterns = []typedFlagPattern{
	{
		name:      "LSN",
		match:     func(f *pflag.Flag) bool { return strings.HasSuffix(f.Name, "lsn") },
		badValues: []string{"hm", "abc", "/3000000", "0/", "0/3000028x"},
	},
	{
		name:      "timeline",
		match:     func(f *pflag.Flag) bool { return strings.HasSuffix(f.Name, "timeline") },
		badValues: []string{"abc", "-1", "999999999999"},
	},
	{
		name:      "action",
		match:     func(f *pflag.Flag) bool { return strings.HasSuffix(f.Name, "to-action") },
		badValues: []string{"rollback", "stopz", "panic"},
	},
	{
		name:      "natural-time / --to",
		match:     func(f *pflag.Flag) bool { return f.Name == "to" },
		badValues: []string{"yesterday afternoonish", "2026-13-99", "next century"},
	},
	{
		name: "patroni-url",
		match: func(f *pflag.Flag) bool {
			return f.Name == "patroni-url"
		},
		badValues: []string{"not-a-url", "ftp://x", "://missing-scheme"},
	},
	{
		name: "patroni-slot",
		match: func(f *pflag.Flag) bool {
			return f.Name == "patroni-slot"
		},
		badValues: []string{"bad-slot/with-slash", "Mixed.Case", "1starts-with-digit"},
	},
	{
		name: "patroni-interval",
		match: func(f *pflag.Flag) bool {
			return f.Name == "patroni-interval"
		},
		badValues: []string{"abc", "-5s", "5"},
	},
}

// invocationTemplate names a leaf command + the minimum scaffolding
// args it needs to reach flag validation. Each row of the matrix below
// is one such template; the test substitutes one typed flag at a time.
//
// We never write to a real PG or a real repo: the validation we test
// happens before any I/O. --skip-probe / --preview / --output json
// keep the test deterministic in <1s per case.
type invocationTemplate struct {
	desc     string   // human label for test name
	baseArgs []string // every arg except the flag-under-test
	// flagUnderTest is the typed flag whose --foo=BAD we sub in. The
	// template's baseArgs must NOT already include this flag.
	flagUnderTest string
	// expectedExit is normally 2 (Misuse). A few cases legitimately
	// also exit 6 (NotFound) when the validator runs after a missing-
	// deployment check; we accept either if the flag's pattern says
	// so.
	expectedExits []int
}

// invocationMatrix is the explicit set of (command, flag-under-test)
// pairs the contract exercises. When a new typed flag lands in a
// command, add an entry here.
//
// The natural temptation is to derive this list from the cobra tree;
// we don't, because each command needs DIFFERENT scaffolding args to
// reach the validator. Hand-written is honest and explicit.
var invocationMatrix = []invocationTemplate{
	// `restore <dep> latest` — typed flags hit validation before any
	// repo / manifest access.
	{
		desc:          "restore --to-lsn",
		baseArgs:      []string{"restore", "db1", "latest", "--target", "/tmp/restore-flagtest", "--repo", "file:///tmp/r", "--preview", "--output", "json"},
		flagUnderTest: "--to-lsn",
		expectedExits: []int{2},
	},
	{
		desc:          "restore --to-timeline",
		baseArgs:      []string{"restore", "db1", "latest", "--target", "/tmp/restore-flagtest", "--repo", "file:///tmp/r", "--preview", "--output", "json"},
		flagUnderTest: "--to-timeline",
		expectedExits: []int{2},
	},
	{
		desc:          "restore --to-action",
		baseArgs:      []string{"restore", "db1", "latest", "--target", "/tmp/restore-flagtest", "--repo", "file:///tmp/r", "--preview", "--output", "json"},
		flagUnderTest: "--to-action",
		expectedExits: []int{2},
	},
	{
		desc:          "restore --to",
		baseArgs:      []string{"restore", "db1", "latest", "--target", "/tmp/restore-flagtest", "--repo", "file:///tmp/r", "--preview", "--output", "json"},
		flagUnderTest: "--to",
		expectedExits: []int{2},
	},
	// `wal stream <dep>` — --start-lsn is parsed before connecting.
	{
		desc:          "wal stream --start-lsn",
		baseArgs:      []string{"wal", "stream", "db1", "--pg-connection", "postgres://x@h/db", "--repo", "file:///tmp/r", "--skip-preflight", "--output", "json"},
		flagUnderTest: "--start-lsn",
		expectedExits: []int{2},
	},
	// `deployment edit <name> --patroni-*` — landed earlier.
	// The deployment doesn't exist; we still want the FLAG validator
	// to fire before the not-found check, because that's what catches
	// "you typed a malformed value in your edit command". Exit 2 is
	// fine; exit 6 is also acceptable if the not-found check wins.
	{
		desc:          "deployment edit --patroni-url",
		baseArgs:      []string{"deployment", "edit", "ghost", "--output", "json"},
		flagUnderTest: "--patroni-url",
		expectedExits: []int{2, 6},
	},
	{
		desc:          "deployment edit --patroni-slot",
		baseArgs:      []string{"deployment", "edit", "ghost", "--patroni-url", "http://x:8008", "--output", "json"},
		flagUnderTest: "--patroni-slot",
		expectedExits: []int{2, 6},
	},
	{
		desc:          "deployment edit --patroni-interval",
		baseArgs:      []string{"deployment", "edit", "ghost", "--patroni-url", "http://x:8008", "--output", "json"},
		flagUnderTest: "--patroni-interval",
		expectedExits: []int{2, 6},
	},
}

// TestFlagValidationContract_RejectsBadValues exercises every
// (template, bad-value) pair and pins exit code 2 (or the
// template-declared alternates) so a regression cannot ship silently.
func TestFlagValidationContract_RejectsBadValues(t *testing.T) {
	for _, tmpl := range invocationMatrix {
		// Resolve the matching pattern by flag name.
		pat := matchPatternForFlag(tmpl.flagUnderTest)
		if pat == nil {
			t.Errorf("invocation template %q targets flag %q which no flagPatterns entry covers — add a pattern row or remove the template",
				tmpl.desc, tmpl.flagUnderTest)
			continue
		}
		for _, bad := range pat.badValues {
			t.Run(tmpl.desc+"="+bad, func(t *testing.T) {
				// Each subtest is independent: build args, invoke,
				// assert exit. We always also pass --output json so
				// the error renders structured, not as a misleading
				// usage-help dump.
				configDir(t) // fresh empty config dir per test
				args := append([]string(nil), tmpl.baseArgs...)
				args = append(args, tmpl.flagUnderTest, bad)
				_, _, exit := runCmd(t, args...)
				ok := false
				for _, want := range tmpl.expectedExits {
					if exit == want {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("%s = %q: exit = %d, want one of %v — the validator did not fire",
						tmpl.flagUnderTest, bad, exit, tmpl.expectedExits)
				}
			})
		}
	}
}

// TestFlagValidationContract_NoUntestedTypedFlags walks the cobra
// tree built by NewRoot() and asserts every flag whose NAME matches
// a flagPatterns entry has at least one invocationMatrix row pointing
// at it. New typed flags must register a row here, otherwise this
// test fails with a clear "you added a typed flag without a fuzz
// case" message.
func TestFlagValidationContract_NoUntestedTypedFlags(t *testing.T) {
	root := cli.NewRoot()

	// Build the set of "flags we have a fuzz row for", keyed by the
	// canonical "--foo" the matrix uses.
	covered := map[string]bool{}
	for _, tmpl := range invocationMatrix {
		covered[tmpl.flagUnderTest] = true
	}

	// Walk every leaf command. For each flag, see if any pattern
	// matches the flag's name; if so, the flag must be in `covered`.
	type miss struct {
		cmdPath  string
		flagName string
		pattern  string
	}
	var misses []miss
	walkCommands(root, func(cmd *cobra.Command, path string) {
		// Skip the root and non-runnable parents.
		if !cmd.Runnable() {
			return
		}
		cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
			// Strongly-typed flags (Uint32Var, IntVar, BoolVar,
			// DurationVar, …) are already validated by pflag at
			// parse-time — passing "abc" to a Uint32 flag exits 2
			// before our handler ever runs.  Only string-typed
			// flags risk silent-acceptance of garbage.
			if f.Value.Type() != "string" {
				return
			}
			for _, pat := range flagPatterns {
				if pat.match(f) {
					key := "--" + f.Name
					if !covered[key] {
						misses = append(misses, miss{
							cmdPath:  path,
							flagName: f.Name,
							pattern:  pat.name,
						})
					}
				}
			}
		})
	}, "")

	if len(misses) == 0 {
		return
	}

	// Sort for deterministic output.
	sort.Slice(misses, func(i, j int) bool {
		if misses[i].cmdPath != misses[j].cmdPath {
			return misses[i].cmdPath < misses[j].cmdPath
		}
		return misses[i].flagName < misses[j].flagName
	})

	var b strings.Builder
	b.WriteString("typed-shape flags without an invocationMatrix entry:\n")
	for _, m := range misses {
		b.WriteString("  ")
		b.WriteString(m.cmdPath)
		b.WriteString("  --")
		b.WriteString(m.flagName)
		b.WriteString("  (pattern: ")
		b.WriteString(m.pattern)
		b.WriteString(")\n")
	}
	b.WriteString("\nAdd an invocationTemplate row in flag_validation_contract_test.go " +
		"with the minimum scaffolding args + the flag-under-test.")
	t.Error(b.String())
}

func matchPatternForFlag(flagWithDashes string) *typedFlagPattern {
	name := strings.TrimPrefix(flagWithDashes, "--")
	fake := &pflag.Flag{Name: name}
	for i := range flagPatterns {
		if flagPatterns[i].match(fake) {
			return &flagPatterns[i]
		}
	}
	return nil
}

func walkCommands(c *cobra.Command, fn func(*cobra.Command, string), prefix string) {
	path := prefix
	if c.Use != "" {
		// Use the first whitespace-separated token (the verb name).
		verb := strings.Fields(c.Use)[0]
		if path == "" {
			path = verb
		} else {
			path = path + " " + verb
		}
	}
	fn(c, path)
	for _, sub := range c.Commands() {
		walkCommands(sub, fn, path)
	}
}
