// rotate_policy_source.go — where a rotate run gets its retention policy.
//
// Issue #66 follow-up (@marsqd): `pg_hardstorage rotate local` planned
// with the hard-coded GFS defaults while the deployment declared
//
//	retention:
//	  policy: count
//	  keep_fulls: 2
//
// The scheduled rotate task has always read that block (buildRotateTask →
// buildRetentionPolicy); the manual command never did. `--repo` had been
// back-filled from the deployment since #12, so the command looked
// config-aware — it named the right repo and the right deployment and then
// applied somebody else's policy to it.
//
// That is worse than an inconvenience. The #66 documentation fix tells
// operators without the agent to chain `backup && rotate --apply` in cron.
// Against a `count: 2` deployment that command would have kept GFS's
// 7 daily + 4 weekly + 12 monthly + 5 yearly — or, for a deployment
// configured to keep MORE than GFS keeps, soft-deleted backups its own
// declared policy protects. The same deployment got two different
// retention policies depending on whether a person or the agent ran it.
//
// Precedence, deliberately coarse:
//
//  1. Any retention flag on the command line (--policy, --keep-*) → the
//     flags define the whole policy, exactly as before. Mixing a config
//     `policy: count` with a command-line `--keep-daily` has no sensible
//     meaning, so flags are all-or-nothing rather than merged field-wise.
//  2. Otherwise, the deployment's `retention:` block — through the SAME
//     builder the agent uses, so manual and scheduled runs cannot drift.
//  3. Otherwise, the built-in GFS defaults.
//
// The source is reported per deployment, so a plan says which of the
// three it followed instead of leaving the operator to infer it.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

const (
	policySourceFlags   = "flags"
	policySourceConfig  = "pg_hardstorage.yaml"
	policySourceDefault = "built-in default"
)

// rotatePolicyFlags lists every flag that shapes the retention policy.
var rotatePolicyFlags = []string{
	"policy", "keep-daily", "keep-weekly", "keep-monthly", "keep-yearly", "keep-for", "keep-fulls",
}

// retentionFlagsChanged reports whether the operator set any
// policy-shaping flag explicitly.
func retentionFlagsChanged(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	for _, n := range rotatePolicyFlags {
		if f := cmd.Flags().Lookup(n); f != nil && f.Changed {
			return true
		}
	}
	return false
}

// retentionDeclared reports whether a deployment's config says anything
// about retention at all. An empty block is "not declared", so a
// deployment that never mentions retention keeps the built-in default
// and is reported as such rather than as coming from config.
func retentionDeclared(r config.RetentionConfig) bool {
	return r != (config.RetentionConfig{})
}

// resolveRotatePolicy picks the policy for one deployment. deps may be
// nil (no config file, or it failed to load), in which case only flags
// and the built-in default are in play.
func resolveRotatePolicy(deployment string, flagsSet bool, opts rotateOpts,
	deps map[string]config.DeploymentConfig) (retention.Policy, string, error) {

	if flagsSet {
		p, err := buildPolicy(opts)
		return p, policySourceFlags, err
	}
	if dep, ok := deps[deployment]; ok && retentionDeclared(dep.Retention) {
		p, err := buildRetentionPolicy(dep.Retention)
		if err != nil {
			// A retention block that does not parse must stop the run.
			// Falling back to GFS here would re-create the exact bug this
			// file exists to fix, just with a worse excuse.
			return nil, policySourceConfig, output.NewError("config.invalid_retention",
				fmt.Sprintf("rotate: deployment %q: %v", deployment, err)).
				WithSuggestion(&output.Suggestion{
					Human: "fix the deployment's retention block in pg_hardstorage.yaml, or pass --policy and --keep-* to override it for this run",
				}).Wrap(err)
		}
		return p, policySourceConfig, nil
	}
	p, err := buildPolicy(opts) // flags untouched → their defaults = GFS
	return p, policySourceDefault, err
}
