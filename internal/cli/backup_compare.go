// backup_compare.go — 'backup compare' CLI verb: diffs two backups (file + chunk overlap).
package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// newBackupCompareCmd implements `pg_hardstorage backup compare
// <deployment> <id1> <id2>` — the manifest-diff surface for
// answering "what's different between these two backups?".
//
// Operationally:
//   - "I took an incremental and the chunk count looks high — what
//     files actually changed?"
//   - "Backup B is 10 GB bigger than backup A on disk; was it
//     supposed to be?"
//   - Audit-style "did the rotation happen as expected — does Tuesday's
//     full backup share the chunks we expect with Monday's?"
//
// The compare reads both manifests (verifying signatures) and
// runs `backup.Compare` over them. No chunk fetches needed; this
// is metadata-only and fast even on huge backups.
func newBackupCompareCmd() *cobra.Command {
	var (
		repoURL string
		topN    int
	)
	c := &cobra.Command{
		Use:   "compare <deployment> <backup-id-a> <backup-id-b>",
		Short: "Diff two backups — file & chunk overlap, size delta, top-N changed files",
		Long: `compare reads two manifests and reports their differences:

  - File-level: which files exist in only A / only B / both / changed
  - Chunk-level: shared / A-only / B-only chunks (CAS-deduped, by hash)
  - Logical bytes: total per side + delta + shared (unchanged) bytes
  - Top-N file deltas: the largest |size| changes, ranked

This is metadata-only — chunks are NOT fetched. Fast even on
huge backups.

Use cases:
  - Incremental forensics ("why is this backup so big?")
  - Dedup-ratio surprises ("is the chain still sharing chunks?")
  - Rotation audit ("does the new full share the right chunks?")

Both backups must exist and verify against the local keyring.
Tombstoned (soft-deleted) manifests are not directly readable —
pass --include-deleted equivalents are NOT supported here; use
the per-side view via ` + "`" + `backup show --include-deleted` + "`" + ` first.`,
		Args:         cobra.ExactArgs(3),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			deployment, idA, idB := args[0], args[1], args[2]
			return runBackupCompare(cmd, deployment, idA, idB, repoURL, topN)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().IntVar(&topN, "top-n", backup.DefaultCompareTopN,
		"cap the top-file-deltas list at this many entries (default 20)")
	return c
}

func runBackupCompare(cmd *cobra.Command, deployment, idA, idB, repoURL string, topN int) error {
	d := DispatcherFrom(cmd)
	if idA == idB {
		return output.NewError("usage.same_id",
			"backup compare: <backup-id-a> and <backup-id-b> must differ").Wrap(output.ErrUsage)
	}
	if topN < 0 {
		return output.NewError("usage.bad_flag",
			"backup compare: --top-n must be ≥ 0").Wrap(output.ErrUsage)
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := backup.NewManifestStore(sp)
	manifestA, err := store.Read(cmd.Context(), deployment, idA, verifier)
	if err != nil {
		return mapCompareReadError("a", deployment, idA, err)
	}
	manifestB, err := store.Read(cmd.Context(), deployment, idB, verifier)
	if err != nil {
		return mapCompareReadError("b", deployment, idB, err)
	}

	res, err := backup.Compare(manifestA, manifestB, backup.CompareOptions{TopN: topN})
	if err != nil {
		return output.NewError("backup.compare.failed",
			fmt.Sprintf("backup compare: %v", err)).Wrap(err)
	}

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(backupCompareBody{
		Deployment: deployment,
		Result:     res,
	}))
}

// mapCompareReadError translates a manifest-read failure into
// the structured CLI errors the operator expects. tagged "a"
// or "b" so the operator sees which side failed.
func mapCompareReadError(side, deployment, backupID string, err error) error {
	if errors.Is(err, storage.ErrNotFound) {
		return output.NewError("notfound.backup",
			fmt.Sprintf("backup compare: %s side: %s/%s not found",
				strings.ToUpper(side), deployment, backupID)).
			WithSuggestion(&output.Suggestion{
				Human:   "list available backups with `pg_hardstorage list " + deployment + "`",
				Command: "pg_hardstorage list " + deployment,
			}).Wrap(err)
	}
	if errors.Is(err, backup.ErrTombstoned) {
		return output.NewError("notfound.backup_tombstoned",
			fmt.Sprintf("backup compare: %s side: %s/%s is tombstoned (soft-deleted)",
				strings.ToUpper(side), deployment, backupID)).
			WithSuggestion(&output.Suggestion{
				Human: "compare reads the live manifest path; run `backup show --include-deleted` for inspection or `backup undelete` to resurrect.",
			}).Wrap(err)
	}
	return output.NewError("backup.compare.read_failed",
		fmt.Sprintf("backup compare: %s side: %v", side, err)).Wrap(err)
}

// backupCompareBody is the v1-stable result shape. Embeds the
// ComparisonResult plus the deployment name (since the result
// itself only carries per-side BackupID).
type backupCompareBody struct {
	Deployment string                   `json:"deployment"`
	Result     *backup.ComparisonResult `json:"result"`
}

// WriteText renders the comparison as compact tables: per-side
// summary, file-class counts, chunk-class counts, top-N deltas.
func (b backupCompareBody) WriteText(w io.Writer) error {
	r := b.Result
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "compare %s/%s ↔ %s\n", b.Deployment, r.A.BackupID, r.B.BackupID)

	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  SIDE\tBACKUP ID\tTYPE\tFILES\tUNIQUE CHUNKS\tLOGICAL")
	fmt.Fprintf(tw, "  A\t%s\t%s\t%d\t%d (%s)\t%s\n",
		r.A.BackupID, r.A.Type, r.A.FileCount,
		r.A.UniqueChunkCount, humanBytes(r.A.UniqueChunkBytes),
		humanBytes(r.A.LogicalBytes))
	fmt.Fprintf(tw, "  B\t%s\t%s\t%d\t%d (%s)\t%s\n",
		r.B.BackupID, r.B.Type, r.B.FileCount,
		r.B.UniqueChunkCount, humanBytes(r.B.UniqueChunkBytes),
		humanBytes(r.B.LogicalBytes))
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(bw)
	fmt.Fprintf(bw, "FILES\n")
	fmt.Fprintf(bw, "  in_both:   %d (%d changed)\n", r.FileCounts.InBoth, r.FileCounts.Changed)
	fmt.Fprintf(bw, "  only_in_a: %d\n", r.FileCounts.OnlyInA)
	fmt.Fprintf(bw, "  only_in_b: %d\n", r.FileCounts.OnlyInB)

	fmt.Fprintf(bw, "CHUNKS (CAS-deduped)\n")
	fmt.Fprintf(bw, "  shared:  %d (%s)\n", r.ChunkCounts.Shared, humanBytes(r.ChunkCounts.SharedBytes))
	fmt.Fprintf(bw, "  a_only:  %d (%s)\n", r.ChunkCounts.AOnly, humanBytes(r.ChunkCounts.AOnlyBytes))
	fmt.Fprintf(bw, "  b_only:  %d (%s)\n", r.ChunkCounts.BOnly, humanBytes(r.ChunkCounts.BOnlyBytes))

	fmt.Fprintf(bw, "LOGICAL BYTES\n")
	fmt.Fprintf(bw, "  a:      %s\n", humanBytes(r.LogicalBytes.A))
	fmt.Fprintf(bw, "  b:      %s\n", humanBytes(r.LogicalBytes.B))
	fmt.Fprintf(bw, "  delta:  %+d (%s)\n", r.LogicalBytes.Delta, humanBytes(r.LogicalBytes.Delta))
	fmt.Fprintf(bw, "  shared: %s (unchanged files: same path + chunk set)\n",
		humanBytes(r.LogicalBytes.Shared))

	if len(r.TopFileDeltas) > 0 {
		fmt.Fprintf(bw, "\nTOP %d FILE DELTA(S)\n", len(r.TopFileDeltas))
		dt := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(dt, "  CLASS\tDELTA\tPATH")
		for _, d := range r.TopFileDeltas {
			fmt.Fprintf(dt, "  %s\t%+d\t%s\n", d.Class, d.Delta, d.Path)
		}
		if err := dt.Flush(); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
