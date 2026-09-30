// restore.go — pgBackRest shim verb: `pgbackrest restore [--target] [--type]` → native restore with PITR target auto-detect.
package pgbackrest

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/compat/internal/pitrtime"
)

// newRestoreCmd implements `pgbackrest --stanza=<n> restore [--target=<t>] [--type=<form>]`.
//
// --type follows pgBackRest's recovery types:
//
//	default (or unset)  → --to-latest: replay every archived segment
//	immediate           → stop at consistency (the native default)
//	time | lsn | name   → --to / --to-lsn / --to-name with --target
//	standby             → native `standby create` (hot standby)
//	xid | preserve | none → refused: no native equivalent
//
// Without --type, a --target value is auto-detected:
//
//	hex with /            → --to-lsn
//	starts with "name:"   → --to-name
//	otherwise time-ish    → --to "<value>"
//
// --target-action maps 1:1 to the native --to-action; pgBackRest's
// "promote" / "shutdown" / "pause" names already match.
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
		"recovery type: default | immediate | time | lsn | name | standby")

	// pgBackRest's own explicit forms. Real pgbackrest spells a
	// recovery target as --target-time / --target-lsn / --target-name;
	// --target + --type is the older shape. Only the older shape
	// parsed, so a cron written against the documented flags died with
	// "unknown flag: --target-lsn".
	c.Flags().StringVar(&globalArgs.targetTime, "target-time", "",
		"recovery target time (pgBackRest form of --target --type=time)")
	c.Flags().StringVar(&globalArgs.targetLSN, "target-lsn", "",
		"recovery target LSN (pgBackRest form of --target --type=lsn)")
	c.Flags().StringVar(&globalArgs.targetName, "target-name", "",
		"recovery target restore-point name (pgBackRest form of --target --type=name)")
	return c
}

// refusedRestoreTypes are pgBackRest recovery types with no native
// equivalent. Each used to fall through the form switch silently: xid
// was sniffed as a TIME, preserve / none became an immediate restore
// that promoted. Refusing names the gap instead.
var refusedRestoreTypes = map[string]string{
	"xid": "native recovery has no transaction-ID target; find the commit's LSN " +
		"(pg_waldump, or pg_current_wal_lsn() logged near the transaction) or its time " +
		"and pass --type=lsn / --type=time",
	"preserve": "native restore always writes its own recovery settings; restore with " +
		"--type=immediate or --type=default, then edit postgresql.auto.conf",
	"none": "pg_hardstorage backups are taken online and the WAL needed to reach " +
		"consistency lives in the repository, so a restore without restore_command " +
		"cannot start; use --type=immediate",
}

// resolveTarget folds pgBackRest's two target spellings and --type
// into the one (form, value) pair runRestore emits. form is "" for
// pgBackRest's default (replay to end of archive), or one of
// immediate / standby / time / lsn / name. The explicit --target-*
// flags win over --target/--type, and setting more than one is a
// usage error rather than a silent pick.
func resolveTarget(a pgbackrestArgs) (form, value string, err error) {
	typ := strings.ToLower(a.targetType)
	switch typ {
	case "", "default", "immediate", "standby", "time", "lsn", "name":
	default:
		if hint, ok := refusedRestoreTypes[typ]; ok {
			return "", "", refuseFlag("--type="+a.targetType, hint)
		}
		return "", "", refuseFlag("--type="+a.targetType,
			"supported recovery types are default, immediate, time, lsn, name and standby")
	}

	explicit := 0
	if a.targetTime != "" {
		form, value, explicit = "time", a.targetTime, explicit+1
	}
	if a.targetLSN != "" {
		form, value, explicit = "lsn", a.targetLSN, explicit+1
	}
	if a.targetName != "" {
		form, value, explicit = "name", a.targetName, explicit+1
	}
	if explicit > 1 {
		return "", "", fmt.Errorf(
			"pg-hardstorage-pgbackrest: restore: --target-time, --target-lsn and --target-name are mutually exclusive")
	}
	if explicit == 1 {
		if a.target != "" {
			return "", "", fmt.Errorf(
				"pg-hardstorage-pgbackrest: restore: pass either --target/--type or one of --target-time/--target-lsn/--target-name, not both")
		}
		if typ != "" && typ != form {
			return "", "", fmt.Errorf(
				"pg-hardstorage-pgbackrest: restore: --type=%s conflicts with --target-%s", typ, form)
		}
		return form, value, nil
	}
	if a.target == "" {
		switch typ {
		case "", "default":
			return "", "", nil
		case "immediate", "standby":
			return typ, "", nil
		default:
			return "", "", fmt.Errorf(
				"pg-hardstorage-pgbackrest: restore: --type=%s requires --target", typ)
		}
	}
	switch typ {
	case "default", "immediate", "standby":
		return "", "", fmt.Errorf(
			"pg-hardstorage-pgbackrest: restore: --target has no meaning with --type=%s", typ)
	}
	form, value = classifyTarget(a.target, typ)
	return form, value, nil
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

	form, value, err := resolveTarget(a)
	if err != nil {
		return err
	}

	if form == "standby" {
		// pgBackRest --type=standby writes standby.signal: the node
		// stays in recovery following the archive and is never
		// promoted. A plain native restore arms recovery_target=
		// 'immediate' + promote, which would turn the would-be replica
		// into a second, divergent primary — so dispatch the native
		// hot-standby builder instead.
		if a.targetAction != "" {
			return fmt.Errorf(
				"pg-hardstorage-pgbackrest: restore: --target-action has no meaning with --type=standby (a standby is never promoted automatically)")
		}
		out := []string{"standby", "create", a.stanza,
			"--deployment", a.stanza, "--backup", "latest", "--target", target}
		out = append(out, native[1:]...)
		if rc := dispatchNative(out); rc != 0 {
			return fmt.Errorf("pg-hardstorage-pgbackrest: restore: native CLI exited %d", rc)
		}
		return nil
	}

	out := []string{native[0], a.stanza, "latest"}
	out = append(out, native[1:]...)
	out = append(out, "--target", target)

	switch form {
	case "lsn":
		out = append(out, "--to-lsn", value)
	case "name":
		out = append(out, "--to-name", value)
	case "time":
		if err := pitrtime.RequireExplicitZone("pgBackRest",
			"pg-hardstorage-pgbackrest: restore: target time", value); err != nil {
			return err
		}
		out = append(out, "--to", value)
	case "immediate":
		// Native's own default: recovery_target='immediate', stop at
		// the backup's consistency point.
	case "":
		// pgBackRest's default type replays every archived segment and
		// then promotes. The native default stops at the backup's
		// consistency point instead, which silently discards all WAL
		// archived after the backup — the most common DR restore would
		// lose everything since the last backup.
		out = append(out, "--to-latest")
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
