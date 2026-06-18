// repo_gc.go — 'repo gc' CLI verb: garbage-collect orphan chunks (approval-gated on --apply).
package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/approval"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// GCOp is the approval-namespace string the `repo gc --apply`
// destructive op binds to. Dry-runs (no --apply) need no approval —
// they only read.
const GCOp = approval.Op("repo.gc")

// newRepoGCCmd implements `pg_hardstorage repo gc`. The same primitives
// power `repair chunks --orphans` — but the two commands have
// deliberately different intents and defaults:
//
//   - `repair chunks --orphans` is the diagnostic. "I think something
//     is wrong; show me what's not referenced." Dry-run by default,
//     `--apply` opt-in.
//   - `repo gc` is the routine maintenance. Same dry-run-by-default
//     posture (you should never blow away chunks without seeing the
//     plan first), but the result body and text output are framed
//     around "how much space did we just reclaim?"
//
// The split lets operators wire `repo gc --apply` into a periodic job
// without having to remember the `repair chunks --orphans --apply`
// incantation, and keeps `repair` as the everything's-on-fire surface.
func newRepoGCCmd() *cobra.Command {
	var (
		repoURL         string
		apply           bool
		requireApproval string
		tombstoneGrace  time.Duration
		minChunkAge     time.Duration
	)
	c := &cobra.Command{
		Use:   "gc <url>",
		Short: "Reclaim space — sweep chunks no manifest references",
		Long: `gc walks every committed manifest, builds the set of referenced
chunk hashes, and lists chunks under chunks/sha256/... that aren't
in that set. Without --apply, the operation is a dry-run and reports
how much space WOULD be freed. With --apply, the orphans are
deleted via the CAS.

Tombstoned manifests (soft-deleted by retention) are excluded from
the reference walk ONCE they age past --tombstone-grace.  Default
grace is 24h: an operator who soft-deletes a backup, notices the
mistake, and runs ` + "`" + `backup undelete` + "`" + ` within 24h gets a
fully-restorable backup back even if --apply ran in between.
Operators willing to accept the audit-flagged race (v23 #3) can
pass --tombstone-grace 0 to restore the historical immediate-
collection behaviour.

Either pass <url> as a positional, or via --repo. The latter form
keeps the command shape consistent with other subcommands (rotate,
list, etc.) for scripting.

--require-approval <id>: gate --apply on an existing n-of-m
approval request. The approval's Op must be ` + "`" + `repo.gc` + "`" + ` and its
Target must be the URL being GC'd; otherwise --apply is refused at
the gate. Dry-runs (no --apply) don't need an approval — they read
only. See ` + "`" + `pg_hardstorage approval` + "`" + `.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if repoURL != "" && repoURL != args[0] {
					return output.NewError("usage.repo_conflict",
						"repo gc: --repo and the positional URL disagree").Wrap(output.ErrUsage)
				}
				repoURL = args[0]
			}
			return runRepoGC(cmd, repoURL, apply, requireApproval, tombstoneGrace, minChunkAge)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (positional <url> is also accepted)")
	c.Flags().BoolVar(&apply, "apply", false,
		"actually delete the orphan chunks (default: dry-run)")
	c.Flags().StringVar(&requireApproval, "require-approval", "",
		"approval request ID that must be in approved state for repo.gc + this URL (n-of-m gate; --apply only)")
	c.Flags().DurationVar(&tombstoneGrace, "tombstone-grace", repo.DefaultTombstoneGracePeriod,
		"minimum tombstone age before the manifest's chunks become GC candidates (defends Undelete-after-delete; pass 0 to disable)")
	c.Flags().DurationVar(&minChunkAge, "min-chunk-age", repo.DefaultOrphanMinAge,
		"minimum age an unreferenced chunk (and stale staging file) must reach before --apply reaps it; defends an in-flight backup whose manifest hasn't committed yet (pass 0 to disable)")
	return c
}

func runRepoGC(cmd *cobra.Command, repoURL string, apply bool, approvalID string, tombstoneGrace, minChunkAge time.Duration) error {
	d := DispatcherFrom(cmd)
	// Positional-or-flag: guard the resolved value, not the flag.
	if repoURL == "" {
		return missingFlagErr(cmd, "--repo (or the first positional <url>)")
	}
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	// Only refuse on --apply: a dry-run never mutates and the operator
	// asking "what *would* I delete?" is a perfectly valid read-only
	// query. Same posture for the approval gate — dry-runs don't
	// touch chunks so they don't need n-of-m sign-off.
	var gateReq *approval.Request
	if apply {
		if err := assertRepoWritable(cmd.Context(), sp, "repo gc --apply"); err != nil {
			return err
		}
		if approvalID != "" {
			req, gerr := approval.NewStore(sp).Gate(cmd.Context(), approval.GateOptions{
				RequestID: approvalID,
				Op:        GCOp,
				Target:    repoURL,
			})
			if gerr != nil {
				return mapApprovalGateError("repo gc --apply", approvalID, gerr)
			}
			gateReq = req
		}
	} else if approvalID != "" {
		// Dry-runs ignore --require-approval rather than refuse —
		// the operator might just be sanity-checking that the
		// approval is queued before they pull the trigger.
		// We surface a notice so they don't think the gate
		// fired.
		_ = DispatcherFrom(cmd).Event(cmd.Context(),
			output.NewEvent(output.SeverityNotice, "repo.gc", "approval_skipped_dry_run").
				WithBody(map[string]any{
					"approval_id": approvalID,
					"hint":        "the gate fires only on --apply; this dry-run does not consult the approval",
				}))
	}

	// Tombstone-grace: a 0 flag value means "disable" (caller
	// explicitly opts out — historical aggressive behaviour); a
	// non-zero value flows through; the package default is
	// applied when CollectReferencesOptions.TombstoneGrace is its
	// zero value, but here the flag default is already the
	// package default so we always pass it through explicitly.
	graceForCall := tombstoneGrace
	if graceForCall == 0 {
		graceForCall = -1 // disable in the underlying API
	}
	refs, err := repo.CollectReferencesWithOptions(cmd.Context(), sp,
		repo.CollectReferencesOptions{TombstoneGrace: graceForCall})
	if err != nil {
		return output.NewError("repo.gc.collect_refs_failed",
			fmt.Sprintf("repo gc: collect references: %v", err)).Wrap(err)
	}
	// Chunk-age floor: an unreferenced chunk younger than this is
	// kept, so a `--apply` racing an in-flight backup (whose chunks
	// are durable but whose manifest hasn't committed yet) can't reap
	// them out from under it.  A 0 flag value means "disable" — maps
	// to the underlying API's negative-disables sentinel, same shape
	// as --tombstone-grace.
	minAgeForCall := minChunkAge
	if minAgeForCall == 0 {
		minAgeForCall = -1
	}

	// Safety-floor warning. The tombstone-grace and chunk-age floors
	// are what stop --apply from reaping an in-flight backup's chunks
	// (durable but not yet manifest-committed) or a just-soft-deleted
	// manifest's chunks before `backup undelete` could recover them.
	// A 0 or negative flag value disables a floor (resolved to <= 0
	// above). On --apply that removes a real guardrail, so say so
	// loudly — an operator who passed `--min-chunk-age 0` thinking it
	// meant "use the default" needs to see they disarmed it. Dry-runs
	// delete nothing, so the warning is --apply-only.
	if apply {
		var disabled []string
		if graceForCall <= 0 {
			disabled = append(disabled, "tombstone-grace")
		}
		if minAgeForCall <= 0 {
			disabled = append(disabled, "min-chunk-age")
		}
		if len(disabled) > 0 {
			_ = d.Event(cmd.Context(),
				output.NewEvent(output.SeverityWarning, "repo.gc", "safety_floor_disabled").
					WithBody(map[string]any{
						"disabled_floors": disabled,
						"impact":          "with these floors disabled, --apply can delete an in-flight backup's chunks (durable but not yet manifest-committed) and a just-soft-deleted manifest's chunks before `backup undelete` could recover them — both unrecoverable",
						"hint":            "omit the flag (or pass a positive duration) to keep the 24h default floor; here 0 means DISABLE, not 'use default'",
					}))
		}
	}

	orphanOpts := repo.FindOrphansOptions{MinAge: minAgeForCall}
	hashes, err := repo.FindOrphansWithOptions(cmd.Context(), sp, refs, orphanOpts)
	if err != nil {
		return output.NewError("repo.gc.find_orphans_failed",
			fmt.Sprintf("repo gc: find orphans: %v", err)).Wrap(err)
	}

	// Stale staging files: `*.json.tmp.<rand>` left by a commit whose
	// process died between the tmp Put and the atomic rename.  No
	// chunk sweep reclaims these (they live under manifests//wal/, not
	// chunks/), so gc is the natural place to reap them.  Same age
	// floor as chunks.
	staleTmp, err := repo.FindStaleTempManifests(cmd.Context(), sp, orphanOpts)
	if err != nil {
		return output.NewError("repo.gc.find_stale_temp_failed",
			fmt.Sprintf("repo gc: find stale staging files: %v", err)).Wrap(err)
	}

	// Account bytes — Stat is O(orphan count) but that's the same shape
	// the actual delete loop has anyway.
	bytes, err := sumChunkBytes(cmd.Context(), sp, hashes)
	if err != nil {
		return output.NewError("repo.gc.size_failed",
			fmt.Sprintf("repo gc: stat orphans: %v", err)).Wrap(err)
	}

	body := repoGCBody{
		DryRun:           !apply,
		ManifestRefCount: refs.Len(),
		OrphanCount:      len(hashes),
		BytesReclaimable: bytes,
		StaleTempCount:   len(staleTmp),
	}

	if apply {
		cas := casdefault.New(sp)
		var deleted int
		var deletedBytes int64
		var failures []string
		for _, h := range hashes {
			// Stat-then-delete so a race-induced miss doesn't blow the
			// whole sweep — we account only what we actually removed.
			info, statErr := sp.Stat(cmd.Context(), repo.ChunkKey(h))
			if delErr := cas.DeleteChunk(cmd.Context(), h); delErr != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", h, delErr))
				continue
			}
			deleted++
			if statErr == nil {
				deletedBytes += info.Size
			}
		}
		body.Applied = deleted
		body.BytesReclaimed = deletedBytes

		// Reap stale staging files.  Best-effort: a tmp object under
		// an active object-lock retention can't be deleted until the
		// lock expires — that surfaces as a delete failure we report
		// rather than fail the whole sweep on; a later gc reaps it
		// once the lock lapses.
		var tmpDeleted int
		for _, key := range staleTmp {
			if delErr := sp.Delete(cmd.Context(), key); delErr != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", key, delErr))
				continue
			}
			tmpDeleted++
		}
		body.StaleTempDeleted = tmpDeleted
		// Capture the true failure count BEFORE truncating the
		// per-hash detail list — otherwise the partial-failure error
		// reports "17 deletions failed" when 100 actually did.
		failureCount := len(failures)
		const maxFailures = 16
		if failureCount > maxFailures {
			failures = append(failures[:maxFailures:maxFailures],
				fmt.Sprintf("... +%d more", failureCount-maxFailures))
		}
		body.Failures = failures

		if failureCount > 0 {
			// Some deletes failed — surface as a structured error so the
			// exit code reflects "the cleanup was partial." Use the
			// generic-error namespace; nothing in our exit-code map flags
			// this as preflight or verify.
			return output.NewError("repo.gc.partial_failure",
				fmt.Sprintf("repo gc: %d deletion(s) failed (of %d orphan chunk(s) + %d stale staging file(s))",
					failureCount, body.OrphanCount, body.StaleTempCount)).
				WithSuggestion(&output.Suggestion{
					Human: "review the failures; transient backend errors usually clear on a retry. Persistent ones are storage-side (perms, throttling).",
				})
		}

		// Audit emission for the gated apply. We only write an audit
		// event when an approval gated the action — un-gated GC is
		// already covered by the structured Result the dispatcher
		// emits, and writing audit on every cron-driven GC would be
		// noise. Best-effort.
		if gateReq != nil {
			body := map[string]any{
				"url":             repoURL,
				"approval_id":     gateReq.ID,
				"approval_op":     string(gateReq.Op),
				"threshold":       gateReq.Threshold,
				"approvers":       len(gateReq.Approvals),
				"orphans_found":   len(hashes),
				"orphans_deleted": deleted,
				"bytes_reclaimed": deletedBytes,
			}
			audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), &audit.Event{
				Action: "repo.gc",
				Tenant: gateReq.Tenant,
				Subject: audit.Subject{
					Repo:   repoURL,
					Tenant: gateReq.Tenant,
				},
				Timestamp: time.Now().UTC(),
				Body:      body,
			})
		}
	}
	if gateReq != nil {
		body.ApprovalID = gateReq.ID
	}

	// Sort hashes lex for deterministic output (FindOrphans already
	// does this; making it explicit here documents the contract).
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].String() < hashes[j].String() })
	const maxListedHashes = 64
	for i, h := range hashes {
		if i >= maxListedHashes {
			body.Hashes = append(body.Hashes, fmt.Sprintf("... +%d more", len(hashes)-maxListedHashes))
			break
		}
		body.Hashes = append(body.Hashes, h.String())
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// sumChunkBytes Stats every orphan once to compute the total reclaim
// estimate. We don't fail the GC on Stat misses (a chunk that's
// already gone simply contributes 0 bytes) — the dry-run is a
// best-effort estimate, not an exact count.
func sumChunkBytes(ctx context.Context, sp storage.StoragePlugin, hashes []repo.Hash) (int64, error) {
	var total int64
	for _, h := range hashes {
		info, err := sp.Stat(ctx, repo.ChunkKey(h))
		if err != nil {
			// Non-fatal: a missing chunk during dry-run is still an
			// orphan candidate; just count 0 for it.
			continue
		}
		total += info.Size
	}
	return total, nil
}

// repoGCBody is the v1-stable result body.
type repoGCBody struct {
	DryRun           bool     `json:"dry_run"`
	ManifestRefCount int      `json:"manifest_ref_count"`
	OrphanCount      int      `json:"orphan_count"`
	BytesReclaimable int64    `json:"bytes_reclaimable"`
	Applied          int      `json:"applied,omitempty"`
	BytesReclaimed   int64    `json:"bytes_reclaimed,omitempty"`
	StaleTempCount   int      `json:"stale_temp_count,omitempty"`
	StaleTempDeleted int      `json:"stale_temp_deleted,omitempty"`
	Hashes           []string `json:"hashes,omitempty"`
	Failures         []string `json:"failures,omitempty"`
	ApprovalID       string   `json:"approval_id,omitempty"`
}

// WriteText renders the operator-facing form.
func (b repoGCBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.DryRun {
		fmt.Fprintf(bw, "repo gc — dry-run\n")
	} else {
		fmt.Fprintf(bw, "repo gc --apply\n")
	}
	fmt.Fprintf(bw, "  manifests reference %d distinct chunks\n", b.ManifestRefCount)
	fmt.Fprintf(bw, "  %d orphan chunk(s) (%s)\n", b.OrphanCount, humanBytes(b.BytesReclaimable))
	if b.StaleTempCount > 0 {
		fmt.Fprintf(bw, "  %d stale staging file(s) from interrupted commits\n", b.StaleTempCount)
	}
	if !b.DryRun {
		fmt.Fprintf(bw, "  ✓ deleted %d (%s reclaimed)\n", b.Applied, humanBytes(b.BytesReclaimed))
		if b.StaleTempCount > 0 {
			fmt.Fprintf(bw, "  ✓ removed %d stale staging file(s)\n", b.StaleTempDeleted)
		}
		if len(b.Failures) > 0 {
			fmt.Fprintf(bw, "  ✗ %d delete failure(s):\n", len(b.Failures))
			for _, f := range b.Failures {
				fmt.Fprintf(bw, "      %s\n", f)
			}
		}
	} else if b.OrphanCount > 0 || b.StaleTempCount > 0 {
		fmt.Fprintf(bw, "  (pass --apply to actually delete)\n")
	}
	if len(b.Hashes) > 0 && b.OrphanCount > 0 {
		fmt.Fprintln(bw, "  hashes:")
		for _, h := range b.Hashes {
			fmt.Fprintf(bw, "    %s\n", h)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
