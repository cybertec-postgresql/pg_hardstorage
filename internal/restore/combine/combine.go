// Package combine implements the PG 17+ incremental restore pipeline:
// chain discovery (walk parent_backup_id back to the full anchor),
// pre-flight checks for pg_combinebackup, and the subprocess
// invocation that produces a self-contained data dir from a chain.
//
// The companion piece is restore.Restore, which detects an
// incremental leaf manifest, materialises every chain link into a
// staging dir, calls Run here, and finalises the merged output.
//
// Why a separate package: chain walking is pure metadata work; the
// pg_combinebackup invocation is a thin subprocess shim. Keeping them
// out of the main restore.go keeps the latter focused on the file
// materialisation pipeline and lets us test the chain logic against
// fake manifests without standing up a real PG.
package combine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// MaxChainDepth bounds the parent-walk recursion. PG itself imposes
// no hard cap on incremental depth, but real-world chains are
// shallow (full + a handful of increments before the next full
// lands). 100 is a paranoid upper bound that catches malformed or
// cyclic ParentBackupID fields without making legitimate chains
// harder.
const MaxChainDepth = 100

// Chain is the restore-ordered list of manifests:
// chain[0] is the full backup; chain[len-1] is the requested leaf.
// A chain of length 1 is a single full backup that does NOT need
// pg_combinebackup; the caller should fall through to the regular
// full-restore path in that case.
type Chain []*backup.Manifest

// IsIncremental reports whether the chain has at least one
// incremental link (len > 1).
func (c Chain) IsIncremental() bool { return len(c) > 1 }

// Build resolves the chain backward from leafID by walking
// ParentBackupID until the full anchor is reached. The returned
// slice is ordered [full, inc1, ..., leaf].
//
// Errors:
//   - notfound.backup if any link in the chain is missing
//   - chain.broken_tombstoned if any link has been tombstoned
//   - chain.cycle if a duplicate is observed in the walk
//   - chain.too_deep if MaxChainDepth is exceeded
//   - chain.no_full_anchor if the chain bottoms out at a non-full
//
// Signature failures bubble up as the underlying ParseAndVerify error
// from ManifestStore.Read — the operator sees the pre-existing
// signature taxonomy rather than a chain-specific wrapping.
func Build(ctx context.Context, sp storage.StoragePlugin, deployment, leafID string, verifier *backup.Verifier) (Chain, error) {
	if sp == nil {
		return nil, errors.New("combine: nil StoragePlugin")
	}
	if deployment == "" {
		return nil, errors.New("combine: deployment is required")
	}
	if leafID == "" {
		return nil, errors.New("combine: leafID is required")
	}
	store := backup.NewManifestStore(sp)

	var chain Chain
	seen := make(map[string]struct{})
	cur := leafID
	for i := 0; cur != ""; i++ {
		if i >= MaxChainDepth {
			return nil, output.NewError("chain.too_deep",
				fmt.Sprintf("restore: incremental chain longer than %d entries (suspected cycle or malformed parent_backup_id)", MaxChainDepth))
		}
		if _, dup := seen[cur]; dup {
			return nil, output.NewError("chain.cycle",
				fmt.Sprintf("restore: cycle detected in parent_backup_id chain at %q", cur))
		}
		seen[cur] = struct{}{}

		m, err := store.Read(ctx, deployment, cur, verifier)
		if err != nil {
			if errors.Is(err, backup.ErrTombstoned) {
				return nil, output.NewError("chain.broken_tombstoned",
					fmt.Sprintf("restore: chain link %q is tombstoned; the chain cannot be restored", cur)).
					WithSuggestion(&output.Suggestion{
						Human: "an ancestor in the incremental chain was soft-deleted. Either un-tombstone it (remove the manifest.json.tombstone marker) or restore from a different backup. Chain-aware retention introduced in this release should normally prevent this state — investigate how the tombstone landed.",
					}).Wrap(err)
			}
			return nil, fmt.Errorf("restore chain: read %s/%s: %w", deployment, cur, err)
		}
		chain = append(chain, m)
		cur = m.ParentBackupID
	}

	// Reverse so chain[0] is the full anchor.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	if len(chain) > 0 {
		anchor := chain[0]
		if anchor.Type != backup.BackupTypeFull && anchor.Type != backup.BackupTypeSnapshot {
			return nil, output.NewError("chain.no_full_anchor",
				fmt.Sprintf("restore: chain anchor %q has type %q, not %q; an incremental chain must root at a full backup",
					anchor.BackupID, anchor.Type, backup.BackupTypeFull))
		}
		// Uniformity: every link must belong to the SAME cluster (system
		// identifier) and the SAME PG major, and only the anchor may be a
		// full/snapshot backup. pg_combinebackup ultimately enforces all
		// of this — but only AFTER pg_hardstorage has materialised the
		// entire chain (potentially many TB) into staging, and it fails
		// with a cryptic mid-merge error. Checking here fails FAST, before
		// any I/O, with a precise reason. These checks cannot reject a
		// valid chain: a real chain shares one cluster + one major, and a
		// full/snapshot is always the anchor (it has no parent, so the
		// parent-walk stops there).
		anchorMajor := pgMajor(anchor.PGVersion)
		for i := 1; i < len(chain); i++ {
			link := chain[i]
			if link.Type != backup.BackupTypeIncremental {
				return nil, output.NewError("chain.non_incremental_link",
					fmt.Sprintf("restore: chain link %q (position %d of %d) has type %q; only the anchor may be a full/snapshot backup",
						link.BackupID, i, len(chain)-1, link.Type))
			}
			if link.SystemIdentifier != anchor.SystemIdentifier {
				return nil, output.NewError("chain.system_identifier_mismatch",
					fmt.Sprintf("restore: chain link %q has system_identifier %s but the anchor %q has %s — the chain spans DIFFERENT clusters and cannot be combined",
						link.BackupID, link.SystemIdentifier, anchor.BackupID, anchor.SystemIdentifier))
			}
			// Compare PG majors only when both are known; tolerate a
			// legacy manifest with an unset PGVersion rather than
			// false-rejecting it (pg_combinebackup will still catch a real
			// incompatibility).
			if lm := pgMajor(link.PGVersion); anchorMajor != 0 && lm != 0 && lm != anchorMajor {
				return nil, output.NewError("chain.pg_major_mismatch",
					fmt.Sprintf("restore: chain link %q is PG major %d but the anchor %q is PG major %d — pg_combinebackup requires a single PostgreSQL major across the whole chain",
						link.BackupID, lm, anchor.BackupID, anchorMajor))
			}
		}
	}
	return chain, nil
}

// pgMajor extracts the PostgreSQL major version from a server_version_num
// (e.g. 170002 → 17). Returns 0 when v is unknown/unset.
func pgMajor(v int) int {
	if v <= 0 {
		return 0
	}
	return v / 10000
}

// DiscoverPGCombineBackup probes PATH for pg_combinebackup. Used by
// pre-flight: a missing binary should fail BEFORE any chain
// materialisation, not in the middle of it.
//
// Returns an absolute path. Mirrors sandbox.DiscoverPGTools — same
// posture so an operator can debug PATH problems with the same
// mental model whether they're hitting partial-restore or
// chain-restore.
//
// IMPORTANT: pg_combinebackup is documented by upstream to be
// supported only for backups taken with the SAME major version of
// PostgreSQL.  Callers that know the backup's PG major should use
// DiscoverPGCombineBackupForMajor instead — this function is the
// version-blind fallback for callers that don't have that context
// (preflight checks, doctor, etc.).
func DiscoverPGCombineBackup() (string, error) {
	p, err := exec.LookPath("pg_combinebackup")
	if err != nil {
		return "", err
	}
	return p, nil
}

// DiscoverPGCombineBackupForMajor returns the absolute path to a
// pg_combinebackup binary whose own PG major matches `pgMajor`.
// Required for chain restores because PG's pg_combinebackup is
// version-locked: running PG 18's pg_combinebackup on PG 17 backup
// data fails with "CRC is incorrect" on every chain link's
// pg_control because the control-file layout differs between
// majors.
//
// Lookup order:
//
//  1. PG_COMBINEBACKUP_<MAJOR> env var (operator override; absolute
//     path expected).
//  2. Well-known per-major install paths used by upstream PGDG
//     packages on the distros we ship.
//  3. exec.LookPath("pg_combinebackup") IFF its --version reports
//     pgMajor.  Returns an error otherwise so a wrong-version
//     binary on PATH doesn't silently corrupt the restore.
//
// On any failure the error carries a structured suggestion the CLI
// can surface to the operator (install the matching PGDG package
// or set PG_COMBINEBACKUP_<MAJOR>=<path>).
func DiscoverPGCombineBackupForMajor(pgMajor int) (string, error) {
	if pgMajor <= 0 {
		// Caller didn't supply a usable major (e.g. legacy
		// manifest without pg_version).  Fall back to the
		// version-blind discovery and trust the operator's PATH.
		return DiscoverPGCombineBackup()
	}
	if env := os.Getenv(fmt.Sprintf("PG_COMBINEBACKUP_%d", pgMajor)); env != "" {
		if abs, err := filepath.Abs(env); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs, nil
			}
		}
	}
	candidates := []string{
		// PGDG RHEL/Fedora layout.
		fmt.Sprintf("/usr/pgsql-%d/bin/pg_combinebackup", pgMajor),
		// PGDG Debian/Ubuntu layout.
		fmt.Sprintf("/usr/lib/postgresql/%d/bin/pg_combinebackup", pgMajor),
		// Homebrew layout.
		fmt.Sprintf("/opt/homebrew/opt/postgresql@%d/bin/pg_combinebackup", pgMajor),
		fmt.Sprintf("/usr/local/opt/postgresql@%d/bin/pg_combinebackup", pgMajor),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	// Last resort: PATH binary, but only if its major matches.
	// A version-mismatched binary on PATH is worse than no binary
	// at all — see the "CRC is incorrect" failure on PG-17 data
	// fed to PG-18's pg_combinebackup.
	if p, err := exec.LookPath("pg_combinebackup"); err == nil {
		if major, verr := readPGCombineBackupMajor(p); verr == nil && major == pgMajor {
			return p, nil
		}
	}
	return "", fmt.Errorf("pg_combinebackup matching PG %d not found "+
		"(checked PG_COMBINEBACKUP_%d, /usr/pgsql-%d/bin, "+
		"/usr/lib/postgresql/%d/bin, homebrew paths, and PATH)",
		pgMajor, pgMajor, pgMajor, pgMajor)
}

// readPGCombineBackupMajor invokes `<path> --version` and parses the
// PG major out of the output. Returns 0 on parse failure so callers
// treat it as "unknown" and skip the binary.
//
// Expected output shape (PG 17+):
//
//	pg_combinebackup (PostgreSQL) 17.5
//	pg_combinebackup (PostgreSQL) 18.0 (Debian 18.0-1.pgdg12+1)
//
// Anything else returns 0 + an error so the caller falls through to
// the next discovery candidate.
func readPGCombineBackupMajor(path string) (int, error) {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return 0, err
	}
	// Split off everything before the first decimal-major token
	// that follows "PostgreSQL".
	const marker = "PostgreSQL) "
	i := strings.Index(string(out), marker)
	if i < 0 {
		return 0, fmt.Errorf("unexpected --version output: %s", strings.TrimSpace(string(out)))
	}
	tail := string(out)[i+len(marker):]
	dot := strings.IndexAny(tail, ".\n \t")
	if dot < 0 {
		dot = len(tail)
	}
	majorStr := strings.TrimSpace(tail[:dot])
	var major int
	if _, err := fmt.Sscanf(majorStr, "%d", &major); err != nil || major <= 0 {
		return 0, fmt.Errorf("parse major from %q: %v", majorStr, err)
	}
	return major, nil
}

// CombineOptions configures one pg_combinebackup invocation.
type CombineOptions struct {
	// PGCombineBackupPath is the absolute path to pg_combinebackup.
	// When empty Run consults DiscoverPGCombineBackup. Operators
	// pin it explicitly when their PATH is shared with multiple
	// PG majors and they want a specific binary.
	PGCombineBackupPath string

	// InputDirs is the ordered list of backup directories. First
	// entry is the full backup; subsequent entries are incrementals
	// in the order they were taken. Must contain at least 2 dirs
	// (one full + at least one incremental); a single-dir chain
	// doesn't need pg_combinebackup.
	InputDirs []string

	// OutputDir is the merged-output target. pg_combinebackup
	// refuses an existing non-empty dir; the caller is responsible
	// for cleanup before calling Run.
	OutputDir string

	// ExtraArgs are appended to the pg_combinebackup command line
	// before the input dirs. Useful for advanced flags (e.g.
	// --tablespace-mapping) without growing this struct's surface.
	// Empty for the common case.
	ExtraArgs []string

	// Stderr captures pg_combinebackup's stderr. Nil discards.
	Stderr io.Writer
}

// Run invokes pg_combinebackup. Blocks until the process exits.
//
// On success the caller has a complete, restorable data dir at
// OutputDir; the staging input dirs are untouched (the caller
// removes them).
//
// Pre-flight: a missing binary surfaces structured
// preflight.pg_combinebackup_missing with a postgresql-client hint;
// invalid input shapes surface combine.bad_inputs (CLI maps to
// ExitMisuse). Subprocess failures wrap exit code + stderr in a
// combine.subprocess_failed error so an operator can copy/paste
// the underlying pg_combinebackup error verbatim.
func Run(ctx context.Context, opts CombineOptions) error {
	if opts.PGCombineBackupPath == "" {
		p, err := DiscoverPGCombineBackup()
		if err != nil {
			return output.NewError("preflight.pg_combinebackup_missing",
				fmt.Sprintf("restore: pg_combinebackup not on PATH: %v", err)).
				WithSuggestion(&output.Suggestion{
					Human: "install postgresql-client (Debian/Ubuntu) or postgresql (RHEL/Fedora) so pg_combinebackup is on PATH; PG 17+ is required for incremental restore",
				}).
				Wrap(err)
		}
		opts.PGCombineBackupPath = p
	}
	if len(opts.InputDirs) < 2 {
		return output.NewError("combine.bad_inputs",
			fmt.Sprintf("restore: pg_combinebackup needs at least 2 input dirs (full + 1 incremental); got %d",
				len(opts.InputDirs))).
			Wrap(output.ErrUsage)
	}
	if opts.OutputDir == "" {
		return output.NewError("combine.bad_inputs",
			"restore: pg_combinebackup OutputDir is required").
			Wrap(output.ErrUsage)
	}

	// Argv shape: pg_combinebackup [extra args] -o OUT INPUT1 INPUT2 ...
	// We put -o + OutputDir first because pg_combinebackup is
	// conservative about positional ambiguity and putting the flag
	// up front is the documented happy path.
	args := make([]string, 0, 2+len(opts.ExtraArgs)+len(opts.InputDirs))
	args = append(args, opts.ExtraArgs...)
	args = append(args, "-o", opts.OutputDir)
	args = append(args, opts.InputDirs...)

	cmd := exec.CommandContext(ctx, opts.PGCombineBackupPath, args...)
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	}
	if err := cmd.Run(); err != nil {
		return output.NewError("combine.subprocess_failed",
			fmt.Sprintf("restore: pg_combinebackup failed: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "the pg_combinebackup subprocess returned non-zero. Check the captured stderr above for the underlying PG error; a common cause is a missing backup_manifest in one of the staging dirs (every chain link must include PGBackupManifest, which only landed — older backups can't anchor a chain)",
			}).
			Wrap(err)
	}
	return nil
}
