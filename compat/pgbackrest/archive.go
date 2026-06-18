// archive.go — pgBackRest shim verb: `pgbackrest archive-push %p` → native `wal push` (archive_command drop-in).
package pgbackrest

import (
	"fmt"

	"github.com/spf13/cobra"
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
	native, warnings, err := mapToNativeArgs("wal", a)
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

func runArchiveGet(a pgbackrestArgs, segmentName, targetPath string) error {
	native, warnings, err := mapToNativeArgs("wal", a)
	if err != nil {
		return err
	}
	emitWarnings(warnings)

	out := []string{"wal", "fetch", a.stanza, segmentName, targetPath}
	out = append(out, native[1:]...)

	if rc := dispatchNative(out); rc != 0 {
		// Exit 1 is the well-defined "no more WAL" signal for
		// PG's restore_command — bubble it through unchanged.
		return fmt.Errorf("pg-hardstorage-pgbackrest: archive-get: native CLI exited %d", rc)
	}
	return nil
}
