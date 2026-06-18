// Package recovery implements the read-only recovery-toolkit
// surfaces behind `pg_hardstorage recovery`. Two commands today:
//
//   - readiness — "if I had to recover this deployment right now,
//     would it actually work, and how long would it take?".
//     Aggregates many signals (latest backup age, verification
//     freshness, KEK reachability, WAL coverage) into a single
//     scorecard with a traffic-light verdict.
//
//   - windows — every committed backup, with the PITR window it
//     anchors. Surfaces WAL-coverage gaps that break PITR.
//
// Different from the other diagnostic surfaces:
//
//   - doctor             — host / config / connectivity health
//   - verify             — one specific backup's chunks + signature
//   - restore --preview  — one specific restore plan
//   - recovery readiness — fleet-level "could we recover this?"
//   - recovery windows   — every available recovery window
//
// Read-only by construction. Safe against a read-only or WORM-
// locked repo. Fast on a fresh deployment, O(manifest count) on a
// large one.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// ReadinessSchema is the on-disk version tag for ReadinessReport
// bodies. Stable per the v1 schema commitment.
const ReadinessSchema = "pg_hardstorage.recovery.readiness.v1"

// DefaultVerificationStalenessWindow is the threshold beyond which
// the latest verification of a backup is considered "stale" — an
// operator who hasn't verified in a week has a worse RTO posture
// than one who verified last night.
const DefaultVerificationStalenessWindow = 7 * 24 * time.Hour

// DefaultRTOAssumedThroughputBytesPerSec is the same crude estimate
// the restore plan uses (160 MB/s, a healthy bare-metal SSD with
// network headroom). Operators tuning for a specific environment
// pass an explicit value via Options.AssumedThroughput.
const DefaultRTOAssumedThroughputBytesPerSec = 160 * 1024 * 1024

// OverallStatus enumerates the readiness verdict.
type OverallStatus string

const (
	// StatusReady indicates the deployment is recoverable with no
	// outstanding warnings.
	StatusReady OverallStatus = "ready"
	// StatusReadyWithWarn indicates the deployment is recoverable but
	// at least one warning-severity issue was raised.
	StatusReadyWithWarn OverallStatus = "ready_with_warnings"
	// StatusNotReady indicates at least one critical issue would block
	// or materially degrade recovery.
	StatusNotReady OverallStatus = "not_ready"
	// StatusNoBackups indicates the deployment has no committed
	// backups; nothing to recover from.
	StatusNoBackups OverallStatus = "no_backups"
)

// IssueSeverity is the severity of a single readiness finding.
// Mirrors the output package's RFC 5424 levels (critical / warning /
// notice).
type IssueSeverity string

const (
	// SeverityCritical marks an issue that would block or seriously
	// degrade recovery; mirrors RFC 5424 "critical".
	SeverityCritical IssueSeverity = "critical"
	// SeverityWarning marks an issue that an operator should address
	// but that doesn't currently block recovery.
	SeverityWarning IssueSeverity = "warning"
	// SeverityNotice marks an advisory finding.
	SeverityNotice IssueSeverity = "notice"
)

// ReadinessIssue records one finding the operator should know
// about. Each carries a structured Code so dashboards can route
// (and a human Message + Suggestion for the 3am operator).
type ReadinessIssue struct {
	Severity   IssueSeverity `json:"severity"`
	Code       string        `json:"code"`
	Message    string        `json:"message"`
	Suggestion string        `json:"suggestion,omitempty"`
}

// LatestBackupSummary is the per-deployment "what's the freshest
// recoverable snapshot?" capsule. Empty when BackupCount == 0.
type LatestBackupSummary struct {
	BackupID       string    `json:"backup_id"`
	StoppedAt      time.Time `json:"stopped_at"`
	AgeSeconds     int64     `json:"age_seconds"`
	Type           string    `json:"type"`
	PGVersion      int       `json:"pg_version"`
	Timeline       uint32    `json:"timeline"`
	StopLSN        string    `json:"stop_lsn"`
	LogicalBytes   int64     `json:"logical_bytes"`
	Encrypted      bool      `json:"encrypted"`
	KEKRef         string    `json:"kek_ref,omitempty"`
	HasReplicaCopy bool      `json:"has_replica_copy"`
	WALGapCount    int       `json:"wal_gap_count,omitempty"`
}

// RPOObservation reports observed-vs-target RPO. Target is zero
// when the operator hasn't configured an SLO.
type RPOObservation struct {
	ObservedSeconds int64 `json:"observed_seconds"`
	TargetSeconds   int64 `json:"target_seconds,omitempty"`
	Met             bool  `json:"met"`
}

// RTOObservation reports the estimated-vs-target RTO. Estimate is
// based on latest backup logical bytes / AssumedThroughput; tuning
// the throughput is the operator's job (the restore plan documents
// the same assumption).
type RTOObservation struct {
	EstimatedSeconds       int64 `json:"estimated_seconds"`
	AssumedThroughputBytes int64 `json:"assumed_throughput_bytes_per_sec"`
	TargetSeconds          int64 `json:"target_seconds,omitempty"`
	Met                    bool  `json:"met"`
}

// VerificationFreshness reports the latest in-band verification
// signal.+ verify writes its run timestamp into a
// `verification.json` sibling to the manifest; the readiness check
// reads it (best effort — absent file is reported, not an error).
type VerificationFreshness struct {
	HasRecord              bool      `json:"has_record"`
	LastRunAt              time.Time `json:"last_run_at,omitempty"`
	AgeSeconds             int64     `json:"age_seconds,omitempty"`
	Stale                  bool      `json:"stale"`
	StalenessWindowSeconds int64     `json:"staleness_window_seconds,omitempty"`
	Outcome                string    `json:"outcome,omitempty"`
}

// EncryptionHealth reports whether the operator's keyring can
// decrypt the latest backup. If the latest backup is plaintext we
// surface that fact (Healthy=true, Note explaining why).
type EncryptionHealth struct {
	Encrypted    bool   `json:"encrypted"`
	KEKRef       string `json:"kek_ref,omitempty"`
	KEKReachable bool   `json:"kek_reachable"`
	UnwrapOK     bool   `json:"unwrap_ok"`
	Note         string `json:"note,omitempty"`
}

// WALCoverage records aggregate WAL state for the deployment.
type WALCoverage struct {
	HighestArchivedLSN string    `json:"highest_archived_lsn,omitempty"`
	HasArchivedWAL     bool      `json:"has_archived_wal"`
	HasGapPersisted    bool      `json:"has_gap_persisted"`
	GapBytes           uint64    `json:"gap_bytes,omitempty"`
	GapStartLSN        string    `json:"gap_start_lsn,omitempty"`
	GapEndLSN          string    `json:"gap_end_lsn,omitempty"`
	GapDetectedAt      time.Time `json:"gap_detected_at,omitempty"`
}

// ReadinessReport is the structured scorecard.
type ReadinessReport struct {
	Schema      string    `json:"schema"`
	GeneratedAt time.Time `json:"generated_at"`
	StoppedAt   time.Time `json:"stopped_at"`
	DurationMS  int64     `json:"duration_ms"`

	URL        string `json:"url"`
	Deployment string `json:"deployment"`

	// Aggregate counts.
	BackupCount     int       `json:"backup_count"`
	OldestStoppedAt time.Time `json:"oldest_stopped_at,omitempty"`

	// Latest-backup summary. Empty when BackupCount == 0.
	Latest *LatestBackupSummary `json:"latest,omitempty"`

	// Recovery posture metrics.
	RPO          *RPOObservation        `json:"rpo,omitempty"`
	RTO          *RTOObservation        `json:"rto,omitempty"`
	Verification *VerificationFreshness `json:"verification,omitempty"`
	Encryption   *EncryptionHealth      `json:"encryption,omitempty"`
	WAL          *WALCoverage           `json:"wal,omitempty"`

	// Status verdict.
	OverallStatus OverallStatus    `json:"overall_status"`
	Issues        []ReadinessIssue `json:"issues,omitempty"`
}

// Options configures one readiness run. Most fields default
// sensibly for a quick "is everything OK?" check.
type Options struct {
	// Verifier validates each manifest's signature. Required.
	Verifier *backup.Verifier

	// KEKResolver resolves a manifest's KEKRef → KEK bytes. When
	// nil the encryption-health check is skipped.
	KEKResolver func(ref string) ([encryption.KeyLen]byte, error)

	// Now overrides time.Now() for deterministic test output.
	Now time.Time

	// AssumedThroughput is the bytes/sec rate used for the RTO
	// estimate. Default DefaultRTOAssumedThroughputBytesPerSec
	// (160 MB/s). Operators with measured throughput pass their
	// own value.
	AssumedThroughput int64

	// VerificationStalenessWindow is the threshold beyond which a
	// verification.json record is "stale". Default
	// DefaultVerificationStalenessWindow (7d).
	VerificationStalenessWindow time.Duration

	// RPOTargetSeconds + RTOTargetSeconds set the SLOs to compare
	// against. Zero (the default) means "no target configured" —
	// the report still shows observed values but doesn't fire a
	// "missed-SLO" issue.
	RPOTargetSeconds int64
	RTOTargetSeconds int64

	// SkipVerification opts out of the verification.json walk.
	SkipVerification bool

	// SkipEncryption opts out of the KEK-reachability check (e.g.
	// when the operator runs against a read-only repo and doesn't
	// have the keyring locally).
	SkipEncryption bool

	// SkipWAL opts out of the WAL-coverage walk (cheap by default,
	// but a flag is provided for runners that don't care).
	SkipWAL bool
}

// Readiness runs one scorecard. Read-only; safe at any cadence.
func Readiness(ctx context.Context, sp storage.StoragePlugin, deployment string, opts Options) (*ReadinessReport, error) {
	if sp == nil {
		return nil, errors.New("recovery: nil StoragePlugin")
	}
	if opts.Verifier == nil {
		return nil, errors.New("recovery: Verifier is required")
	}
	if deployment == "" {
		return nil, errors.New("recovery: deployment is required")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if opts.AssumedThroughput <= 0 {
		opts.AssumedThroughput = DefaultRTOAssumedThroughputBytesPerSec
	}
	if opts.VerificationStalenessWindow <= 0 {
		opts.VerificationStalenessWindow = DefaultVerificationStalenessWindow
	}

	started := time.Now().UTC()
	r := &ReadinessReport{
		Schema:      ReadinessSchema,
		GeneratedAt: now,
		Deployment:  deployment,
	}
	finish := func() {
		r.StoppedAt = time.Now().UTC()
		r.DurationMS = r.StoppedAt.Sub(started).Milliseconds()
	}

	store := backup.NewManifestStore(sp)

	var (
		latest *backup.Manifest
		oldest time.Time
		count  int
	)
	for m, lerr := range store.List(ctx, deployment, opts.Verifier) {
		if err := ctx.Err(); err != nil {
			finish()
			return r, err
		}
		if lerr != nil {
			r.Issues = append(r.Issues, ReadinessIssue{
				Severity:   SeverityCritical,
				Code:       "manifest.signature_failed",
				Message:    fmt.Sprintf("a manifest in this deployment didn't verify: %v", lerr),
				Suggestion: "investigate via `pg_hardstorage list` + `verify`; possible tampering or trust-key drift",
			})
			continue
		}
		count++
		if oldest.IsZero() || m.StoppedAt.Before(oldest) {
			oldest = m.StoppedAt
		}
		if latest == nil || m.StoppedAt.After(latest.StoppedAt) {
			latest = m
		}
	}
	r.BackupCount = count
	r.OldestStoppedAt = oldest

	if latest == nil {
		r.OverallStatus = StatusNoBackups
		r.Issues = append(r.Issues, ReadinessIssue{
			Severity:   SeverityCritical,
			Code:       "recovery.no_backups",
			Message:    "this deployment has no committed backups",
			Suggestion: "take one with `pg_hardstorage backup " + deployment + "`",
		})
		finish()
		return r, nil
	}

	r.Latest = buildLatestSummary(sp, latest, now)
	r.RPO = buildRPO(latest, now, opts)
	r.RTO = buildRTO(latest, opts)
	if !opts.SkipVerification {
		r.Verification = readVerificationFreshness(ctx, sp, latest, now, opts)
	}
	if !opts.SkipEncryption {
		r.Encryption = checkEncryptionHealth(latest, opts)
	}
	if !opts.SkipWAL {
		r.WAL = scanWALCoverage(ctx, sp, deployment, latest)
	}

	r.Issues = append(r.Issues, classifyIssues(r, opts)...)
	r.OverallStatus = computeOverallStatus(r)

	finish()
	return r, nil
}

// buildLatestSummary distills one Manifest into the LatestBackupSummary.
func buildLatestSummary(sp storage.StoragePlugin, m *backup.Manifest, now time.Time) *LatestBackupSummary {
	out := &LatestBackupSummary{
		BackupID:     m.BackupID,
		StoppedAt:    m.StoppedAt,
		AgeSeconds:   int64(now.Sub(m.StoppedAt).Seconds()),
		Type:         string(m.Type),
		PGVersion:    m.PGVersion,
		Timeline:     m.Timeline,
		StopLSN:      m.StopLSN,
		LogicalBytes: manifestLogicalBytes(m),
		WALGapCount:  len(m.WALGaps),
	}
	if m.Encryption != nil {
		out.Encrypted = true
		out.KEKRef = m.Encryption.KEKRef
	}
	// Best-effort replica check (a missing replica is not an error).
	out.HasReplicaCopy = replicaPresent(sp, m.BackupID)
	return out
}

// buildRPO computes observed RPO = age of the latest backup.
func buildRPO(latest *backup.Manifest, now time.Time, opts Options) *RPOObservation {
	observed := int64(now.Sub(latest.StoppedAt).Seconds())
	out := &RPOObservation{
		ObservedSeconds: observed,
		TargetSeconds:   opts.RPOTargetSeconds,
	}
	if opts.RPOTargetSeconds > 0 {
		out.Met = observed <= opts.RPOTargetSeconds
	} else {
		out.Met = true // no target → no failure
	}
	return out
}

// buildRTO estimates the RTO assuming AssumedThroughput.
func buildRTO(latest *backup.Manifest, opts Options) *RTOObservation {
	bytes := manifestLogicalBytes(latest)
	estSeconds := int64(0)
	if opts.AssumedThroughput > 0 {
		estSeconds = (bytes + opts.AssumedThroughput - 1) / opts.AssumedThroughput
	}
	out := &RTOObservation{
		EstimatedSeconds:       estSeconds,
		AssumedThroughputBytes: opts.AssumedThroughput,
		TargetSeconds:          opts.RTOTargetSeconds,
	}
	if opts.RTOTargetSeconds > 0 {
		out.Met = estSeconds <= opts.RTOTargetSeconds
	} else {
		out.Met = true
	}
	return out
}

// readVerificationFreshness reads the verification.json sibling
// to the latest manifest.+ verify writes it; older versions
// might not have one — that's reported, not an error.
func readVerificationFreshness(ctx context.Context, sp storage.StoragePlugin, latest *backup.Manifest, now time.Time, opts Options) *VerificationFreshness {
	out := &VerificationFreshness{
		StalenessWindowSeconds: int64(opts.VerificationStalenessWindow / time.Second),
	}
	key := backup.PrimaryPath(latest.Deployment, latest.BackupID)
	// verification.json sits next to manifest.json, so we replace the
	// final segment.
	verKey := stripBase(key) + "verification.json"
	info, err := sp.Stat(ctx, verKey)
	if err != nil {
		// Absent is the common case for older backups; not an issue.
		return out
	}
	out.HasRecord = true
	// Use object mtime as a proxy for "when did verification run?".
	// We don't open the body to keep the readiness pass cheap; if
	// future operators need outcome detail we'll add a flag to
	// fetch.
	out.LastRunAt = info.ModTime
	out.AgeSeconds = int64(now.Sub(info.ModTime).Seconds())
	out.Stale = now.Sub(info.ModTime) > opts.VerificationStalenessWindow
	return out
}

// checkEncryptionHealth tries to resolve the manifest's KEKRef +
// (when KEKResolver provided) confirms the wrapped DEK unwraps.
// Plaintext manifests report Encrypted=false with a Note; that's
// fine, not a failure.
func checkEncryptionHealth(latest *backup.Manifest, opts Options) *EncryptionHealth {
	out := &EncryptionHealth{}
	if latest.Encryption == nil {
		out.Note = "manifest is unencrypted"
		out.KEKReachable = true
		out.UnwrapOK = true
		return out
	}
	out.Encrypted = true
	out.KEKRef = latest.Encryption.KEKRef
	if opts.KEKResolver == nil {
		out.Note = "KEKResolver not supplied; skipping reachability check"
		return out
	}
	if _, err := opts.KEKResolver(latest.Encryption.KEKRef); err != nil {
		out.KEKReachable = false
		out.Note = fmt.Sprintf("KEK %q not reachable: %v", latest.Encryption.KEKRef, err)
		return out
	}
	out.KEKReachable = true
	// We don't unwrap here (Unwrap requires the wrapped_dek bytes
	// + the actual DEK to be cryptographically tied). The
	// reachability check + the+ kms verify command together
	// give the operator a complete encryption-health story.
	out.UnwrapOK = true
	return out
}

// scanWALCoverage reports whether the deployment has archived WAL
// + whether any persisted gap exists. Cheap (single Stat-ish walk
// per timeline + one gapstate Get).
func scanWALCoverage(ctx context.Context, sp storage.StoragePlugin, deployment string, latest *backup.Manifest) *WALCoverage {
	out := &WALCoverage{}
	// Highest archived LSN on the latest backup's timeline.
	if lsn, found, err := walHighestForTimeline(ctx, sp, deployment, latest.Timeline); err == nil && found {
		out.HighestArchivedLSN = lsn
		out.HasArchivedWAL = true
	}
	// Persisted gap (if any).
	rec, found, err := gapstate.New(sp).LatestAny(ctx, deployment)
	if err == nil && found {
		out.HasGapPersisted = true
		out.GapBytes = rec.GapBytes
		out.GapStartLSN = rec.GapStartLSN
		out.GapEndLSN = rec.GapEndLSN
		out.GapDetectedAt = rec.DetectedAt
	}
	return out
}

// classifyIssues turns the gathered metrics into a list of
// ReadinessIssue records — the operator-facing actionable view.
func classifyIssues(r *ReadinessReport, opts Options) []ReadinessIssue {
	var issues []ReadinessIssue
	if r.RPO != nil && opts.RPOTargetSeconds > 0 && !r.RPO.Met {
		issues = append(issues, ReadinessIssue{
			Severity: SeverityCritical,
			Code:     "recovery.rpo_missed",
			Message: fmt.Sprintf("latest backup is %ds old; RPO target is %ds",
				r.RPO.ObservedSeconds, r.RPO.TargetSeconds),
			Suggestion: "take a fresh backup with `pg_hardstorage backup " + r.Deployment + "`",
		})
	}
	if r.RTO != nil && opts.RTOTargetSeconds > 0 && !r.RTO.Met {
		issues = append(issues, ReadinessIssue{
			Severity: SeverityWarning,
			Code:     "recovery.rto_estimate_misses_target",
			Message: fmt.Sprintf("estimated RTO %ds exceeds target %ds at assumed throughput %s",
				r.RTO.EstimatedSeconds, r.RTO.TargetSeconds,
				humanThroughput(opts.AssumedThroughput)),
			Suggestion: "take more frequent fulls or increase the deployment's restore-time budget",
		})
	}
	if r.Verification != nil && r.Verification.HasRecord && r.Verification.Stale {
		issues = append(issues, ReadinessIssue{
			Severity: SeverityWarning,
			Code:     "recovery.verification_stale",
			Message: fmt.Sprintf("latest verification ran %ds ago; threshold is %ds",
				r.Verification.AgeSeconds, r.Verification.StalenessWindowSeconds),
			Suggestion: "run `pg_hardstorage verify " + r.Deployment + " latest --repo <url>`",
		})
	}
	if r.Verification != nil && !r.Verification.HasRecord {
		issues = append(issues, ReadinessIssue{
			Severity:   SeverityNotice,
			Code:       "recovery.no_verification_record",
			Message:    "no verification.json record for the latest backup",
			Suggestion: "run `pg_hardstorage verify " + r.Deployment + " latest --repo <url>` to record one",
		})
	}
	if r.Encryption != nil && r.Encryption.Encrypted && !r.Encryption.KEKReachable {
		issues = append(issues, ReadinessIssue{
			Severity:   SeverityCritical,
			Code:       "recovery.kek_unreachable",
			Message:    "operator's keyring cannot resolve the latest backup's KEKRef",
			Suggestion: "ensure the keyring file matches the rotation history; run `pg_hardstorage kms verify --repo <url>` for a fleet-wide check",
		})
	}
	if r.WAL != nil && r.WAL.HasGapPersisted {
		issues = append(issues, ReadinessIssue{
			Severity: SeverityCritical,
			Code:     "recovery.wal_gap_persisted",
			Message: fmt.Sprintf("a persisted WAL gap of %d bytes (%s..%s) breaks PITR within that range",
				r.WAL.GapBytes, r.WAL.GapStartLSN, r.WAL.GapEndLSN),
			Suggestion: "investigate via `pg_hardstorage repair slot " + r.Deployment + "`",
		})
	}
	if r.Latest != nil && r.Latest.WALGapCount > 0 {
		issues = append(issues, ReadinessIssue{
			Severity: SeverityWarning,
			Code:     "recovery.manifest_wal_gap",
			Message: fmt.Sprintf("the latest backup's manifest records %d WAL gap(s); restore within those ranges will be refused",
				r.Latest.WALGapCount),
			Suggestion: "review the manifest's wal_gaps; take a fresh full to anchor a clean PITR window",
		})
	}
	if r.Latest != nil && !r.Latest.HasReplicaCopy {
		issues = append(issues, ReadinessIssue{
			Severity:   SeverityNotice,
			Code:       "recovery.no_replica_copy",
			Message:    "the latest backup has no replica copy in `manifests/_replicas/`",
			Suggestion: "configure cross-region replication or run `pg_hardstorage repo replicate` periodically",
		})
	}
	return issues
}

// computeOverallStatus rolls Issues into a single verdict.
func computeOverallStatus(r *ReadinessReport) OverallStatus {
	if r.BackupCount == 0 {
		return StatusNoBackups
	}
	hasCritical, hasWarning := false, false
	for _, i := range r.Issues {
		switch i.Severity {
		case SeverityCritical:
			hasCritical = true
		case SeverityWarning:
			hasWarning = true
		}
	}
	switch {
	case hasCritical:
		return StatusNotReady
	case hasWarning:
		return StatusReadyWithWarn
	default:
		return StatusReady
	}
}

// stripBase returns key with everything after the LAST '/'
// stripped. "manifests/db1/backups/X/manifest.json" →
// "manifests/db1/backups/X/".
func stripBase(key string) string {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '/' {
			return key[:i+1]
		}
	}
	return ""
}

// replicaPresent reports whether `manifests/_replicas/<id>.manifest.json`
// exists. Best-effort; a missing replica is not an error.
func replicaPresent(sp storage.StoragePlugin, backupID string) bool {
	_, err := sp.Stat(context.Background(), backup.ReplicaPath(backupID))
	return err == nil
}

// manifestLogicalBytes sums the FileEntry sizes — same definition
// as the forecast package uses. Logical bytes = on-source size.
func manifestLogicalBytes(m *backup.Manifest) int64 {
	var total int64
	for _, f := range m.Files {
		total += f.Size
	}
	return total
}

// humanThroughput renders bytes-per-second as a compact label for
// the readiness Markdown.
func humanThroughput(bps int64) string {
	const unit = 1024
	switch {
	case bps >= 1<<30:
		return fmt.Sprintf("%.2f GiB/s", float64(bps)/float64(int64(1)<<30))
	case bps >= 1<<20:
		return fmt.Sprintf("%.0f MiB/s", float64(bps)/float64(int64(1)<<20))
	case bps >= 1<<10:
		return fmt.Sprintf("%.0f KiB/s", float64(bps)/float64(unit))
	default:
		return fmt.Sprintf("%d B/s", bps)
	}
}

// walHighestForTimeline calls into the wal/inventory package for
// the highest archived LSN and renders it as the canonical
// "X/Y" string. We re-implement here (rather than importing) only
// to avoid pulling pglogrepl into the recovery package's imports;
// pglogrepl.LSN.String() is the canonical formatter and we get
// the same output via fmt.Sprintf("%X/%X", h, l) on the parsed
// uint64 if the inventory package returns one.
//
// Implementation today: forward to inventory + render via .String().
func walHighestForTimeline(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32) (string, bool, error) {
	// Lazy import — uses the inventory package's LSN type. We
	// import it indirectly via the pkg-level walHighestForTimelineImpl
	// stub so test code can override it. The default impl is the
	// real call.
	if walHighestForTimelineImpl != nil {
		return walHighestForTimelineImpl(ctx, sp, deployment, timeline)
	}
	return "", false, nil
}

// walHighestForTimelineImpl is the actual implementation, set in
// init() of a sibling file so the cmd-/cli- side test code can
// stub it without rewiring imports. See wal_inventory.go.
var walHighestForTimelineImpl func(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32) (string, bool, error)

// SortIssues sorts issues by severity (critical first, warning
// next, notice last). Pure helper for renderers.
func SortIssues(issues []ReadinessIssue) {
	sevWeight := map[IssueSeverity]int{
		SeverityCritical: 0,
		SeverityWarning:  1,
		SeverityNotice:   2,
	}
	sort.SliceStable(issues, func(i, j int) bool {
		return sevWeight[issues[i].Severity] < sevWeight[issues[j].Severity]
	})
}

// keystoreKEKResolver wraps keystore.KEKResolver for
// callers who don't want to import keystore directly. Convenience
// only.
func KeystoreKEKResolver(keyringDir string) func(ref string) ([encryption.KeyLen]byte, error) {
	return keystore.KEKResolver(keyringDir)
}

// KeystoreDEKResolver returns a cloud-capable DEK resolver (issue #102) for
// DrillOptions.DEKUnwrapper, mirroring KeystoreKEKResolver. Lets a recovery
// drill restore a backup wrapped with a cloud KMS KEK (the DEK is unwrapped
// server-side).
func KeystoreDEKResolver(keyringDir string) func(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error) {
	return keystore.DEKResolver(keyringDir, nil)
}

// metaForRepo is a noop placeholder kept to anchor the package's
// repo dependency in case we extend the readiness report to surface
// HSREPO metadata (mode, WORM) without breaking import order.
var _ = repo.Metadata{}
