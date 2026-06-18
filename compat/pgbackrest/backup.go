// backup.go — pgBackRest shim verb: `pgbackrest backup [--type=full|incr|diff]` → native backup (diff refused).
package pgbackrest

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newBackupCmd implements `pgbackrest --stanza=<n> backup [--type=full|incr]`.
//
// Native dispatch:
//
//	full → `pg_hardstorage backup <stanza> --pg-connection ... --repo ...`
//	incr → same plus `--incremental-from latest`
//	diff → refused with remediation pointing at --type=incr
func newBackupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:           "backup",
		Short:         "Take a full or incremental backup",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackup(globalArgs)
		},
	}
	c.Flags().StringVar(&globalArgs.backupType, "type", "full",
		"backup type: full | incr | diff")
	return c
}

func runBackup(a pgbackrestArgs) error {
	switch strings.ToLower(a.backupType) {
	case "", "full":
		// fall through; no extra flag needed.
	case "incr":
		// fall through; --incremental-from latest appended below.
	case "diff":
		return refuseFlag("--type=diff",
			"use --type=incr (PG 17 page-level incremental); see docs/how-to/migration/from-pgbackrest.md")
	default:
		return refuseFlag("--type="+a.backupType,
			"supported types are full and incr")
	}

	native, warnings, err := mapToNativeArgs("backup", a)
	if err != nil {
		return err
	}
	emitWarnings(warnings)

	// Verb argument shape: `backup <stanza> ...flags...`.
	// mapToNativeArgs stamped only the verb so we splice the
	// positional in here.
	out := []string{native[0], a.stanza}
	out = append(out, native[1:]...)

	if strings.EqualFold(a.backupType, "incr") {
		out = append(out, "--incremental-from", "latest")
	}

	if rc := dispatchNative(out); rc != 0 {
		return fmt.Errorf("pg-hardstorage-pgbackrest: backup: native CLI exited %d", rc)
	}
	return nil
}
