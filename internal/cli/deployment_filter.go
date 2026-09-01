// deployment_filter.go — validate a --deployment filter before a
// verdict-producing walk uses it.
package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// maxListedDeployments caps how many known names an unknown-deployment
// error echoes back. Enough to spot a typo on a normal fleet; a large
// one gets a count instead of a wall of text.
const maxListedDeployments = 12

// requireDeploymentExists refuses a --deployment value that names no
// deployment in the repo.
//
// The commands that call this produce a PASS/FAIL verdict with its own
// exit code, and they scope their walk by building a key prefix from
// the flag:
//
//	manifests/<deployment>/backups/
//
// A name that does not exist yields an empty listing, so every counter
// lands on zero, nothing is classified as broken, and the command exits
// 0 with a body that reads as a clean result — while echoing the
// operator's own typo back as the scope it checked. A compliance job
// pointed at a renamed or retired deployment reports green forever.
//
// The check is on the NAME, deliberately, and not on the resulting
// count. Zero manifests is also the correct answer for a deployment
// that genuinely exists and has had every backup tombstoned, so
// inferring "you must have meant something else" from an empty walk
// would refuse a legitimate run. ManifestStore.Deployments enumerates
// the manifests/ prefix, and tombstones live under the same prefix, so
// such a deployment is still listed here.
//
// This is NOT extended to --kek-ref. For the post-rotation audit that
// flag exists to serve — "what is still wrapped under the old ref?" —
// zero matches is the success condition, not a mistake.
func requireDeploymentExists(ctx context.Context, sp storage.StoragePlugin, command, deployment string) error {
	if deployment == "" {
		return nil // unfiltered: the walk covers every deployment
	}
	known, err := backup.NewManifestStore(sp).Deployments(ctx)
	if err != nil {
		// Enumeration failed, so we cannot tell whether the name is
		// real. Surface it rather than assuming either way: silently
		// continuing would restore the vacuous pass, and silently
		// refusing would block a valid run on a transient List error.
		return output.NewError("repo.list_deployments_failed",
			fmt.Sprintf("%s: --deployment %q could not be checked: %v", command, deployment, err)).Wrap(err)
	}
	for _, d := range known {
		if d == deployment {
			return nil
		}
	}
	return output.NewError("usage.unknown_deployment",
		fmt.Sprintf("%s: no deployment named %q in this repo", command, deployment)).
		Wrap(output.ErrUsage).
		WithSuggestion(&output.Suggestion{
			Human: "check the spelling — a --deployment that matches nothing would otherwise " +
				"walk an empty prefix and report a clean result. " + knownDeploymentsHint(known),
			Command: "pg_hardstorage list deployments",
		})
}

// knownDeploymentsHint renders the repo's deployment names for an
// error message, sorted and capped.
func knownDeploymentsHint(known []string) string {
	if len(known) == 0 {
		return "This repo has no deployments at all yet."
	}
	names := append([]string(nil), known...)
	sort.Strings(names)
	if len(names) > maxListedDeployments {
		return fmt.Sprintf("This repo has %d deployments; the first %d are: %s, ...",
			len(known), maxListedDeployments,
			strings.Join(names[:maxListedDeployments], ", "))
	}
	return "Known deployments: " + strings.Join(names, ", ") + "."
}
