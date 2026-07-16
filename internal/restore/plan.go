// plan.go — Plan: dry-run description of a restore the CLI's --preview renders.
package restore

import (
	"context"
	"errors"
	"fmt"
	stdfs "io/fs"
	"os"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

// Plan describes what a Restore call would do, without touching disk.
//
// Returned by the Plan function. The CLI's --preview flag renders this
// so operators can sanity-check before committing to a real restore.
type Plan struct {
	BackupID   string `json:"backup_id"`
	Deployment string `json:"deployment"`
	Tenant     string `json:"tenant,omitempty"`
	TargetDir  string `json:"target_dir"`

	PGVersion        int    `json:"pg_version"`
	SystemIdentifier string `json:"system_identifier"`
	StartLSN         string `json:"start_lsn"`
	StopLSN          string `json:"stop_lsn"`
	Timeline         uint32 `json:"timeline"`

	FileCount         int   `json:"file_count"`
	TotalBytes        int64 `json:"total_bytes"`
	ChunkRefCount     int   `json:"chunk_ref_count"`
	UniqueChunkCount  int   `json:"unique_chunk_count"`
	UniqueChunkBytes  int64 `json:"unique_chunk_bytes"`
	BackupLabelSize   int   `json:"backup_label_size"`
	TablespaceMapSize int   `json:"tablespace_map_size"`

	Tablespaces []backup.Tablespace `json:"tablespaces,omitempty"`

	// Recovery, when non-nil, mirrors the operator's PITR target
	// flags (--to / --to-lsn / --to-name / --to-action / --to-
	// timeline) back into the plan body so `--preview` confirms
	// the target reached the planner.  Before issue #99 the
	// preview path silently dropped these flags — the operator
	// saw the backup's Stop LSN under "Stop LSN / TLI:" and
	// assumed --to-lsn had no effect; this block makes the
	// effect visible.
	Recovery *PlanRecovery `json:"recovery,omitempty"`

	// Preflight is the snapshot of target-dir checks. A Plan with
	// PreflightOK=false can still be returned successfully (the user
	// asked to plan, not to do); the CLI surfaces the issues so the
	// operator can fix them before the real restore.
	PreflightOK     bool     `json:"preflight_ok"`
	PreflightIssues []string `json:"preflight_issues,omitempty"`

	// WALArchiveHoleLSN, when non-empty, is the first LSN of a WAL
	// segment that is MISSING from the archive between this backup's
	// stop LSN and the requested PITR target — recovery would HALT
	// there. Surfaced so --preview matches the real restore's
	// restore.wal_archive_hole warning instead of reporting "✓ ready".
	WALArchiveHoleLSN string `json:"wal_archive_hole_lsn,omitempty"`

	// EstimatedRTO is a crude estimate based on TotalBytes divided by
	// a fixed throughput baseline. Documented as approximate; the real
	// number depends on storage backend, network, disk throughput, and
	// chunk dedup hit-rate from prior restores.
	EstimatedRTO time.Duration `json:"estimated_rto_ms"`

	// AssumedThroughput is the bytes-per-second figure used to compute
	// EstimatedRTO. Surfaced so the operator can re-estimate manually.
	AssumedThroughput int64 `json:"assumed_throughput_bytes_per_sec"`
}

// PlanOptions configures a planning run. Same shape as Options minus
// AllowOverwrite + OnEvent — Plan is read-only and emits no events
// (it would be one event, drowning the actual signal of the result).
type PlanOptions struct {
	RepoURL    string
	Deployment string
	BackupID   string
	TargetDir  string
	Verifier   *backup.Verifier

	// Recovery, when non-nil, is the PITR target the operator
	// asked for.  Preview echoes it into Plan.Recovery so the
	// rendered output confirms the flag reached the planner, and
	// runs the same reachability check Restore() runs (a
	// --to-lsn value below the backup's StopLSN cannot be
	// reached by forward WAL replay).  Issue #99.
	Recovery *Recovery
}

// PlanRecovery is the JSON-rendered echo of a Recovery target in
// the Plan body.  Field names mirror the GUC names PG actually
// applies; operators copying values between `--preview` output
// and postgresql.auto.conf shouldn't have to translate.
type PlanRecovery struct {
	TargetLSN  string `json:"target_lsn,omitempty"`
	TargetTime string `json:"target_time,omitempty"`
	TargetName string `json:"target_name,omitempty"`
	Inclusive  bool   `json:"inclusive"`
	Action     string `json:"action,omitempty"`
	Timeline   string `json:"timeline,omitempty"`
}

// EstimateThroughput is the bytes-per-second baseline used when no
// better measurement is available. Conservative: 100 MiB/s. The CLI
// can override this once we have storage-backend-specific telemetry.
const EstimateThroughput = 100 * 1024 * 1024

// Preview returns a Plan describing what Restore would do for opts.
//
// All operations are read-only. The repo is opened, the manifest is
// fetched + verified, the target dir is inspected (without being
// created), and aggregates are computed by walking the manifest's
// FileEntries.
func Preview(ctx context.Context, opts PlanOptions) (*Plan, error) {
	if err := validatePlanOptions(&opts); err != nil {
		return nil, err
	}
	_, sp, err := repo.Open(ctx, opts.RepoURL)
	if err != nil {
		return nil, mapRepoErr(opts.RepoURL, err)
	}
	defer sp.Close()

	store := backup.NewManifestStore(sp)
	m, err := store.Read(ctx, opts.Deployment, opts.BackupID, opts.Verifier)
	if err != nil {
		return nil, fmt.Errorf("plan: read manifest %s/%s: %w",
			opts.Deployment, opts.BackupID, err)
	}

	p := &Plan{
		BackupID:          m.BackupID,
		Deployment:        m.Deployment,
		Tenant:            m.Tenant,
		TargetDir:         opts.TargetDir,
		PGVersion:         m.PGVersion,
		SystemIdentifier:  m.SystemIdentifier,
		StartLSN:          m.StartLSN,
		StopLSN:           m.StopLSN,
		Timeline:          m.Timeline,
		FileCount:         len(m.Files),
		Tablespaces:       m.Tablespaces,
		BackupLabelSize:   len(m.BackupLabel),
		TablespaceMapSize: len(m.TablespaceMap),
		AssumedThroughput: EstimateThroughput,
	}

	uniqueBytes := map[repo.Hash]int64{}
	for _, f := range m.Files {
		p.TotalBytes += f.Size
		p.ChunkRefCount += len(f.Chunks)
		for _, c := range f.Chunks {
			uniqueBytes[c.Hash] = c.Len
		}
	}
	p.UniqueChunkCount = len(uniqueBytes)
	for _, sz := range uniqueBytes {
		p.UniqueChunkBytes += sz
	}

	// Plumb the operator's PITR target into the plan body so
	// --preview surfaces it (issue #99: the absence of this echo
	// made --to-lsn look like a no-op).  We also run the
	// reachability gate here so a backup that physically cannot
	// reach the target LSN refuses at preview time — the same
	// gate fires in Restore() so a preview-skipper still hits it.
	if opts.Recovery != nil && opts.Recovery.IsTargetSet() {
		if err := CheckTargetReachable(m.StopLSN, opts.Recovery); err != nil {
			return nil, err
		}
		p.Recovery = planRecoveryFromRecovery(opts.Recovery)
	}

	// Pre-flight: peek at the target dir without creating anything.
	// Plan reports the check; it does NOT refuse to plan.
	preflightSnapshot(opts.TargetDir, p)

	// WAL-contiguity peek: the real restore path warns (restore.
	// wal_archive_hole) when a WAL segment needed to replay from this
	// backup to the target LSN is MISSING from the archive — recovery
	// would HALT at the hole. --preview must surface the same finding
	// so the dry-run doesn't say "✓ ready" for a target the real
	// restore knows is unreachable.
	if opts.Recovery != nil && opts.Recovery.Enable && !opts.Recovery.SkipGapCheck && opts.Recovery.TargetLSN != "" {
		if target, terr := pglogrepl.ParseLSN(opts.Recovery.TargetLSN); terr == nil {
			if stop, serr := pglogrepl.ParseLSN(m.StopLSN); serr == nil && target >= stop {
				if hole, found, herr := inventory.FirstWALHoleInRange(ctx, sp, opts.Deployment, m.Timeline, stop, target); herr == nil && found {
					p.WALArchiveHoleLSN = hole.String()
				}
			}
		}
	}

	if p.AssumedThroughput > 0 {
		secs := float64(p.TotalBytes) / float64(p.AssumedThroughput)
		p.EstimatedRTO = time.Duration(secs * float64(time.Second))
	}
	return p, nil
}

// planRecoveryFromRecovery extracts the operator-facing fields
// from a *Recovery into the JSON-rendered PlanRecovery shape.
// Only the target / action / timeline / inclusive fields surface;
// internal fields (RestoreCommand, SkipGapCheck, StandbyMode) are
// implementation detail and stay out of the preview body.
func planRecoveryFromRecovery(r *Recovery) *PlanRecovery {
	if r == nil {
		return nil
	}
	pr := &PlanRecovery{
		TargetLSN:  r.TargetLSN,
		TargetName: r.TargetName,
		Inclusive:  r.Inclusive,
		Action:     r.Action,
		Timeline:   r.Timeline,
	}
	if !r.TargetTime.IsZero() {
		pr.TargetTime = r.TargetTime.UTC().Format(time.RFC3339Nano)
	}
	return pr
}

// CheckTargetReachable refuses a PITR target the chosen backup
// cannot reach by forward WAL replay.  PG replays WAL strictly
// forward starting from the backup's checkpoint LSN; a
// --to-lsn earlier than the backup's StopLSN can never be hit,
// so PG would silently recover to end-of-WAL instead of where
// the operator asked.  Before issue #99 there was no check —
// the operator got an end-of-WAL recovery and a wrong-time
// database without ever seeing an error.
//
// Returns nil for non-LSN targets (time / name) — those resolve
// to an LSN inside PG at recovery time and cannot be statically
// compared against StopLSN.  The wal-gap pre-flight still
// covers known-bad time targets via preflightTimeTargetGap.
//
// Returns nil when the recovery has no target set (end-of-WAL)
// or is StandbyMode (no stop point).
func CheckTargetReachable(stopLSN string, r *Recovery) error {
	if r == nil || !r.IsTargetSet() {
		return nil
	}
	if r.TargetLSN == "" {
		// Time/name targets aren't statically checkable.
		return nil
	}
	target, err := pglogrepl.ParseLSN(r.TargetLSN)
	if err != nil {
		return output.NewError("usage.bad_target_lsn",
			fmt.Sprintf("restore: parse --to-lsn %q: %v", r.TargetLSN, err)).
			Wrap(output.ErrUsage)
	}
	stop, err := pglogrepl.ParseLSN(stopLSN)
	if err != nil {
		// A malformed StopLSN is a manifest-invariant failure;
		// surface as such rather than masking it inside the
		// reachability gate.
		return output.NewError("manifest.invalid",
			fmt.Sprintf("restore: manifest stop_lsn %q is not a valid LSN: %v",
				stopLSN, err)).Wrap(err)
	}
	// Reachability semantics depend on whether the operator
	// asked for inclusive or exclusive stop (see targetReachable's
	// docstring).  The boundary comparison lives in its own file so
	// the mutation harness can flip it (mutation_target_reachable_off_by_one)
	// and prove the exclusive-equality test is tight.
	reachable := targetReachable(target, stop, r.Inclusive)
	if !reachable {
		hint := "at or after"
		if !r.Inclusive {
			hint = "strictly after"
		}
		return output.NewError("restore.target_unreachable",
			fmt.Sprintf("restore: --to-lsn %s is %s the backup's stop_lsn %s "+
				"(inclusive=%t); WAL replay only moves forward, so this target "+
				"can never be hit by recovery and PG would silently run to "+
				"end-of-WAL instead",
				r.TargetLSN, beforeOrAt(target, stop, r.Inclusive), stopLSN, r.Inclusive)).
			WithSuggestion(&output.Suggestion{
				Human: "pick a --to-lsn " + hint + " the chosen backup's stop_lsn, " +
					"or choose an earlier backup whose stop_lsn ≤ your target. " +
					"`pg_hardstorage list <deployment>` prints stop_lsn for every backup.",
			})
		// Intentionally NOT wrapping output.ErrUsage: this is a
		// conflict (target vs. backup range), not a flag-shape
		// error.  ExitCodeFor routes restore.target_* to
		// ExitConflict so cron-driven restores can distinguish
		// config errors from transient infrastructure failures.
	}
	return nil
}

// beforeOrAt returns the human phrasing for the relationship the
// reachability error reports.  Kept as a small helper so the error
// message reads naturally in both inclusive and exclusive modes
// without an inline ternary.
func beforeOrAt(target, stop pglogrepl.LSN, inclusive bool) string {
	if target == stop && !inclusive {
		return "AT (with exclusive stop, equivalent to BEFORE)"
	}
	return "BEFORE"
}

// preflightSnapshot fills p.PreflightOK + p.PreflightIssues by
// inspecting target without mutating it. Mirrors what preflightTarget
// does for Restore, but never returns an error.
func preflightSnapshot(target string, p *Plan) {
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, stdfs.ErrNotExist) {
			// Target doesn't exist yet — the real Restore will create
			// it. Plan-time this is fine.
			p.PreflightOK = true
			return
		}
		p.PreflightIssues = append(p.PreflightIssues,
			fmt.Sprintf("stat target: %v", err))
		return
	}
	if !info.IsDir() {
		p.PreflightIssues = append(p.PreflightIssues,
			fmt.Sprintf("target %q exists but is not a directory", target))
		return
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		p.PreflightIssues = append(p.PreflightIssues,
			fmt.Sprintf("read target %q: %v", target, err))
		return
	}
	if len(entries) > 0 {
		p.PreflightIssues = append(p.PreflightIssues,
			fmt.Sprintf("target %q is not empty (%d entries) — restore will need --force",
				target, len(entries)))
		return
	}
	p.PreflightOK = true
}

func validatePlanOptions(o *PlanOptions) error {
	if o.RepoURL == "" {
		return output.NewError("usage.missing_repo_url",
			"plan: RepoURL is required").Wrap(output.ErrUsage)
	}
	if o.Deployment == "" {
		return output.NewError("usage.missing_deployment",
			"plan: Deployment is required").Wrap(output.ErrUsage)
	}
	if o.BackupID == "" {
		return output.NewError("usage.missing_backup_id",
			"plan: BackupID is required").Wrap(output.ErrUsage)
	}
	if o.TargetDir == "" {
		return output.NewError("usage.missing_target_dir",
			"plan: TargetDir is required").Wrap(output.ErrUsage)
	}
	if o.Verifier == nil {
		return output.NewError("usage.missing_verifier",
			"plan: Verifier is required").Wrap(output.ErrUsage)
	}
	return nil
}
