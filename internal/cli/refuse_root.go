// refuse_root.go — PersistentPreRunE chain + root-uid refusal gate (with per-command allowlist).
package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// chainPreRunE composes a list of PersistentPreRunE handlers
// left-to-right; the first error short-circuits the chain.  Cobra
// only natively supports one PersistentPreRunE per command, and we
// need at least two (refuseRoot + installDispatcher).
func chainPreRunE(fns ...func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		for _, fn := range fns {
			if err := fn(cmd, args); err != nil {
				return err
			}
		}
		return nil
	}
}

// geteuid is a test seam.  Production: os.Geteuid.  Tests inject a
// constant 0 to verify the refuse-root gate fires.
var geteuid = os.Geteuid

// rootRefusalAllowedCommands lists subcommands that legitimately
// need to run before / outside the gate.  At time of writing,
// `version` and `--help` are the only safe ones: they emit static
// metadata and never touch a repo, PG connection, or the keyring.
// Cobra dispatches --help before any RunE, so we only need to
// list the value-printing subcommands here.
var rootRefusalAllowedCommands = map[string]bool{
	"version":    true,
	"completion": true,
}

// refuseRoot is the PersistentPreRunE wrapper that rejects euid 0
// at the top of every command.  The pg_hardstorage agent is
// designed to run as a dedicated unprivileged system user
// (`pgbackup` on Debian/RPM, `runAsNonRoot: true` in k8s); running
// as root is never required, runs the keyring + repo with too-wide
// perms, and historically led to the drop-to-postgres hack in
// postverify that we've now removed.
//
// `version` / `completion` are allow-listed because they emit
// static metadata and never touch real state.  --help and -h are
// dispatched by Cobra before any PersistentPreRunE so they don't
// need explicit allow-listing.
func refuseRoot(cmd *cobra.Command, _ []string) error {
	if geteuid() != 0 {
		return nil
	}
	if rootRefusalAllowedCommands[cmd.Name()] {
		return nil
	}
	// Walk up to find a top-level subcommand name too, so
	// `pg_hardstorage version --json` still hits the allow-list
	// when Cobra dispatched a child of the version command.
	for parent := cmd; parent != nil; parent = parent.Parent() {
		if rootRefusalAllowedCommands[parent.Name()] {
			return nil
		}
	}
	return output.NewError("usage.refused_as_root",
		"pg_hardstorage refuses to run as root (euid 0). "+
			"Run as a dedicated system user — the Debian / RPM packages create `pgbackup` and the "+
			"shipped systemd unit (User=pgbackup) is the canonical way to launch the agent. "+
			"In containers, set securityContext.runAsNonRoot: true (helm charts/pg-hardstorage-sidecar "+
			"sets this by default).").
		WithSuggestion(&output.Suggestion{
			Human: "switch to a non-root user before launching pg_hardstorage; the systemd unit at /lib/systemd/system/pg_hardstorage.service does this for you.",
		}).Wrap(output.ErrUsage)
}
