// wal_prune.go — CLI surface for pruning archived WAL behind the oldest live backup.
package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newWalPruneCmd implements `pg_hardstorage wal prune <deployment>`.
//
// What it does: deletes WAL segment manifests whose end_lsn is
// strictly older than the oldest non-tombstoned backup's start_lsn
// for the deployment. Those segments can never participate in any
// recovery using the kept backups, so they're dust.
//
// What it does NOT do: touch chunks. The deleted manifests' chunks
// become orphans for `repo gc` to sweep. This is the same split
// `rotate` uses for backup manifests — prune the references, let
// GC handle the bytes.
//
// Default mode is --dry-run; pass --apply to actually delete.
// Same safe-by-default posture every other destructive op honours.
//
// Flag --keep-since <duration> enforces the plan's "keep_wal_days"
// semantics on top of the LSN-based primary rule: even when a
// segment is below the LSN frontier, if its CreatedAt is newer
// than now()-keep-since it's preserved. Default 0 = no time floor.
//
// Cron-friendly: counters are top-level integers,
// duration_ms is a fixed unit, started_at / stopped_at are RFC 3339.
func newWalPruneCmd() *cobra.Command {
	var (
		repoURL        string
		apply          bool
		keepSince      time.Duration
		tombstoneGrace time.Duration
	)
	c := &cobra.Command{
		Use:   "prune <deployment>",
		Short: "Delete WAL segment manifests no kept backup can use for recovery",
		Long: `Walk wal/<deployment>/<TLI>/ and delete segment manifests
whose end_lsn is below the oldest non-tombstoned backup's
start_lsn (the "frontier"). Segments at or after the frontier are
needed for PITR and stay.

Default mode is --dry-run; pass --apply to actually delete.

--keep-since <duration> adds a time-based floor: segments whose
CreatedAt is newer than now()-keep-since are preserved even when
their end_lsn is below the LSN frontier. Use this to enforce a
"keep at least N days of WAL" policy on top of the LSN-based
primary rule (the plan's keep_wal_days).

Chunks are NOT deleted by this command. Run 'pg_hardstorage repo
gc --apply' afterwards to reclaim the now-orphan chunk bytes.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWalPrune(cmd, args[0], repoURL, apply, keepSince, tombstoneGrace)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().BoolVar(&apply, "apply", false,
		"actually delete the candidate segments (default: dry-run)")
	c.Flags().DurationVar(&keepSince, "keep-since", 0,
		"keep WAL segments newer than now-<duration> regardless of the LSN rule (e.g. 14d → 336h)")
	c.Flags().DurationVar(&tombstoneGrace, "tombstone-grace", repo.DefaultTombstoneGracePeriod,
		"keep a just-tombstoned backup's WAL until its tombstone ages past this, so a backup undelete within the window can still recover it (matches repo gc); pass 0 to disable")
	return c
}

func runWalPrune(cmd *cobra.Command, deployment, repoURL string, apply bool, keepSince, tombstoneGrace time.Duration) error {
	d := DispatcherFrom(cmd)
	if keepSince < 0 {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("wal prune: --keep-since must be >= 0; got %v", keepSince)).Wrap(output.ErrUsage)
	}

	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	if apply {
		if err := assertRepoWritable(cmd.Context(), sp, "wal prune --apply"); err != nil {
			return err
		}
	}

	// Mirror repo gc: an explicit --tombstone-grace 0 means "no grace"
	// (immediate exclusion); the underlying API spells that as a negative.
	graceForCall := tombstoneGrace
	if graceForCall == 0 {
		graceForCall = -1
	}
	opts := repo.WALPruneOptions{
		Deployment:     deployment,
		DryRun:         !apply,
		TombstoneGrace: graceForCall,
	}
	if keepSince > 0 {
		opts.KeepFloorTime = time.Now().Add(-keepSince)
	}
	res, err := repo.WALPrune(cmd.Context(), sp, opts)
	if err != nil {
		return output.NewError("repo.wal_prune.failed",
			fmt.Sprintf("wal prune: %v", err)).Wrap(err)
	}

	body := walPruneBody{
		WALPruneResult: *res,
		KeepSince:      keepSince,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// walPruneBody is the v1-stable Result body. Cron-friendly counters;
// the embedded WALPruneResult carries the per-segment detail.
type walPruneBody struct {
	repo.WALPruneResult
	// KeepSince echoes the operator-supplied --keep-since value so
	// monitoring sees what time-floor was active during the run.
	// Zero means no floor (LSN-only retention).
	KeepSince time.Duration `json:"keep_since,omitempty"`
}

// WriteText renders the prune outcome — frontier, retained range, and
// deletion counts — as human-readable text to w.
func (b walPruneBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "wal prune — %s\n", b.Deployment)
	if b.DryRun {
		fmt.Fprintln(bw, "  (dry-run — nothing deleted)")
	}
	if b.FrontierBackupID == "" {
		fmt.Fprintln(bw, "  No non-tombstoned backup found — nothing to prune.")
		fmt.Fprintln(bw, "  (Take a backup first; WALPrune needs a frontier.)")
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	fmt.Fprintf(bw, "  Frontier:        %s @ start_lsn %s\n",
		b.FrontierBackupID, b.FrontierLSN)
	if b.KeepSince > 0 {
		fmt.Fprintf(bw, "  Keep-since:      %s (floor at %s)\n",
			b.KeepSince, b.KeepFloor)
	}
	fmt.Fprintf(bw, "  Segments:        %d considered · %d deleted · %d kept · %d failed\n",
		b.SegmentsConsidered, b.SegmentsDeleted, b.SegmentsKept, b.SegmentsFailed)
	if b.SegmentsKeptByFloor > 0 {
		fmt.Fprintf(bw, "  Kept by floor:   %d (newer than --keep-since cutoff)\n",
			b.SegmentsKeptByFloor)
	}
	fmt.Fprintf(bw, "  Bytes %s: %s\n",
		map[bool]string{true: "would-delete", false: "deleted     "}[b.DryRun],
		humanBytes(b.BytesDeleted))
	fmt.Fprintf(bw, "  Duration:        %d ms\n", b.DurationMS)
	if b.SegmentsFailed > 0 {
		fmt.Fprintf(bw, "  ✗ %d failure(s):\n", b.SegmentsFailed)
		for _, f := range b.Failures {
			fmt.Fprintf(bw, "    %s — %s\n", f.Key, f.Err)
		}
	} else if b.SegmentsDeleted == 0 {
		fmt.Fprintln(bw, "  ✓ no candidates (all WAL is at-or-after the frontier)")
	} else {
		fmt.Fprintln(bw, "  ✓ pruning clean")
		if b.DryRun {
			fmt.Fprintln(bw, "  Re-run with --apply to actually delete.")
		} else {
			fmt.Fprintln(bw, "  Run 'pg_hardstorage repo gc --apply' to reclaim the orphan chunk bytes.")
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
