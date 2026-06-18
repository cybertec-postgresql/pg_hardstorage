// wal_gap_purge.go — CLI surface for purging recorded WAL gap markers.
package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// newWalGapPurgeCmd implements `pg_hardstorage wal gap-purge
// <deployment> [--orphans|--all] [--dry-run] [--yes]`.
//
// Two modes:
//
//	--orphans  remove gap records whose timeline is no longer
//	           referenced by any live manifest in the
//	           deployment. The routine cleanup that pairs with
//	           `repo gc --apply` semantically — gap records
//	           point at a TLI nothing else cares about; PITR
//	           within the gap is impossible regardless because
//	           there's no backup to seed it.
//
//	--all      remove every gap record for the deployment.
//	           Used when a deployment is being wiped end-to-end
//	           (operator removed it from config). PurgeOrphans
//	           refuses --all semantics specifically because
//	           an empty liveTimelines would over-reap; this
//	           path is the explicit opt-in.
//
// Refuses without --dry-run OR --yes (same UX as
// `hold purge-expired`).
func newWalGapPurgeCmd() *cobra.Command {
	var (
		repoURL string
		orphans bool
		all     bool
		dryRun  bool
		yes     bool
	)
	c := &cobra.Command{
		Use:   "gap-purge <deployment>",
		Short: "Reap orphan WAL-gap records (or every record for a wiped deployment)",
		Long: `gap-purge removes WAL-gap records under
` + "`" + `wal/<deployment>/gaps/` + "`" + ` that are no longer operationally
meaningful.

Modes:

  --orphans  Remove records whose timeline is no longer present in
             any live manifest. The cleanup that pairs with
             ` + "`" + `repo gc --apply` + "`" + ` — a gap on a TLI nothing else
             references is forensic noise; PITR within the gap is
             impossible regardless (no backup to seed from).

  --all      Remove every gap record for the deployment. Used when
             the deployment was wiped end-to-end. --orphans refuses
             this case to avoid accidental over-reap; --all is the
             explicit opt-in.

Each removal is audit-emitted (` + "`" + `wal.gap_purged` + "`" + ` action) so the
chain has a per-record forensic trail. --dry-run emits no audit
events.

  pg_hardstorage wal gap-purge db1 --orphans --dry-run
  pg_hardstorage wal gap-purge db1 --orphans --yes
  pg_hardstorage wal gap-purge db1 --all --yes`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWalGapPurge(cmd, args[0], repoURL, orphans, all, dryRun, yes)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().BoolVar(&orphans, "orphans", false,
		"reap records whose timeline is no longer referenced by any live manifest")
	c.Flags().BoolVar(&all, "all", false,
		"reap every gap record for the deployment (deployment-wipe path)")
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"preview the records that would be removed (no mutations, no audit emits)")
	c.Flags().BoolVar(&yes, "yes", false,
		"confirm bulk removal — required unless --dry-run is set")
	return c
}

func runWalGapPurge(cmd *cobra.Command, deployment, repoURL string, orphans, all, dryRun, yes bool) error {
	d := DispatcherFrom(cmd)
	if orphans == all {
		return output.NewError("usage.bad_flag",
			"wal gap-purge: exactly one of --orphans / --all must be set").
			WithSuggestion(&output.Suggestion{
				Human: "--orphans is the routine cleanup (TLI no longer live); --all is the deployment-wipe path. They're distinct because PurgeOrphans refuses an empty liveTimelines to avoid over-reap.",
			}).Wrap(output.ErrUsage)
	}
	if !dryRun && !yes {
		return output.NewError("aborted.confirmation_required",
			"wal gap-purge: refusing bulk removal without --yes").
			WithSuggestion(&output.Suggestion{
				Human: "preview first with --dry-run to see what would be removed; re-run with --yes once you're sure.",
			})
	}

	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	if !dryRun {
		if err := assertRepoWritable(cmd.Context(), sp, "wal gap-purge"); err != nil {
			return err
		}
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}

	store := backup.NewManifestStore(sp)
	gs := gapstate.New(sp)
	var (
		removed []gapstate.Record
		mode    string
	)
	switch {
	case all:
		mode = "all"
		removed, err = gs.PurgeAll(cmd.Context(), deployment, dryRun)
	default:
		mode = "orphans"
		live, lerr := collectLiveTimelines(cmd.Context(), store, deployment, verifier)
		if lerr != nil {
			return output.NewError("wal.gap_purge_scan_failed",
				fmt.Sprintf("wal gap-purge --orphans: collect live timelines: %v", lerr)).Wrap(lerr)
		}
		if len(live) == 0 {
			// No live manifests at all — refuse rather than
			// silently reap everything. Operator should pass
			// --all explicitly if that's what they mean.
			return output.NewError("conflict.no_live_manifests",
				fmt.Sprintf("wal gap-purge --orphans: deployment %q has no live manifests", deployment)).
				WithSuggestion(&output.Suggestion{
					Human:   "the deployment has no live manifests, so 'orphan' has no meaning. If you intended to wipe gap records, pass --all. Otherwise check whether the deployment is correctly registered and has at least one un-tombstoned backup.",
					Command: "pg_hardstorage wal gap-purge " + deployment + " --all --yes --repo " + repoURL,
				})
		}
		removed, err = gs.PurgeOrphans(cmd.Context(), deployment, live, dryRun)
	}
	if err != nil {
		return output.NewError("wal.gap_purge_failed",
			fmt.Sprintf("wal gap-purge: %v (%d removed before failure)", err, len(removed))).
			WithSuggestion(&output.Suggestion{
				Human: "the operation is naturally idempotent — re-run with the same arguments after fixing the underlying error to drain the rest.",
			}).Wrap(err)
	}

	// One audit event per removed record (mirror of
	// hold.purge_expired). Skipped on dry-run.
	if !dryRun && len(removed) > 0 {
		auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
		for _, r := range removed {
			ev := &audit.Event{
				Action: "wal.gap_purged",
				Subject: audit.Subject{
					Deployment: r.Deployment,
					Repo:       repoURL,
				},
				Body: map[string]any{
					"mode":        mode,
					"timeline":    r.Timeline,
					"slot_name":   r.SlotName,
					"slot_role":   r.SlotRole,
					"gap_bytes":   r.GapBytes,
					"gap_start":   r.GapStartLSN,
					"gap_end":     r.GapEndLSN,
					"detected_at": r.DetectedAt.Format(time.RFC3339),
				},
			}
			_ = auditStore.Append(cmd.Context(), ev)
		}
	}

	rows := make([]walGapPurgeRow, 0, len(removed))
	for _, r := range removed {
		rows = append(rows, walGapPurgeRow{
			Timeline:    r.Timeline,
			SlotName:    r.SlotName,
			SlotRole:    r.SlotRole,
			GapBytes:    r.GapBytes,
			GapStartLSN: r.GapStartLSN,
			GapEndLSN:   r.GapEndLSN,
			DetectedAt:  r.DetectedAt.UTC().Format(time.RFC3339),
		})
	}
	// Stable order (timeline, detected_at) for deterministic
	// JSON across runs.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Timeline != rows[j].Timeline {
			return rows[i].Timeline < rows[j].Timeline
		}
		return rows[i].DetectedAt < rows[j].DetectedAt
	})
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(walGapPurgeBody{
		Deployment: deployment,
		Mode:       mode,
		DryRun:     dryRun,
		Count:      len(rows),
		Removed:    rows,
	}))
}

// collectLiveTimelines walks the deployment's live manifests
// (signature-verified, non-tombstoned) and returns the set of
// distinct Timeline values. Used by `--orphans` to decide which
// gap records are still operationally meaningful.
func collectLiveTimelines(ctx context.Context, store *backup.ManifestStore, deployment string, verifier *backup.Verifier) (map[uint32]struct{}, error) {
	out := make(map[uint32]struct{})
	for m, err := range store.List(ctx, deployment, verifier) {
		if err != nil {
			// Signature failures don't define orphan-status:
			// silently skip — same posture as
			// pickLatestBackup.
			continue
		}
		if m == nil {
			continue
		}
		out[m.Timeline] = struct{}{}
	}
	return out, nil
}

// walGapPurgeBody is the v1-stable result. DryRun reflects the
// mode; Mode is "orphans" or "all"; Removed is the per-record
// forensic shape.
type walGapPurgeBody struct {
	Deployment string           `json:"deployment"`
	Mode       string           `json:"mode"`
	DryRun     bool             `json:"dry_run,omitempty"`
	Count      int              `json:"count"`
	Removed    []walGapPurgeRow `json:"removed"`
}

type walGapPurgeRow struct {
	Timeline    uint32 `json:"timeline"`
	SlotName    string `json:"slot_name"`
	SlotRole    string `json:"slot_role,omitempty"`
	GapBytes    uint64 `json:"gap_bytes"`
	GapStartLSN string `json:"gap_start_lsn"`
	GapEndLSN   string `json:"gap_end_lsn"`
	DetectedAt  string `json:"detected_at"`
}

// WriteText renders the gap-purge result as human-readable text to w,
// distinguishing dry-run from a real removal.
func (b walGapPurgeBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.Count == 0 {
		switch {
		case b.DryRun:
			fmt.Fprintf(bw, "no gap records to purge (dry-run, mode=%s)\n", b.Mode)
		default:
			fmt.Fprintf(bw, "no gap records to purge (mode=%s) — nothing to do\n", b.Mode)
		}
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	verb := "✓ Removed"
	if b.DryRun {
		verb = "Would remove"
	}
	fmt.Fprintf(bw, "%s %d gap record(s) for %s (mode=%s)\n", verb, b.Count, b.Deployment, b.Mode)
	for _, r := range b.Removed {
		fmt.Fprintf(bw, "  TLI %d  slot=%s  gap=%d bytes  range=%s..%s  detected=%s\n",
			r.Timeline, r.SlotName, r.GapBytes, r.GapStartLSN, r.GapEndLSN, r.DetectedAt)
	}
	if b.DryRun {
		fmt.Fprintln(bw, "Re-run with --yes to perform the removal (each record is audit-emitted).")
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// silence import warning for storage when not directly used in
// future variants; placeholder is cheap.
var _ = storage.ErrNotFound
