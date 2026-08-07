// backup_undelete.go — CLI surface for restoring tombstoned backups, with optional chunk-presence check.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newBackupUndeleteCmd implements
// `pg_hardstorage backup undelete <deployment> <backup-id>...`.
//
// Pairs with `backup delete`. The cascade we shipped for
// SoftDelete deliberately stops short of an automated unwind —
// re-resurrecting a chain via parent_backup_id traversal of
// tombstoned manifests is more graph-walk than an operator typically
// wants in the recovery moment. Instead, the audit trail and the
// cascade response slice already give the operator the exact
// deletion-order list (`cascade_deleted`); they pass that list back
// to undelete and the chain is restored.
//
// Order of un-tombstoning has no semantic constraint at the storage
// layer (un-marking doesn't break the chain-protection invariant
// either way), so we process IDs in argv order. The output reports
// each ID's outcome (restored vs already-live) for an audit-friendly
// trail.
func newBackupUndeleteCmd() *cobra.Command {
	var (
		repoURL     string
		reason      string
		checkChunks bool
		skipMissing bool
		force       bool
	)
	c := &cobra.Command{
		Use:   "undelete <deployment> <backup-id> [<backup-id>...]",
		Short: "Resurrect one or more soft-deleted backups (removes tombstone marker)",
		Long: `undelete removes the tombstone marker for the named backup(s),
making them visible again to ` + "`" + `backup list` + "`" + ` and ` + "`" + `restore` + "`" + `.

Use this to recover from an over-aggressive delete or cascade
before chunk-GC runs. The window is bounded: once
` + "`" + `repo gc --apply` + "`" + ` has reclaimed the chunks the manifest
references, undelete REFUSES with conflict.chunks_missing (the
store fails closed) unless --force is passed to recover the
metadata of a backup that can no longer restore.

Idempotent. An undelete of a manifest that's already live is a
no-op; no error, no audit entry. Multiple IDs may be passed in
ANY order: within the batch, ancestors are resurrected before
their descendants automatically (the store refuses an
incremental under a tombstoned ancestor), so the leaf-first
cascade_deleted slice works verbatim. Outcomes are reported in
the order given.

Pairs with ` + "`" + `backup delete --cascade` + "`" + `: the cascade response's
` + "`" + `cascade_deleted` + "`" + ` slice (or the equivalent audit body
field) is exactly what you pass back to ` + "`" + `undelete` + "`" + ` to
unwind a wrong cascade.

Restorability pre-flight (--check-chunks):
  --check-chunks  Stat every chunk referenced by each manifest
                  BEFORE removing its tombstone. Refuses with
                  conflict.chunks_missing if any required chunk
                  is absent — telling you the manifest can't be
                  meaningfully restored even if you resurrect it.
                  Same primitive that backs verify --existence-only.
  --skip-missing  When --check-chunks fires on multi-ID input,
                  skip the manifests with missing chunks and
                  undelete the rest. Without --skip-missing, the
                  whole operation refuses up-front (atomic
                  semantics — same posture as cascade delete).`,
		Args:         cobra.MinimumNArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			deployment := args[0]
			ids := args[1:]
			return runBackupUndelete(cmd, deployment, ids, repoURL, reason, checkChunks, skipMissing, force)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&reason, "reason", "",
		"free-form reason captured in the audit chain (recommended for forensics)")
	c.Flags().BoolVar(&checkChunks, "check-chunks", false,
		"run the missing-chunk check as an up-front BATCH pre-pass, so a multi-ID undelete refuses atomically before touching anything (and --skip-missing can select the survivors). The per-ID safety does not depend on this flag: the store fails closed on missing chunks regardless.")
	c.Flags().BoolVar(&skipMissing, "skip-missing", false,
		"when --check-chunks fails on some IDs, undelete the rest (default: refuse the whole batch)")
	c.Flags().BoolVar(&force, "force", false,
		"resurrect even when the manifest's chunks are gone, to recover the metadata of an un-restorable backup (forensic use). Without it, undelete FAILS CLOSED on missing chunks — the store refuses with conflict.chunks_missing whether or not --check-chunks was passed.")
	return c
}

// orderAncestorsFirst returns ids reordered so that, within the batch,
// every backup precedes its descendants. Order among independent ids —
// and everything else about the sequence — follows the caller's
// original order, so single-ID and unrelated-ID calls are untouched.
//
// The store refuses to resurrect an incremental under a tombstoned
// ancestor, and `backup delete --cascade` hands back its
// cascade_deleted slice LEAF-FIRST. This reordering is what makes
// "pass that slice straight back" — the unwind the documentation
// promises — actually work.
//
// Best-effort by design: a manifest that cannot be read contributes no
// edge and stays in caller order, and the per-ID loop then surfaces the
// real error for it. A parent outside the batch contributes no edge
// either — if that parent is still tombstoned, no order makes the
// resurrection safe, and the store's refusal stands.
func orderAncestorsFirst(ctx context.Context, store *backup.ManifestStore, deployment string, ids []string, verifier *backup.Verifier) []string {
	inBatch := make(map[string]bool, len(ids))
	for _, id := range ids {
		inBatch[id] = true
	}
	parent := make(map[string]string, len(ids))
	for _, id := range ids {
		m, _, err := store.ReadIncludingTombstoned(ctx, deployment, id, verifier)
		if err != nil || m == nil {
			continue
		}
		if m.ParentBackupID != "" && inBatch[m.ParentBackupID] {
			parent[id] = m.ParentBackupID
		}
	}
	if len(parent) == 0 {
		return ids
	}
	ordered := make([]string, 0, len(ids))
	emitted := make(map[string]bool, len(ids))
	// Stable Kahn walk: each pass emits, in caller order, every id
	// whose in-batch parent has been emitted. Bounded by len(ids)
	// passes; anything left after that (a cycle, which commit-time
	// validation makes impossible) falls back to caller order.
	for pass := 0; pass < len(ids); pass++ {
		progressed := false
		for _, id := range ids {
			if emitted[id] {
				continue
			}
			if p, has := parent[id]; has && !emitted[p] {
				continue
			}
			ordered = append(ordered, id)
			emitted[id] = true
			progressed = true
		}
		if !progressed || len(ordered) == len(ids) {
			break
		}
	}
	for _, id := range ids {
		if !emitted[id] {
			ordered = append(ordered, id)
		}
	}
	return ordered
}

func runBackupUndelete(cmd *cobra.Command, deployment string, ids []string, repoURL, reason string, checkChunks, skipMissing, force bool) error {
	d := DispatcherFrom(cmd)
	if deployment == "" {
		return output.NewError("usage.missing_arg",
			"backup undelete: deployment is required").Wrap(output.ErrUsage)
	}
	if len(ids) == 0 {
		return output.NewError("usage.missing_arg",
			"backup undelete: at least one backup-id is required").Wrap(output.ErrUsage)
	}
	if skipMissing && !checkChunks {
		return output.NewError("usage.bad_flag",
			"backup undelete: --skip-missing requires --check-chunks").
			WithSuggestion(&output.Suggestion{
				Human: "--skip-missing changes the behaviour of --check-chunks (skip vs refuse on missing); without --check-chunks there's nothing to skip.",
			}).Wrap(output.ErrUsage)
	}
	for _, id := range ids {
		if id == "" {
			return output.NewError("usage.missing_arg",
				"backup undelete: backup-id must be non-empty").Wrap(output.ErrUsage)
		}
	}

	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	if err := assertRepoWritable(cmd.Context(), sp, "backup undelete"); err != nil {
		return err
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}

	store := backup.NewManifestStore(sp)
	auditReason := reason
	if auditReason == "" {
		auditReason = "operator-initiated"
	}

	// Restorability pre-flight when --check-chunks is set.
	// We read the (still-tombstoned) manifest body via
	// ReadIncludingTombstoned, then Stat every unique chunk
	// hash via backup.CheckChunkExistence. The check itself
	// is read-only; mutations only happen below if the check
	// passes (or --skip-missing is set).
	chunkChecks := make([]chunkCheckRow, 0, len(ids))
	skipDueToMissing := make(map[string]struct{})
	// --force skips the restorability pre-flight entirely (it
	// resurrects metadata even when chunks are gone), so the
	// --check-chunks pre-pass is moot under --force.
	if checkChunks && !force {
		for _, id := range ids {
			m, _, rerr := store.ReadIncludingTombstoned(cmd.Context(), deployment, id, verifier)
			if rerr != nil {
				return output.NewError("backup.undelete.read_failed",
					fmt.Sprintf("backup undelete --check-chunks: read manifest %s/%s: %v", deployment, id, rerr)).Wrap(rerr)
			}
			res, cerr := backup.CheckChunkExistence(cmd.Context(), sp, m)
			if cerr != nil {
				return output.NewError("backup.undelete.check_failed",
					fmt.Sprintf("backup undelete --check-chunks: stat chunks for %s/%s: %v", deployment, id, cerr)).Wrap(cerr)
			}
			row := chunkCheckRow{
				BackupID:    id,
				TotalUnique: res.TotalUnique,
				Present:     res.Present,
				MissingN:    len(res.Missing),
			}
			row.Missing = summarizeMissingHashes(res.Missing, 5)
			chunkChecks = append(chunkChecks, row)
			if !res.AllPresent() {
				skipDueToMissing[id] = struct{}{}
			}
		}

		// Atomic-batch posture: if ANY manifest has missing
		// chunks and --skip-missing is NOT set, refuse the
		// whole batch up-front. Same posture as
		// SoftDeleteCascade — partial state on a multi-ID
		// op is strictly worse than no op.
		if !skipMissing && len(skipDueToMissing) > 0 {
			refused := make([]string, 0, len(skipDueToMissing))
			for id := range skipDueToMissing {
				refused = append(refused, id)
			}
			sortStringsStable(refused)
			return output.NewError("conflict.chunks_missing",
				fmt.Sprintf("backup undelete --check-chunks: %d of %d manifest(s) have missing chunks: %s",
					len(refused), len(ids), strings.Join(refused, ", "))).
				WithSuggestion(&output.Suggestion{
					Human:   "the named manifests reference chunks that are no longer in the repo (chunk-GC reclaimed them, or the manifest was committed with chunks that never wrote). Resurrecting them would produce un-restorable backups. Run with --skip-missing to undelete the rest of the batch instead.",
					Command: "pg_hardstorage backup undelete " + deployment + " <id...> --repo " + repoURL + " --check-chunks --skip-missing",
				})
		}
	}

	// Process ancestors before descendants, whatever order the caller
	// passed.
	//
	// The store refuses to resurrect an incremental whose ancestor is
	// still tombstoned (a safety added after this command), and
	// `backup delete --cascade` returns its cascade_deleted slice
	// LEAF-FIRST — the correct order for deletion is exactly the wrong
	// order for resurrection. The undelete docs tell the operator that
	// slice "is exactly what you pass back to unwind a wrong cascade",
	// and until this reordering existed, doing precisely that failed on
	// the first ID. An operator unwinding a wrong cascade is following
	// instructions verbatim under stress; the batch has to work AS
	// RETURNED.
	//
	// Only edges WITHIN the batch are reordered. An ancestor that is
	// tombstoned but absent from the batch still refuses, correctly —
	// no order can make that resurrection safe. Results are reported in
	// the caller's argv order regardless of processing order, so the
	// output shape does not depend on chain topology.
	ordered := orderAncestorsFirst(cmd.Context(), store, deployment, ids, verifier)

	outcomeByID := make(map[string]backupUndeleteOutcome, len(ids))
	restoredIDs := make([]string, 0, len(ids))
	for _, id := range ordered {
		if _, skip := skipDueToMissing[id]; skip {
			outcomeByID[id] = backupUndeleteOutcome{
				BackupID:      id,
				Restored:      false,
				ChunksMissing: true,
			}
			continue
		}
		var (
			restored bool
			uerr     error
		)
		if force {
			restored, uerr = store.UndeleteForce(cmd.Context(), deployment, id)
		} else {
			// Safe by default: Undelete fails closed if the
			// manifest's chunks have been swept, leaving the
			// tombstone in place.
			restored, uerr = store.Undelete(cmd.Context(), deployment, id)
		}
		if uerr != nil {
			var cm *backup.UndeleteChunksMissingError
			if errors.As(uerr, &cm) {
				return output.NewError("conflict.chunks_missing",
					fmt.Sprintf("backup undelete: %s/%s references %d chunk(s) no longer in the repo (%d missing); resurrecting it would be un-restorable",
						deployment, id, cm.TotalUnique, len(cm.Missing))).
					WithSuggestion(&output.Suggestion{
						Human:   "the manifest's chunks were reclaimed by chunk-GC (or never wrote). Pass --force to recover the metadata anyway (the resurrected backup will NOT restore), or restore from a different backup.",
						Command: "pg_hardstorage backup undelete " + deployment + " " + id + " --repo " + repoURL + " --force",
					}).Wrap(uerr)
			}
			return output.NewError("backup.undelete.failed",
				fmt.Sprintf("backup undelete: %s/%s: %v", deployment, id, uerr)).Wrap(uerr)
		}
		outcomeByID[id] = backupUndeleteOutcome{
			BackupID: id,
			Restored: restored,
		}
		if restored {
			restoredIDs = append(restoredIDs, id)
		}
	}

	// Report rows in the caller's argv order, whatever order processing
	// used — the result shape must not vary with chain topology.
	results := make([]backupUndeleteOutcome, 0, len(ids))
	for _, id := range ids {
		results = append(results, outcomeByID[id])
	}

	// Single audit event per call, listing every backup that was
	// actually resurrected. Already-live IDs are NOT in this list:
	// the audit chain captures real state changes, not no-ops.
	// Nothing to emit when no ID was actually restored.
	if len(restoredIDs) > 0 {
		body := map[string]any{
			"deployment":   deployment,
			"reason":       auditReason,
			"restored":     restoredIDs,
			"requested":    ids,
			"already_live": len(ids) - len(restoredIDs),
		}
		audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), &audit.Event{
			Action: "backup.undelete",
			Subject: audit.Subject{
				Deployment: deployment,
				BackupID:   restoredIDs[0],
				Repo:       repoURL,
			},
			Timestamp: time.Now().UTC(),
			Body:      body,
		})
	}

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(backupUndeleteBody{
		Deployment:  deployment,
		Reason:      auditReason,
		Outcomes:    results,
		Restored:    restoredIDs,
		ChunkChecks: chunkChecks,
	}))
}

// backupUndeleteBody is the v1-stable result. Outcomes preserves
// per-ID status in argv order; Restored is the convenience slice
// of just the IDs that were actually resurrected (mirror of the
// cascade-delete result body). ChunkChecks is populated only
// when --check-chunks is set; per-manifest chunk-existence
// totals + missing summary so the operator sees what passed +
// what didn't.
type backupUndeleteBody struct {
	Deployment  string                  `json:"deployment"`
	Reason      string                  `json:"reason,omitempty"`
	Outcomes    []backupUndeleteOutcome `json:"outcomes"`
	Restored    []string                `json:"restored"`
	ChunkChecks []chunkCheckRow         `json:"chunk_checks,omitempty"`
}

type backupUndeleteOutcome struct {
	BackupID string `json:"backup_id"`
	Restored bool   `json:"restored"`
	// ChunksMissing is true when --check-chunks + --skip-missing
	// caused this ID to be skipped because some chunks were
	// absent. Distinguished from "already live" so an operator
	// reading the JSON sees the right reason.
	ChunksMissing bool `json:"chunks_missing,omitempty"`
}

// chunkCheckRow surfaces the per-manifest pre-flight result
// inside the response body. Only populated under --check-chunks.
type chunkCheckRow struct {
	BackupID    string `json:"backup_id"`
	TotalUnique int    `json:"total_unique"`
	Present     int    `json:"present"`
	MissingN    int    `json:"missing"`
	// Missing is a truncated list of missing-chunk hashes
	// (max 5; "+N more" appended for longer lists). Forensic-
	// friendly without bloating the body for multi-thousand-
	// chunk manifests.
	Missing string `json:"missing_summary,omitempty"`
}

// WriteText renders the undelete outcome — restored IDs plus any chunk-presence
// findings — as human-readable text to w.
func (b backupUndeleteBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if len(b.Restored) == 0 {
		// Pure no-op: every ID was already live (no
		// --check-chunks skips, no actual mutations).
		// Distinguish from --skip-missing case below.
		var skipped int
		for _, o := range b.Outcomes {
			if o.ChunksMissing {
				skipped++
			}
		}
		if skipped > 0 {
			fmt.Fprintf(bw, "✗ %s: 0 restored — %d skipped due to missing chunks\n",
				b.Deployment, skipped)
		} else {
			fmt.Fprintf(bw, "✓ no-op: every requested backup already live in %s (%d ID(s) checked)",
				b.Deployment, len(b.Outcomes))
			_, err := io.WriteString(w, bw.String())
			return err
		}
	} else {
		fmt.Fprintf(bw, "✓ %s: %d backup(s) restored\n", b.Deployment, len(b.Restored))
	}
	for i, o := range b.Outcomes {
		marker := "├─"
		if i == len(b.Outcomes)-1 {
			marker = "└─"
		}
		state := "restored"
		switch {
		case o.ChunksMissing:
			state = "skipped (chunks missing)"
		case !o.Restored:
			state = "already live (no-op)"
		}
		fmt.Fprintf(bw, "  %s %s — %s\n", marker, o.BackupID, state)
	}
	if len(b.ChunkChecks) > 0 {
		fmt.Fprintf(bw, "\nCHUNK CHECK\n")
		for _, c := range b.ChunkChecks {
			ok := "✓"
			if c.MissingN > 0 {
				ok = "✗"
			}
			fmt.Fprintf(bw, "  %s %s — %d/%d present", ok, c.BackupID, c.Present, c.TotalUnique)
			if c.MissingN > 0 {
				fmt.Fprintf(bw, ", %d missing: %s", c.MissingN, c.Missing)
			}
			fmt.Fprintln(bw)
		}
	}
	if b.Reason != "" {
		fmt.Fprintf(bw, "  Reason: %s\n", b.Reason)
	}
	fmt.Fprintf(bw, "  Note:   restorability depends on chunks not yet swept; run `pg_hardstorage verify %s <id>` to confirm",
		b.Deployment)
	_, err := io.WriteString(w, bw.String())
	return err
}

// summarizeMissingHashes returns up to max hashes formatted as
// hex strings, with "(+N more)" appended when truncated.
// Mirror of cli/verify.go's summarizeHashes — kept local so the
// undelete file doesn't import the verify-internal helper.
func summarizeMissingHashes(h []repo.Hash, max int) string {
	if len(h) == 0 {
		return ""
	}
	n := len(h)
	if n > max {
		n = max
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, h[i].String())
	}
	out := strings.Join(parts, ", ")
	if len(h) > max {
		out += fmt.Sprintf(" (+%d more)", len(h)-max)
	}
	return out
}

// sortStringsStable is a tiny wrapper so the per-callsite intent
// stays readable.
func sortStringsStable(s []string) {
	sort.Strings(s)
}
