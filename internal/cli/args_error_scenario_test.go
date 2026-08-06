package cli

// args_error_scenario_test.go — the unknown-deployment hint must only
// fire for commands whose positional IS a deployment.
//
// enrichUnknownDeploymentError turns "missing --repo" into a much more
// useful "deployment %q is not in pg_hardstorage.yaml (configured:
// ...)" — for `backup <deployment>` that is exactly right. It keyed
// only on the error code and the presence of "--repo" in the message,
// so `gameday run <scenario>` matched too: running
// `gameday run patroni_split_brain` without --repo reported that
// patroni_split_brain "is not in pg_hardstorage.yaml", listing the
// configured deployments and pointing at `deployment list`. The
// operator goes looking for a deployment they never meant to name,
// and the exit code moves from 2 (usage) to 6 (notfound).

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestEnrichUnknownDeployment_OnlyForDeploymentPositionals(t *testing.T) {
	missingRepo := output.NewError("usage.missing_flag",
		"gameday run: no repository to drill: pass --repo").Wrap(output.ErrUsage)

	cases := []struct {
		name     string
		use      string
		arg      string
		rewrites bool
	}{
		{
			name: "scenario positional is left alone",
			use:  "run <scenario>", arg: "patroni_split_brain", rewrites: false,
		},
		{
			name: "deployment positional still gets the hint",
			use:  "backup <deployment>", arg: "definitely-not-configured", rewrites: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Stage a config with a known deployment. Without this the
			// rewrite depends on whatever config the developer's
			// machine happens to have, and the test cannot fail
			// reliably — the first version of this file passed with the
			// fix reverted, for exactly that reason.
			t.Setenv("HOME", t.TempDir())
			t.Setenv("PG_HARDSTORAGE_ROOT", "")
			t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
			t.Setenv("PG_HARDSTORAGE_CONFIG",
				"deployments:\n  configured-one:\n"+
					"    pg_connection: postgresql://postgres@127.0.0.1:1/postgres\n"+
					"    repo: file:///tmp/does-not-matter\n")

			cmd := &cobra.Command{Use: tc.use, Run: func(*cobra.Command, []string) {}}
			if err := cmd.ParseFlags([]string{tc.arg}); err != nil {
				t.Fatal(err)
			}
			got := enrichUnknownDeploymentError(cmd, missingRepo)
			oe, ok := output.AsOutputError(got)
			if !ok {
				t.Fatalf("lost the structured error: %v", got)
			}
			rewritten := oe.Code == "notfound.deployment"

			if tc.rewrites && !rewritten {
				t.Errorf("`%s` with an unconfigured deployment %q did NOT get the "+
					"notfound.deployment hint; the hint is the useful half of this "+
					"helper and must survive the scoping fix", tc.use, tc.arg)
			}
			if !tc.rewrites && rewritten {
				t.Errorf("`%s` had its missing --repo rewritten to notfound.deployment for "+
					"positional %q.\n\nThat positional is a %s, not a deployment: the "+
					"operator is told to check `deployment list` for a name that was never "+
					"meant to be one, and the exit code moves from 2 (usage) to 6 (notfound).",
					tc.use, tc.arg, strings.Trim(positionalOf(tc.use), "<>"))
			}
		})
	}
}

// positionalOf returns the first <placeholder> in a Use string.
func positionalOf(use string) string {
	i := strings.Index(use, "<")
	if i < 0 {
		return "positional"
	}
	j := strings.Index(use[i:], ">")
	if j < 0 {
		return "positional"
	}
	return use[i : i+j+1]
}
