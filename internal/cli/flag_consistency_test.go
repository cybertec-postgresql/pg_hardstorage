package cli_test

// flag_consistency_test.go — one binary, one grammar, enforced.
//
// The duration-unit divergences were fixed twice in one release and
// regrew between the two fixes: repo gc --min-chunk-age was made
// day-aware while repair --min-chunk-age (same name, same meaning,
// different command) stayed hour-bound. Fixing instances one at a
// time loses to the rate at which flags are added; these tests fix
// the CLASS by walking the whole command tree and asserting the
// policy on every flag at once. A new flag that violates the grammar
// fails here on the day it is added, with a message naming it.

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// TestEveryDurationFlagAcceptsDayUnits: the policy is uniform —
// every duration-typed flag parses day and week units, even where the
// unit is unlikely (--status-interval 1d is silly but harmless).
// Uniformity is the point: operators learn the grammar once, and a
// retention flag can never again reject the "30d" its sibling
// accepts.
func TestEveryDurationFlagAcceptsDayUnits(t *testing.T) {
	root := cli.NewRoot()
	checked := 0
	visit := func(c *cobra.Command, _ string) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Value.Type() != "duration" {
				return
			}
			checked++
			saved := f.Value.String()
			for _, in := range []string{"1d", "2w", "90m"} {
				if err := f.Value.Set(in); err != nil {
					t.Errorf("%s --%s rejects %q: %v\n"+
						"Every duration flag must accept day/week units — declare it with "+
						"DurationDaysVar, not pflag's DurationVar.", c.CommandPath(), f.Name, in, err)
				}
			}
			_ = f.Value.Set(saved)
		})
	}
	walkCommands(root, visit, "")
	if checked < 20 {
		t.Fatalf("only %d duration flags found — the tree walk is broken, not the flags", checked)
	}
}

var dayExampleRe = regexp.MustCompile(`\b\d+d\b`)

// sameNameTypeAllowlist holds the flags that legitimately mean
// different things on different commands. Every entry needs a reason;
// an empty reason is a bug in this file.
var sameNameTypeAllowlist = map[string]string{
	"since": "audit-family --since is a point in time (string: RFC3339 or duration ago); " +
		"gameday report --since is a WINDOW (duration; 0 = unbounded, reported as " +
		"window_seconds); logs --since is a journalctl passthrough (string)",
	"target": "insider learn --target is a duration budget; restore/drill --target is a " +
		"recovery-target string",
	"url": "patroni --url is the Patroni REST endpoint, not a repo URL",
	"threshold": "anomaly check --threshold is a score (float); approval/threshold-roster " +
		"--threshold is a signer count (int)",
	"repo": "server --repo is repeatable (stringArray) — it serves several repositories; " +
		"every other command operates on exactly one (string)",
	"horizon": "capacity report --horizon is one duration; forecast --horizon is a LIST of " +
		"horizons to project (stringSlice)",
}

// TestSameFlagNameMeansSameType: a flag name that appears on several
// commands must have one type everywhere, or be explicitly excused
// above. gameday --since being a duration while eight other --since
// flags were strings is how an operator learns the wrong grammar on
// one command and gets exit 2 on the next.
func TestSameFlagNameMeansSameType(t *testing.T) {
	root := cli.NewRoot()
	types := map[string]map[string][]string{} // name -> type -> commands
	visit := func(c *cobra.Command, _ string) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			byType, ok := types[f.Name]
			if !ok {
				byType = map[string][]string{}
				types[f.Name] = byType
			}
			byType[f.Value.Type()] = append(byType[f.Value.Type()], c.CommandPath())
		})
	}
	walkCommands(root, visit, "")
	for name, byType := range types {
		if len(byType) == 1 {
			continue
		}
		if reason, excused := sameNameTypeAllowlist[name]; excused && reason != "" {
			continue
		}
		var detail []string
		for typ, cmds := range byType {
			detail = append(detail, typ+": "+strings.Join(cmds, ", "))
		}
		t.Errorf("--%s has %d types across the tree (%s)\n"+
			"Give it one type everywhere or add an allowlist entry WITH a reason.",
			name, len(byType), strings.Join(detail, " | "))
	}
}

// TestPointInTimeSinceFlagsShareTheGrammar: every string --since that
// is a point in time must advertise the shared grammar (RFC3339 or a
// duration like 7d) in its help, so the contract parseSinceUntil
// implements is visible wherever the flag appears. logs --since is a
// journalctl passthrough and is excused explicitly.
func TestPointInTimeSinceFlagsShareTheGrammar(t *testing.T) {
	root := cli.NewRoot()
	excused := map[string]string{
		"logs": "journalctl passthrough — its grammar is journalctl's, not ours",
	}
	visit := func(c *cobra.Command, _ string) {
		f := c.LocalFlags().Lookup("since")
		if f == nil || f.Value.Type() != "string" {
			return
		}
		leaf := c.Name()
		if _, ok := excused[leaf]; ok {
			return
		}
		if !strings.Contains(f.Usage, "RFC3339") {
			t.Errorf("%s --since help does not mention RFC3339: %q\n"+
				"Point-in-time --since flags parse via parseSinceUntil and must say so.",
				c.CommandPath(), f.Usage)
		}
		if !dayExampleRe.MatchString(f.Usage) {
			t.Errorf("%s --since help shows no day-unit example (like 7d or 30d): %q\n"+
				"The grammar accepts days; the help must show it.",
				c.CommandPath(), f.Usage)
		}
	}
	walkCommands(root, visit, "")
}
