// repo_gc.go — 'repo gc' CLI verb: garbage-collect orphan chunks (approval-gated on --apply).
package cli

import (
	"context"
	"encoding/json"
	"errors"
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
)

// GCOp is the approval-namespace string the `repo gc --apply`
// destructive op binds to. Dry-runs (no --apply) need no approval —
// they only read.
const GCOp = approval.Op("repo.gc")

// gcDeleteConcurrency bounds the chunk deletes a sweep runs at once.
const gcDeleteConcurrency = 16

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
	// Day-aware for the same reason as wal prune's twin flag: the two
	// grace values are meant to be THE SAME VALUE (prune keeps a
	// tombstoned backup's WAL while gc keeps its chunks — if they
	// drift, undelete breaks), and prune's help even says "matches
	// repo gc". A pair of flags that must agree but accept different
	// spellings of the same duration invites exactly that drift.
	DurationDaysVar(c.Flags(), &tombstoneGrace, "tombstone-grace", repo.DefaultTombstoneGracePeriod,
		"minimum tombstone age before the manifest's chunks become GC candidates (defends Undelete-after-delete; pass 0 to disable)")
	DurationDaysVar(c.Flags(), &minChunkAge, "min-chunk-age", repo.DefaultOrphanMinAge,
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

	// In -o json the command's output is ONE document: the Result. An
	// Event emitted beside it made gc print two JSON documents, which a
	// `repo gc -o json | jq` pipeline cannot parse. Notices therefore go
	// into the result body in JSON mode and stay events otherwise.
	var notices []repoGCNotice
	notify := func(sev output.Severity, code string, body map[string]any) {
		if d.Renderer().Name() == "json" {
			notices = append(notices, repoGCNotice{Severity: sev.String(), Code: code, Detail: body})
			return
		}
		_ = d.Event(cmd.Context(), output.NewEvent(sev, "repo.gc", code).WithBody(body))
	}

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
		// Dry-runs ignore --require-approval rather than refuse — the
		// operator might just be sanity-checking that the approval is
		// queued before they pull the trigger. Say so, so they don't
		// think the gate fired.
		notify(output.SeverityNotice, "approval_skipped_dry_run", map[string]any{
			"approval_id": approvalID,
			"hint":        "the gate fires only on --apply; this dry-run does not consult the approval",
		})
	}

	// Tombstone-grace and chunk-age floor: a 0 flag value means
	// "disable" — mapped to the underlying API's negative sentinel.
	graceForCall := tombstoneGrace
	if graceForCall == 0 {
		graceForCall = -1
	}
	minAgeForCall := minChunkAge
	if minAgeForCall == 0 {
		minAgeForCall = -1
	}

	// Safety-floor warning. The tombstone-grace and chunk-age floors
	// are what stop --apply from reaping an in-flight backup's chunks
	// (durable but not yet manifest-committed) or a just-soft-deleted
	// manifest's chunks before `backup undelete` could recover them.
	// On --apply a disabled floor removes a real guardrail, so say so
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
			notify(output.SeverityWarning, "safety_floor_disabled", map[string]any{
				"disabled_floors": disabled,
				"impact":          "with these floors disabled, --apply can delete an in-flight backup's chunks (durable but not yet manifest-committed) and a just-soft-deleted manifest's chunks before `backup undelete` could recover them — both unrecoverable",
				"hint":            "omit the flag (or pass a positive duration) to keep the 24h default floor; here 0 means DISABLE, not 'use default'",
			})
		}
	}

	// The sweep. repo.Sweep holds the whole safety protocol (see
	// internal/repo/gcfence.go): the run record writers fence against,
	// the settle before the deciding snapshot, and per-batch
	// checkpoints that re-read writer pins, re-scan backup leases and
	// re-scan manifests committed since the snapshot. Backup leases are
	// defence in depth for writers that predate the fence: a live one
	// refuses the start, and one that appears mid-sweep stops it.
	res, serr := repo.Sweep(cmd.Context(), sp, repo.SweepOptions{
		TombstoneGrace: graceForCall,
		MinChunkAge:    minAgeForCall,
		Apply:          apply,
		LiveLeases: func(ctx context.Context) ([]string, error) {
			return findLiveBackupLeases(ctx, sp, time.Now().UTC())
		},
		DeleteConcurrency: gcDeleteConcurrency,
		OnWarning: func(msg string) {
			notify(output.SeverityWarning, "cleanup_incomplete", map[string]any{"detail": msg})
		},
	})
	if serr != nil && res == nil {
		return mapSweepError(serr)
	}

	body := repoGCBody{
		DryRun:            !apply,
		ManifestRefCount:  res.RefCount,
		OrphanCount:       len(res.Orphans),
		BytesReclaimable:  res.OrphanBytes,
		StaleTempCount:    len(res.StaleTemps),
		RunID:             res.RunID,
		SkippedReferenced: res.SkippedReferenced,
		StoppedEarly:      res.StoppedEarly,
	}

	// partialErr, when non-nil, records that the sweep did not complete
	// (delete failures, or a sweep that had to stop). It is returned
	// only after the result body and audit event are emitted, so a
	// partial sweep still reports what it reclaimed.
	var partialErr error
	if apply {
		body.Applied = res.Deleted
		body.BytesReclaimed = res.DeletedBytes
		body.StaleTempDeleted = res.StaleTempDeleted
		body.StagingReaped = res.StagingReaped
		body.StagingReapedBytes = res.StagingReapedBytes
		failureCount := len(res.Failures)
		failures := res.Failures
		const maxFailures = 16
		if failureCount > maxFailures {
			failures = append(failures[:maxFailures:maxFailures],
				fmt.Sprintf("... +%d more", failureCount-maxFailures))
		}
		body.Failures = failures
		switch {
		case serr != nil:
			partialErr = mapSweepError(serr)
		case failureCount > 0:
			partialErr = output.NewError("repo.gc.partial_failure",
				fmt.Sprintf("repo gc: %d deletion(s) failed (of %d orphan chunk(s) + %d stale staging file(s))",
					failureCount, body.OrphanCount, body.StaleTempCount)).
				WithSuggestion(&output.Suggestion{
					Human: "review the failures; transient backend errors usually clear on a retry. Persistent ones are storage-side (perms, throttling).",
				})
		}

		// Audit emission for the gated apply. Only when an approval
		// gated the action — un-gated GC is covered by the structured
		// Result, and an audit event per cron-driven GC would be noise.
		if gateReq != nil {
			audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), &audit.Event{
				Action: "repo.gc",
				Tenant: gateReq.Tenant,
				Subject: audit.Subject{
					Repo:   repoURL,
					Tenant: gateReq.Tenant,
				},
				Timestamp: time.Now().UTC(),
				Body: map[string]any{
					"url":             repoURL,
					"approval_id":     gateReq.ID,
					"approval_op":     string(gateReq.Op),
					"threshold":       gateReq.Threshold,
					"approvers":       len(gateReq.Approvals),
					"orphans_found":   len(res.Orphans),
					"orphans_deleted": res.Deleted,
					"bytes_reclaimed": res.DeletedBytes,
				},
			})
		}
	}
	if gateReq != nil {
		body.ApprovalID = gateReq.ID
	}

	// Orphans are sorted by hash (repo.Sweep's contract).
	const maxListedHashes = 64
	for i, h := range res.Orphans {
		if i >= maxListedHashes {
			body.Hashes = append(body.Hashes, fmt.Sprintf("... +%d more", len(res.Orphans)-maxListedHashes))
			break
		}
		body.Hashes = append(body.Hashes, h.String())
	}
	body.Notices = notices
	if err := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); err != nil {
		return err
	}
	return partialErr
}

// mapSweepError maps a repo.Sweep failure onto the CLI's error codes. A
// live backup lease is a CONFLICT (exit 7, retry-safe once the backup
// finishes) — exit-codes.md documents lease / in-progress conflicts as
// exit 7, and gc used to exit 1 for it.
func mapSweepError(err error) error {
	if errors.Is(err, repo.ErrSweepBackupInFlight) {
		return output.NewError("conflict.gc_backup_in_flight",
			fmt.Sprintf("repo gc: refusing to sweep while backups are in flight: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "an in-flight backup may have deduplicated against chunks this sweep would delete — re-run after the backups finish (a crashed holder's lease expires within its TTL, 15 minutes by default)",
			}).Wrap(err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return output.NewError("aborted.gc", fmt.Sprintf("repo gc: %v", err)).Wrap(err)
	}
	return output.NewError("repo.gc.failed", fmt.Sprintf("repo gc: %v", err)).Wrap(err)
}

// repoGCNotice is a warning/notice carried in the result body in JSON
// mode (where emitting it as a separate event would break the
// one-document contract).
type repoGCNotice struct {
	Severity string         `json:"severity"`
	Code     string         `json:"code"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// repoGCBody is the v1-stable result body.
type repoGCBody struct {
	DryRun           bool  `json:"dry_run"`
	ManifestRefCount int   `json:"manifest_ref_count"`
	OrphanCount      int   `json:"orphan_count"`
	BytesReclaimable int64 `json:"bytes_reclaimable"`
	Applied          int   `json:"applied,omitempty"`
	BytesReclaimed   int64 `json:"bytes_reclaimed,omitempty"`
	StaleTempCount   int   `json:"stale_temp_count,omitempty"`
	StaleTempDeleted int   `json:"stale_temp_deleted,omitempty"`
	// StagingReaped counts backend staging files a crashed writer left
	// (invisible to List) that this run removed.
	StagingReaped      int      `json:"staging_reaped,omitempty"`
	StagingReapedBytes int64    `json:"staging_reaped_bytes,omitempty"`
	Hashes             []string `json:"hashes,omitempty"`
	Failures           []string `json:"failures,omitempty"`
	ApprovalID         string   `json:"approval_id,omitempty"`
	// RunID names this sweep's run record (gc/runs/<id>.json) — the
	// object concurrent writers fence against. --apply only.
	RunID string `json:"run_id,omitempty"`
	// SkippedReferenced counts orphans spared at delete time because a
	// manifest committed after the snapshot, or a writer's pin, claimed
	// them.
	SkippedReferenced int `json:"skipped_referenced,omitempty"`
	// StoppedEarly says why the sweep stopped before its last batch.
	StoppedEarly string `json:"stopped_early,omitempty"`
	// Notices carries warnings in JSON mode (see runRepoGC).
	Notices []repoGCNotice `json:"notices,omitempty"`
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
		if b.StagingReaped > 0 {
			fmt.Fprintf(bw, "  ✓ removed %d leftover backend staging file(s) (%s)\n", b.StagingReaped, humanBytes(b.StagingReapedBytes))
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
	if b.StoppedEarly != "" {
		fmt.Fprintf(bw, "  ✗ stopped early: %s\n", b.StoppedEarly)
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

// findLiveBackupLeases scans leases/ for backup leases whose
// ExpiresAt is still in the future and returns the deployments that
// hold them. `repo gc --apply` refuses to sweep while any exist: an
// in-flight backup may have deduplicated against exactly the old,
// unreferenced chunks the sweep is about to delete, and neither the
// chunk-age floor nor the reference snapshot can see that reuse.
func findLiveBackupLeases(ctx context.Context, sp storage.StoragePlugin, now time.Time) ([]string, error) {
	var live []string
	for info, err := range sp.List(ctx, "leases/") {
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(info.Key, "/backup.json") {
			continue
		}
		rc, gerr := sp.Get(ctx, info.Key)
		if gerr != nil {
			if errors.Is(gerr, storage.ErrNotFound) {
				continue // released between List and Get
			}
			return nil, gerr
		}
		var lease struct {
			Deployment string    `json:"deployment"`
			ExpiresAt  time.Time `json:"expires_at"`
			Released   bool      `json:"released"`
		}
		derr := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&lease)
		_ = rc.Close()
		if derr != nil {
			// An unreadable lease is treated as LIVE, not ignored:
			// gc deleting chunks because it couldn't parse the very
			// object that says "backup in flight" is the wrong
			// failure direction.
			return nil, fmt.Errorf("parse lease %s: %w", info.Key, derr)
		}
		if !lease.Released && now.Before(lease.ExpiresAt) {
			name := lease.Deployment
			if name == "" {
				name = strings.TrimSuffix(strings.TrimPrefix(info.Key, "leases/"), "/backup.json")
			}
			live = append(live, name)
		}
	}
	sort.Strings(live)
	return live, nil
}
