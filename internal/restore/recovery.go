// recovery.go — Recovery: writes recovery.signal + recovery_target_* GUCs to enable PITR.
package restore

import (
	"errors"
	"fmt"
	stdfs "io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/walfetchcmd"
)

// Recovery configures point-in-time recovery (PITR) on a restored
// data directory.
//
// PostgreSQL 12 dropped the legacy `recovery.conf` file; the modern
// way to mark a data directory as "recover, don't start as primary"
// is to drop a marker file `recovery.signal` (presence-only — the
// content is irrelevant beyond optional comments) and to set the
// `recovery_target_*` GUCs in the cluster's regular config files.
//
// We append a managed block to `postgresql.auto.conf` in the target
// directory:
//
//	# --- pg_hardstorage managed block (PITR) ---
//	restore_command = '...'
//	recovery_target_lsn = '...'             # one of these three
//	recovery_target_time = '...'
//	recovery_target_name = '...'
//	recovery_target_inclusive = true|false
//	recovery_target_action = 'pause'|'promote'|'shutdown'
//	recovery_target_timeline = 'latest'|'<n>'
//	# --- end pg_hardstorage managed block ---
//
// At most one of TargetLSN / TargetTime / TargetName may be set. If
// all three are zero, recovery replays to the end of available WAL
// (still useful: it activates restore_command, so PG fetches WAL
// from our repo via `pg_hardstorage wal fetch`).
type Recovery struct {
	// Enable activates the recovery block. Without it Recovery is a
	// no-op even if other fields are populated.
	Enable bool

	// TargetLSN is a PostgreSQL LSN string ("0/3000028"). Recovery
	// stops once this LSN is reached (subject to Inclusive).
	TargetLSN string

	// TargetTime is the wall-clock instant after which recovery
	// stops. Zero means unset. Always emitted in UTC for unambiguous
	// PG parsing (PG accepts the offset as part of the literal).
	TargetTime time.Time

	// TargetName is a PG named-restore-point label. Created with
	// SELECT pg_create_restore_point('label') on the source cluster
	// before the events to recover up to.
	TargetName string

	// Inclusive: when true, recovery stops just after the target.
	// PG's default is true; we propagate that as the field's default
	// so the user sees the actual rendered GUC.
	Inclusive bool

	// Action: what PG does when the target is reached.
	//   "pause"     — recovery pauses; user manually promotes (default, safest)
	//   "promote"   — PG ends recovery immediately and accepts writes
	//   "shutdown"  — PG shuts down cleanly, ready to be re-examined
	// Empty string is treated as "pause".
	Action string

	// Timeline: "latest" (default) or an explicit TLI as a stringified
	// uint (e.g. "3"). "latest" follows the most recent TLI in the
	// archive — the safest choice in almost all cases.
	Timeline string

	// RestoreCommand is the literal `restore_command` GUC value. The
	// CLI assembles this from "<binary-path> wal fetch <deployment>
	// %f %p --repo <url>".
	RestoreCommand string

	// StandbyMode requests a hot standby instead of point-in-time
	// recovery. When true, WriteRecoveryFiles drops `standby.signal`
	// (and NOT recovery.signal) so PG stays in recovery mode forever,
	// applying any WAL `restore_command` provides. Used by `standby
	// create` to bring up a continuously-updating read replica fed
	// by the backup pipeline.
	//
	// StandbyMode and Target* are mutually exclusive: a hot standby
	// has no stop point.
	StandbyMode bool

	// SkipGapCheck disables the+ WAL-gap pre-flight that
	// would otherwise refuse a TargetLSN restore landing in a
	// known-failover gap (or warn for time/name targets). Off
	// by default — set explicitly when the operator has
	// validated the restore safety some other way (recovery
	// drill on synthetic data, manual WAL splice from a
	// secondary archive, etc.).
	//
	// The override is logged as an audit event with severity
	// Notice so post-incident review can see who made the
	// choice. The Coordinator's wal_gap_advisory event still
	// fires when relevant — operators see the warning, the
	// override just says "I've seen it, proceed anyway."
	SkipGapCheck bool
}

// IsTargetSet reports whether one of the recovery_target_* fields
// names a specific stop point. False means "recover to end of WAL".
func (r Recovery) IsTargetSet() bool {
	return r.TargetLSN != "" || !r.TargetTime.IsZero() || r.TargetName != ""
}

// WriteRecoveryFiles writes the PITR managed block to
// `postgresql.auto.conf` and then drops `recovery.signal` in target.
//
// Order matters for partial-failure safety: GUCs are written FIRST,
// signal LAST. If the signal write fails after the GUCs land, PG
// starts NORMALLY (no recovery) with our GUCs sitting in auto.conf —
// harmless at non-recovery startup (PG ignores recovery_target_*
// when not in recovery mode). If we did the inverse (signal first,
// GUCs second) and the GUC write failed, PG would enter recovery
// without the right targets and replay all available WAL — possibly
// past the operator's intended PITR target. The current ordering
// fails safe; the inverse fails dangerous.
func WriteRecoveryFiles(target string, r Recovery) error {
	if !r.Enable {
		return nil
	}
	if err := validateRecovery(r); err != nil {
		return err
	}

	autoPath := filepath.Join(target, "postgresql.auto.conf")
	block := buildAutoConfBlock(r)
	if err := appendAutoConf(autoPath, block); err != nil {
		return err
	}

	sigName := "recovery.signal"
	sigBody := "# pg_hardstorage PITR — presence of this file marks the cluster for recovery\n"
	if r.StandbyMode {
		// standby.signal keeps PG in recovery indefinitely. PG with
		// JUST standby.signal (no recovery.signal) treats the cluster
		// as a hot standby — readable, never promoted unless an
		// explicit pg_promote() runs.
		sigName = "standby.signal"
		sigBody = "# pg_hardstorage hot standby — presence of this file keeps the cluster in recovery, applying WAL via restore_command\n"
	}
	sigPath := filepath.Join(target, sigName)
	// fsutil.WriteFileSync flushes the file inode AND fsyncs the
	// parent dir.  These signal files are the entire control surface
	// for "did PG enter recovery on next start?" — losing the file
	// (or losing its visibility) after a crash silently changes the
	// recovery posture.
	if err := fsutil.WriteFileSync(sigPath, []byte(sigBody), 0o600); err != nil {
		return fmt.Errorf("recovery: write %s: %w", sigName, err)
	}
	return nil
}

// WriteAutoRecovery primes a non-PITR restored data dir so PG can
// reach consistency on first start: drops `standby.signal`, appends
// `recovery_target='immediate'` + `recovery_target_action='promote'`
// + a `restore_command` invoking `<agent> wal fetch <deployment> %f %p
// --repo <repoURL>` to postgresql.auto.conf.
//
// Why every restore needs this:
// `pg_hardstorage backup` snapshots PGDATA while the source cluster
// is live, so pg_control records a checkpoint LSN whose trailing
// WAL segment isn't bundled into the restored pg_wal/.  Without a
// recovery signal + restore_command, PG either FATALs at startup
// ("could not locate required checkpoint record") or sits in standby
// forever waiting for the missing segment.  This was the dominant
// failure cluster in soak testing (10 of 16 soak cells + test-wal-stream-suite).
//
// agentBinary defaults to os.Executable() — the agent invoking
// restore.  Empty repoURL or deployment → restore_command line is
// skipped (cluster still gets the signal + recovery_target, useful
// for offline / synthesised-manifest tests).
func WriteAutoRecovery(target, deployment, repoURL string) error {
	autoPath := filepath.Join(target, "postgresql.auto.conf")
	var b strings.Builder
	b.WriteString("\n# --- pg_hardstorage managed block (auto-recovery) ---\n")
	b.WriteString("recovery_target = 'immediate'\n")
	b.WriteString("recovery_target_action = 'promote'\n")
	if repoURL != "" && deployment != "" {
		// os.Executable() is how we discover the agent binary to embed
		// in restore_command.  If it fails (or returns empty) we CANNOT
		// silently drop the line: without restore_command PG enters
		// recovery via the standby.signal below and then waits forever
		// for a WAL segment nobody will ever hand it — the exact
		// "standby waits forever" failure this function exists to
		// prevent.  Surface it as a hard error so the caller sees the
		// gap instead of shipping a cluster that never reaches
		// consistency.  (Callers that deliberately want a signal-only
		// data dir pass empty repoURL/deployment, handled above.)
		bin, err := os.Executable()
		if err != nil {
			return fmt.Errorf("auto-recovery: cannot resolve agent binary for restore_command (os.Executable: %w); "+
				"the restored cluster would enter recovery and wait forever for WAL with no way to fetch it", err)
		}
		if bin == "" {
			return errors.New("auto-recovery: os.Executable returned an empty path; " +
				"cannot build restore_command, and the restored cluster would wait forever for WAL")
		}
		// quoteSQL not naked '%s': walfetchcmd.Build returns a
		// shell command that contains single-quoted args (POSIX
		// safety for repo URLs with `&`), so wrapping in raw PG
		// single quotes here would produce nested apostrophes
		// PG parses as `'sh -c "'<break>` → "syntax error near
		// token /".  The PITR-config path (configPITR) routes
		// through quoteSQL for the same reason.
		fmt.Fprintf(&b, "restore_command = %s\n",
			quoteSQL(walfetchcmd.Build(bin, deployment, repoURL)))
	}
	if err := appendAutoConf(autoPath, b.String()); err != nil {
		return err
	}
	sigPath := filepath.Join(target, "standby.signal")
	if err := fsutil.WriteFileSync(sigPath, []byte(
		"# pg_hardstorage auto-recovery — presence of this file gates startup into recovery, applying WAL via restore_command\n",
	), 0o600); err != nil {
		return fmt.Errorf("auto-recovery: write standby.signal: %w", err)
	}
	return nil
}

// validateRecovery enforces the at-most-one-target rule and normalises
// optional fields. RestoreCommand is mandatory because without it PG
// has no way to fetch WAL during recovery.
func validateRecovery(r Recovery) error {
	if r.RestoreCommand == "" {
		return errors.New("recovery: RestoreCommand is required when Enable=true")
	}
	set := 0
	if r.TargetLSN != "" {
		set++
	}
	if !r.TargetTime.IsZero() {
		set++
	}
	if r.TargetName != "" {
		set++
	}
	if set > 1 {
		return errors.New("recovery: at most one of TargetLSN, TargetTime, TargetName may be set")
	}
	if r.StandbyMode && set > 0 {
		return errors.New("recovery: StandbyMode is mutually exclusive with TargetLSN/TargetTime/TargetName (a hot standby has no stop point)")
	}
	switch r.Action {
	case "", "pause", "promote", "shutdown":
		// ok
	default:
		return fmt.Errorf("recovery: Action %q is not one of pause|promote|shutdown", r.Action)
	}
	if r.TargetLSN != "" && !LooksLikeLSN(r.TargetLSN) {
		return fmt.Errorf("recovery: TargetLSN %q is not a valid PG LSN (expected hex form like 0/3000028)", r.TargetLSN)
	}
	if r.Timeline != "" && r.Timeline != "latest" {
		n, err := strconv.ParseUint(r.Timeline, 10, 32)
		if err != nil || n == 0 {
			return fmt.Errorf("recovery: Timeline %q is not \"latest\" or a positive integer", r.Timeline)
		}
	}
	return nil
}

// LooksLikeLSN lives in recovery_lsn_shape.go so the mutation-testing
// harness can swap in a deliberately-broken variant under the
// mutation_lsn_shape_loose build tag without disturbing this file.

// buildAutoConfBlock renders the managed block as a string. Every
// emitted GUC value is single-quote-quoted; embedded single quotes
// are doubled per PostgreSQL config-file rules.
func buildAutoConfBlock(r Recovery) string {
	var b strings.Builder
	b.WriteString("\n# --- pg_hardstorage managed block (PITR) ---\n")
	fmt.Fprintf(&b, "restore_command = %s\n", quoteSQL(r.RestoreCommand))

	if r.TargetLSN != "" {
		fmt.Fprintf(&b, "recovery_target_lsn = %s\n", quoteSQL(r.TargetLSN))
	}
	if !r.TargetTime.IsZero() {
		// PG accepts ISO-8601-like timestamps with offset. Emit UTC
		// with explicit "+00" so the parsing is unambiguous.
		fmt.Fprintf(&b, "recovery_target_time = %s\n",
			quoteSQL(r.TargetTime.UTC().Format("2006-01-02 15:04:05.999999-07")))
	}
	if r.TargetName != "" {
		fmt.Fprintf(&b, "recovery_target_name = %s\n", quoteSQL(r.TargetName))
	}

	fmt.Fprintf(&b, "recovery_target_inclusive = %t\n", r.Inclusive)

	action := r.Action
	if action == "" {
		action = "pause"
	}
	fmt.Fprintf(&b, "recovery_target_action = %s\n", quoteSQL(action))

	timeline := r.Timeline
	if timeline == "" {
		timeline = "latest"
	}
	fmt.Fprintf(&b, "recovery_target_timeline = %s\n", quoteSQL(timeline))

	b.WriteString("# --- end pg_hardstorage managed block ---\n")
	return b.String()
}

// quoteSQL renders s as a PostgreSQL string literal for embedding in
// postgresql.auto.conf.
//
// CRITICAL: PostgreSQL's configuration-file lexer (src/.../guc-file.l)
// is NOT the SQL lexer. standard_conforming_strings does NOT apply
// here. Inside a single-quoted .conf value the lexer processes C-style
// backslash escapes — verified empirically against PG: `\\`→`\`,
// `\t`→tab, `\n`→newline, `\r`→CR, and a lone backslash before the
// closing quote (`\'`) escapes it, leaving the string unterminated so
// PG FATALs at config load and the restored cluster refuses to start.
//
// Therefore we escape:
//
//   - `\`        — DOUBLED, and FIRST. A literal backslash that isn't
//     doubled is silently dropped or fused with the next char
//     (`C:\Users` → `C:Users`); a trailing backslash makes PG refuse
//     to start. Must run before the control-char escapes below so the
//     backslashes THEY introduce are not re-doubled. (The old code
//     left `\` unescaped on a since-disproven "standard_conforming_
//     strings makes `\` literal" assumption — that governs SQL, not
//     .conf files.)
//   - `'`        — doubled (the standard .conf quote escape).
//   - `\n`, `\r` — replaced with their backslash-letter form so a raw
//     newline/CR can't terminate the directive line; PG un-escapes
//     them back to the raw byte when loading the GUC.
//   - `\x00`     — replaced with `\0` (a raw NUL would truncate the
//     value).
func quoteSQL(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	s = strings.ReplaceAll(s, "\x00", `\0`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return "'" + s + "'"
}

// appendAutoConf reads the existing postgresql.auto.conf (if any),
// concatenates block, and atomically rewrites the file via
// fsutil.WriteFileAtomic.
//
// History — audit: the previous implementation opened with
// O_CREATE|O_APPEND, WriteString'd the block, and fsync'd.  That's
// data-durable for the existing file's bytes (f.Sync() handles the
// inode), but two durability gaps remained:
//
//  1. When O_CREATE actually creates the file, the parent
//     directory's dentry list is not flushed.  A power loss after
//     f.Sync() returns but before the parent fsync can lose the
//     file's existence even though its bytes are durable.  PG
//     then comes up without the recovery block — silent
//     mis-recovery.
//  2. POSIX guarantees O_APPEND-mode writes are atomic only up to
//     PIPE_BUF (typically 4 KiB).  Recovery blocks today are well
//     under that, but a future caller adding more GUCs could
//     cross the boundary; an interleaved reader would then see a
//     half-applied block.
//
// fsutil.WriteFileAtomic (tmp + fsync + rename + syncDir) closes
// both gaps.  The cost is one extra read of the existing file —
// negligible for a config file that's typically a few hundred
// bytes.
func appendAutoConf(path, block string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, stdfs.ErrNotExist) {
		return fmt.Errorf("recovery: read postgresql.auto.conf: %w", err)
	}
	merged := append(existing, []byte(block)...)
	if err := fsutil.WriteFileAtomic(path, merged, 0o600); err != nil {
		return fmt.Errorf("recovery: write postgresql.auto.conf: %w", err)
	}
	return nil
}
