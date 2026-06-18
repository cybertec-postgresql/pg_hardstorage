// restore.go — pgBackRest shim verb: `pgbackrest restore [--target] [--type]` → native restore with PITR target auto-detect.
package pgbackrest

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// newRestoreCmd implements `pgbackrest --stanza=<n> restore [--target=<t>] [--type=<form>]`.
//
// Target form auto-detection:
//
//	hex with /            → --to-lsn
//	starts with "name:"   → --to-name
//	otherwise time-ish    → --to "<value>"
//
// Operators can pin the form explicitly with --type=time|lsn|name
// (mirroring pgBackRest's flag).  --target-action maps 1:1 to the
// native --to-action; pgBackRest's "promote" / "shutdown" / "pause"
// names already match.
func newRestoreCmd() *cobra.Command {
	c := &cobra.Command{
		Use:           "restore",
		Short:         "Restore the latest backup to the PG data directory",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRestore(globalArgs)
		},
	}
	c.Flags().StringVar(&globalArgs.target, "target", "",
		"recovery target (auto-detected as time / LSN / name)")
	c.Flags().StringVar(&globalArgs.targetAction, "target-action", "",
		"action when target reached: promote | shutdown | pause")
	c.Flags().StringVar(&globalArgs.targetType, "type", "",
		"target form override: time | lsn | name | immediate")
	return c
}

func runRestore(a pgbackrestArgs) error {
	native, warnings, err := mapToNativeArgs("restore", a)
	if err != nil {
		return err
	}
	emitWarnings(warnings)

	// pgBackRest restores into PG's data directory (PGDATA);
	// pg_hardstorage's restore needs --target.  We honour
	// PGDATA env if set; otherwise the operator must supply
	// PGDATA the way every PG tool already requires.
	target := os.Getenv("PGDATA")
	if target == "" {
		return fmt.Errorf(
			"pg-hardstorage-pgbackrest: restore: PGDATA env var must be set " +
				"(pgBackRest uses it implicitly; the shim forwards it as --target)")
	}

	out := []string{native[0], a.stanza, "latest"}
	out = append(out, native[1:]...)
	out = append(out, "--target", target)

	if a.target != "" {
		form, value := classifyTarget(a.target, a.targetType)
		switch form {
		case "lsn":
			out = append(out, "--to-lsn", value)
		case "name":
			out = append(out, "--to-name", value)
		case "time":
			out = append(out, "--to", value)
		case "immediate":
			// Native maps "immediate" to no PITR target — recovery
			// stops at the end of the base backup, which is the
			// default already.
		}
	}
	if a.targetAction != "" {
		out = append(out, "--to-action", strings.ToLower(a.targetAction))
	}

	if rc := dispatchNative(out); rc != 0 {
		return fmt.Errorf("pg-hardstorage-pgbackrest: restore: native CLI exited %d", rc)
	}
	return nil
}

// classifyTarget picks the recovery-target form.  An explicit
// --type wins; otherwise we sniff the value: `0/3000028` is an
// LSN, anything starting `name:` (or simply containing letters
// without spaces or colons) is a named restore point, every
// other shape is treated as a time string.
func classifyTarget(value, explicit string) (form, normalised string) {
	switch strings.ToLower(explicit) {
	case "lsn":
		return "lsn", value
	case "name":
		return "name", strings.TrimPrefix(value, "name:")
	case "time":
		return "time", value
	case "immediate":
		return "immediate", ""
	}
	// Auto-detect.
	if strings.HasPrefix(value, "name:") {
		return "name", strings.TrimPrefix(value, "name:")
	}
	if looksLikeLSN(value) {
		return "lsn", value
	}
	return "time", value
}

// looksLikeLSN returns true for the canonical PG LSN form
// `<hex>/<hex>`.  Bare hex with a slash is the only thing
// PG accepts and pgBackRest follows the same shape.
func looksLikeLSN(s string) bool {
	slash := strings.Index(s, "/")
	if slash <= 0 || slash == len(s)-1 {
		return false
	}
	for _, r := range s {
		switch {
		case r == '/':
			continue
		case r >= '0' && r <= '9':
			continue
		case r >= 'a' && r <= 'f':
			continue
		case r >= 'A' && r <= 'F':
			continue
		default:
			return false
		}
	}
	return true
}
