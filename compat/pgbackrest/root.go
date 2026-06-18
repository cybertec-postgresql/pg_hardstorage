// Package pgbackrest implements the drop-in compat shim that
// mimics the pgBackRest CLI surface so operators can swap the
// pgbackrest binary on PATH for pg-hardstorage-pgbackrest
// without rewriting cron jobs, archive_command settings, or
// monitoring scripts.
//
// Each verb in this package parses pgBackRest-style flags,
// builds a synthetic argv for the native pg_hardstorage CLI,
// and dispatches via internal/cli.NewRoot() + SetArgs() +
// Execute().  No internal/backup or internal/wal callouts: the
// shim tracks future native-CLI evolution by going through the
// public CLI surface.
//
// See compat/README.md for the architectural rationale and
// docs/how-to/migration/from-pgbackrest.md for the operator-
// facing migration path.
package pgbackrest

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// NewRoot returns the cobra command tree exposed by
// pg-hardstorage-pgbackrest.  The cmd/pg-hardstorage-pgbackrest
// dispatcher mounts this directly at top level so the binary
// presents itself as `pgbackrest`.
//
// Verbs implemented (8): stanza-create, backup, restore,
// archive-push, archive-get, info, check, verify.
// Anything else lands in refuseUnknown() with a clear
// remediation pointing at the native equivalent.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "pgbackrest",
		Short: "pg_hardstorage drop-in shim for pgBackRest",
		Long: `pg-hardstorage-pgbackrest mimics the pgBackRest command
surface so existing cron jobs, archive_command lines, and
monitoring scripts keep working after the binary is symlinked
into PATH as ` + "`pgbackrest`" + `.

Backups, WAL archive, and restores produced by this shim are
native pg_hardstorage artefacts — chunked through FastCDC,
signed, optionally KMS-wrapped.  Old pgBackRest repos remain
readable only by pgBackRest; the shim does not parse them.

See docs/how-to/migration/from-pgbackrest.md for the cutover
playbook.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// pgBackRest's --stanza is global — we attach it on the root
	// so every subcommand gets it for free.  The remaining
	// ~12 most-cited flags are persistent too; each verb
	// reads what it needs via mapToNativeArgs.
	registerCommonFlags(root.PersistentFlags())

	// Suppress cobra's default `help` and `completion`
	// children — pgBackRest doesn't have them and we don't
	// want shim users to see surface area that isn't real.
	root.SetHelpCommand(&cobra.Command{Hidden: true})
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(
		newStanzaCreateCmd(),
		newBackupCmd(),
		newRestoreCmd(),
		newArchivePushCmd(),
		newArchiveGetCmd(),
		newInfoCmd(),
		newCheckCmd(),
		newVerifyCmd(),
	)

	// Cobra by default prints "unknown command" verbatim.
	// We override the args-validation path so any verb not
	// in the eight above lands in refuseUnknown with the
	// canonical remediation message.
	root.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return refuseUnknown(args[0])
	}
	root.Args = cobra.ArbitraryArgs

	return root
}

// Execute parses os.Args and runs the shim.
// cmd/pg-hardstorage-pgbackrest/main.go calls this directly.
//
// Refusals (unknown verb, --type=diff, etc.) exit with code
// 2 — matching the Barman shim and Cobra's usage-error
// convention.  Other failures exit 1.
func Execute() int {
	root := NewRoot()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitCode(err)
	}
	return 0
}

// notImplementedExitCode is what every refusal returns —
// kept consistent with the Barman shim so wrapper scripts
// that test for "tool exited non-zero" behave the same
// regardless of which legacy tool the operator was running.
const notImplementedExitCode = 2

// shimError carries an exit code so Execute can return the
// right process status.  Mirrors compat/barman's pattern.
type shimError struct {
	exitCode int
	message  string
}

// Error returns the pre-formatted refusal message.
func (e *shimError) Error() string { return e.message }

// ExitCode returns the requested process exit code, or 1 if
// the error is some other type that didn't carry one.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if se, ok := err.(*shimError); ok {
		return se.exitCode
	}
	return 1
}

// refuseUnknown is the one-stop helper for any pgBackRest
// command we don't translate in v1.1.  Output format is
// stable across the shim:
//
//	pg-hardstorage-pgbackrest: <command>: not implemented
//	in v1.1; native equivalent: <suggestion>
func refuseUnknown(command string) error {
	return &shimError{
		exitCode: notImplementedExitCode,
		message: fmt.Sprintf(
			"pg-hardstorage-pgbackrest: %s: not implemented in v1.1; native equivalent: pg_hardstorage --help",
			command),
	}
}

// refuseFlag is the same idea for an unsupported flag inside
// a translated verb (e.g. --type=diff inside `backup`).
func refuseFlag(flag, suggestion string) error {
	return &shimError{
		exitCode: notImplementedExitCode,
		message: fmt.Sprintf(
			"pg-hardstorage-pgbackrest: %s: not implemented in v1.1; native equivalent: %s",
			flag, suggestion),
	}
}

// stderrWriter resolves to os.Stderr unless overridden in tests.
var stderrWriter io.Writer = os.Stderr
