// archive.go — pgBackRest shim verb: `pgbackrest archive-push %p` → native `wal push` (archive_command drop-in).
package pgbackrest

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newArchivePushCmd implements `pgbackrest --stanza=<n> archive-push %p`.
// PG invokes this from archive_command, with %p being the path to a
// completed WAL segment.
//
// Native dispatch: `pg_hardstorage wal push <stanza> <segment-path>`.
func newArchivePushCmd() *cobra.Command {
	c := &cobra.Command{
		Use:           "archive-push <segment-path>",
		Short:         "Archive one WAL segment",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchivePush(globalArgs, args[0])
		},
	}
	return c
}

func runArchivePush(a pgbackrestArgs, segmentPath string) error {
	native, warnings, err := mapToNativeArgs("wal push", a)
	if err != nil {
		return err
	}
	emitWarnings(warnings)

	// Verb shape: `wal push <stanza> <segment-path>`.
	out := []string{"wal", "push", a.stanza, segmentPath}
	out = append(out, native[1:]...)

	if rc := dispatchNative(out); rc != 0 {
		return fmt.Errorf("pg-hardstorage-pgbackrest: archive-push: native CLI exited %d", rc)
	}
	return nil
}

// newArchiveGetCmd implements `pgbackrest --stanza=<n> archive-get %f %p`.
// PG invokes this from restore_command.
//
// Native dispatch: `pg_hardstorage wal fetch <stanza> %f %p`.
func newArchiveGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:           "archive-get <segment-name> <target-path>",
		Short:         "Fetch one WAL segment from the repository",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveGet(globalArgs, args[0], args[1])
		},
	}
	return c
}

// exitAbortRecovery is the status that makes PostgreSQL treat a failed
// restore_command as fatal rather than as the end of the archive: its
// RestoreArchivedFile classifies the result with
// wait_result_is_any_signal(rc, true), which is true for any exit
// status greater than 125. Same value and rationale as
// compat/barmancloud's wal-restore and the shell wrapper in
// internal/restore/walfetchcmd/tail.go.
const exitAbortRecovery = 126

// exitSegmentAbsent is pgBackRest's (and PostgreSQL's) ordinary
// "segment not in the archive" answer — at the end of an unbounded
// recovery it means "stop replaying and promote".
const exitSegmentAbsent = 1

// restoreCommandExit is the three-way exit-code contract every
// restore_command shim speaks:
//
//	native 0            → exit 0   (segment delivered)
//	native 6 (notfound) → exit 1   (the genuine "no such segment")
//	anything else       → exit 126 (recovery ABORTS loudly)
//
// Collapsing every failure to 1 made an S3 503, an unreadable keyring
// or a stanza missing from pg_hardstorage.yaml read to PostgreSQL as a
// clean end of archive: the server promoted with unreplayed WAL still
// in the repository.
func restoreCommandExit(nativeRC int) int {
	switch nativeRC {
	case 0:
		return 0
	case int(output.ExitNotFound):
		return exitSegmentAbsent
	default:
		return exitAbortRecovery
	}
}

// abortRecovery marks an archive-get failure that must exit 126.
func abortRecovery(err error) error {
	return &shimError{
		exitCode: exitAbortRecovery,
		message: fmt.Sprintf("%v\npg-hardstorage-pgbackrest: archive-get: exiting %d to ABORT recovery — this is a fetch FAILURE, "+
			"not an end of archive; PostgreSQL must not promote here", err, exitAbortRecovery),
	}
}

func runArchiveGet(a pgbackrestArgs, segmentName, targetPath string) error {
	native, warnings, err := mapToNativeArgs("wal fetch", a)
	if err != nil {
		// Missing stanza / repo config never reached the repository,
		// so it cannot be "no such segment".
		return abortRecovery(err)
	}
	emitWarnings(warnings)

	out := []string{"wal", "fetch", a.stanza, segmentName, targetPath}
	out = append(out, native[1:]...)

	rc := dispatchNative(out)
	switch code := restoreCommandExit(rc); code {
	case 0:
		return nil
	case exitSegmentAbsent:
		return &shimError{
			exitCode: code,
			message: fmt.Sprintf("pg-hardstorage-pgbackrest: archive-get: %s not in the archive (native CLI exited %d)",
				segmentName, rc),
		}
	default:
		return abortRecovery(fmt.Errorf("pg-hardstorage-pgbackrest: archive-get: native CLI exited %d", rc))
	}
}
