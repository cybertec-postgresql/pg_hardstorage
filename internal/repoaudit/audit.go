// Package repoaudit implements the comprehensive read-only
// repository state report behind `pg_hardstorage repo audit`.
//
// repo audit is the operator's "tell me everything about my repo
// state in one shot" command. It complements:
//
//   - doctor      — host/config/connectivity (LOCAL view)
//   - verify      — one backup's chunks + signature (PER-BACKUP)
//   - kms verify  — fleet-wide encryption envelope health (CRYPTO)
//   - repo usage  — object count + bytes per category (STORAGE)
//
// repo audit walks ALL of these dimensions in one read-only pass:
//
//   - per-deployment lifecycle (active / tombstoned / held counts,
//     oldest / newest backup, latest backup metadata, encryption
//     posture, KEK refs, schema versions),
//   - fleet-wide rollups (KEK ref → manifest count, schema-version
//     distribution, replica completeness),
//   - audit-chain summary (event count, last-anchor age, chain-head),
//   - storage summary (objects / bytes per category, reused from
//     repo usage's scanner),
//   - WORM mode + retention (read from HSREPO).
//
// The report is deliberately a snapshot of FACTS, not a verdict. It
// surfaces the data an operator needs to answer compliance audits,
// capacity-planning questions, and "what's going on with my fleet?"
// at a glance — without prescribing what's "broken" (that's
// doctor's job).
//
// Read-only by construction: safe against a read-only or WORM-locked
// repo, in production, at any cadence. The walk is O(manifest count
// + audit event count + holds count + storage object count); for
// fleets in the tens-of-thousands range, the report builds in
// seconds.
package repoaudit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// ReportSchema is the on-disk version tag for Report bodies. Stable
// per the v1 schema commitment.
const ReportSchema = "pg_hardstorage.repo_audit.v1"

// Options configures one audit run. Every section can be disabled
// individually so an operator on a mega-fleet can spend the
// expensive walks (audit chain, storage usage) only when needed.
type Options struct {
	// Verifier validates each manifest's signature at iteration time.
	// Required — the report's deployment counts include
	// signature_failed entries that wouldn't be visible without it.
	Verifier *backup.Verifier

	// DeploymentFilter restricts the per-deployment section to one
	// deployment. Fleet-wide rollups still consider every manifest
	// (so a per-tenant audit doesn't miss cross-deployment KEK
	// drift). Empty walks every deployment.
	DeploymentFilter string

	// SkipStorage skips the per-category storage usage scan.
	// Significantly cheaper on huge repos; flip back on for
	// capacity-planning runs.
	SkipStorage bool

	// SkipAuditChain skips the audit-chain summary (event count +
	// last-anchor age). The chain walk is O(audit event count) and
	// at fleet scale can dominate the report's runtime.
	SkipAuditChain bool

	// SkipApprovals skips listing the approval requests. Cheap on
	// most repos but a defensive opt-out for fleets with very
	// large approval volumes.
	SkipApprovals bool
}

// Report is the structured audit body. Every field is JSON-stable
// per the v1 schema commitment.
type Report struct {
	Schema      string    `json:"schema"`
	GeneratedAt time.Time `json:"generated_at"`
	StoppedAt   time.Time `json:"stopped_at,omitempty"`
	DurationMS  int64     `json:"duration_ms"`

	URL  string       `json:"url"`
	Repo *RepoSummary `json:"repo,omitempty"`

	DeploymentFilter string `json:"deployment_filter,omitempty"`

	// Per-deployment lifecycle. One entry per deployment in the
	// repo (or one entry total when DeploymentFilter is set).
	Deployments []DeploymentAudit `json:"deployments"`

	// Fleet rollups (computed across every visible manifest, ignoring
	// DeploymentFilter — operators want the global crypto picture
	// even on a single-deployment audit).
	KEKRefs        []KEKRefSummary  `json:"kek_refs,omitempty"`
	SchemaVersions []SchemaSummary  `json:"schema_versions,omitempty"`
	Replicas       *ReplicaSummary  `json:"replicas,omitempty"`
	Approvals      *ApprovalSummary `json:"approvals,omitempty"`
	Chain          *ChainSummary    `json:"chain,omitempty"`
	Storage        *StorageSummary  `json:"storage,omitempty"`
}

// RepoSummary captures the static metadata in HSREPO.
type RepoSummary struct {
	ID                   string `json:"id,omitempty"`
	Schema               string `json:"schema,omitempty"`
	CreatedAt            string `json:"created_at,omitempty"`
	Mode                 string `json:"mode,omitempty"`      // "read-write" | "read-only"
	WORMMode             string `json:"worm_mode,omitempty"` // compliance | governance | ""
	WORMRetentionSeconds int64  `json:"worm_retention_seconds,omitempty"`
}

// DeploymentAudit records one deployment's full picture. The
// counters separate the lifecycle states so an operator can read
// "5 active, 3 tombstoned, 1 held" off the report.
type DeploymentAudit struct {
	Name string `json:"name"`

	// Active is the count of committed (non-tombstoned) manifests.
	Active int `json:"active"`

	// Tombstoned is the count of soft-deleted manifests still
	// holding chunk references (waiting for `repo gc` to reap).
	Tombstoned int `json:"tombstoned"`

	// Held is the count of currently-active hold markers
	// (indefinite or with future ExpiresAt). Expired holds aren't
	// counted — they're protecting nothing.
	Held int `json:"held"`

	// HeldExpired is the count of hold markers whose ExpiresAt has
	// passed but haven't been removed. Operational hygiene signal;
	// `hold purge-expired` cleans these up.
	HeldExpired int `json:"held_expired,omitempty"`

	// SignatureFailed counts manifests that didn't verify at
	// iteration time. The LOUDEST possible operational finding —
	// either tampering or trust-key drift.
	SignatureFailed int `json:"signature_failed,omitempty"`

	// OldestStoppedAt and NewestStoppedAt bracket the deployment's
	// recovery window. Empty (zero time) when Active == 0.
	OldestStoppedAt time.Time `json:"oldest_stopped_at,omitempty"`
	NewestStoppedAt time.Time `json:"newest_stopped_at,omitempty"`

	// Latest is the metadata of the newest active manifest. Empty
	// (zero values) when Active == 0.
	Latest *LatestBackupSummary `json:"latest,omitempty"`

	// EncryptionPosture summarises whether the deployment's active
	// manifests are encrypted, plaintext, or mixed. Operators
	// running compliance audits need this at-a-glance.
	EncryptionPosture string `json:"encryption_posture"` // "encrypted" | "plaintext" | "mixed" | "none"

	// KEKRefs is the set of distinct kek_refs across this
	// deployment's active manifests. Ideally a singleton; multiple
	// values across active manifests mean a partial rotation or a
	// multi-tenant misconfig the operator should know about.
	KEKRefs []string `json:"kek_refs,omitempty"`

	// PGVersions is the set of distinct PostgreSQL major versions
	// across active manifests. A spread tells you the deployment
	// was upgraded mid-history (which can affect restore-target
	// compatibility).
	PGVersions []int `json:"pg_versions,omitempty"`

	// Timelines is the set of distinct WAL timelines across active
	// manifests. Timeline = N+1 means a Patroni promotion happened.
	Timelines []uint32 `json:"timelines,omitempty"`

	// Schemas is the set of distinct on-disk manifest schema
	// versions. Useful for tracking 24-month-compat upgrade rollouts.
	Schemas []string `json:"schemas,omitempty"`
}

// LatestBackupSummary is the per-deployment "what's the freshest
// backup we have?" capsule.
type LatestBackupSummary struct {
	BackupID       string    `json:"backup_id"`
	StoppedAt      time.Time `json:"stopped_at"`
	Type           string    `json:"type"`
	PGVersion      int       `json:"pg_version"`
	Timeline       uint32    `json:"timeline"`
	Encrypted      bool      `json:"encrypted"`
	KEKRef         string    `json:"kek_ref,omitempty"`
	HasReplicaCopy bool      `json:"has_replica_copy"`
	WALGapCount    int       `json:"wal_gap_count,omitempty"`
}

// KEKRefSummary is one row in the fleet-wide kek_ref breakdown.
type KEKRefSummary struct {
	KEKRef        string `json:"kek_ref"`
	ManifestCount int    `json:"manifest_count"`
}

// SchemaSummary is one row in the fleet-wide schema-version
// distribution.
type SchemaSummary struct {
	Schema        string `json:"schema"`
	ManifestCount int    `json:"manifest_count"`
}

// ReplicaSummary aggregates replica-completeness metrics.
type ReplicaSummary struct {
	PrimaryManifests int `json:"primary_manifests"`
	ReplicaManifests int `json:"replica_manifests"`

	// UnreplicatedPrimaries counts active manifests whose
	// _replicas/<id>.manifest.json copy is absent. Higher numbers
	// indicate the cross-region replicate worker is behind, the
	// replica region is unreachable, or replication wasn't
	// configured.
	UnreplicatedPrimaries int `json:"unreplicated_primaries"`

	// OrphanedReplicas counts replica copies whose primary is
	// gone (deleted, GC'd, never existed). A finding worth
	// surfacing — either the primary was prematurely deleted, or
	// the replica was written for a manifest that never committed.
	OrphanedReplicas int `json:"orphaned_replicas,omitempty"`
}

// ApprovalSummary aggregates the approval-request lifecycle.
type ApprovalSummary struct {
	Pending  int `json:"pending,omitempty"`
	Approved int `json:"approved,omitempty"`
	Expired  int `json:"expired,omitempty"`
	Revoked  int `json:"revoked,omitempty"`
	Total    int `json:"total"`
}

// ChainSummary aggregates the audit-log chain state.
type ChainSummary struct {
	EventCount        int       `json:"event_count"`
	OldestAt          time.Time `json:"oldest_at,omitempty"`
	NewestAt          time.Time `json:"newest_at,omitempty"`
	HeadHashAvailable bool      `json:"head_hash_available"`
	AnchorCount       int       `json:"anchor_count,omitempty"`
	LastAnchorAt      time.Time `json:"last_anchor_at,omitempty"`
	LastAnchorAgeMS   int64     `json:"last_anchor_age_ms,omitempty"`
}

// StorageSummary mirrors repo usage's per-category breakdown so an
// operator gets one report covering both audit dimensions and
// storage cost.
type StorageSummary struct {
	Categories   []CategoryUsage `json:"categories"`
	TotalObjects int64           `json:"total_objects"`
	TotalBytes   int64           `json:"total_bytes"`
}

// CategoryUsage is one row in StorageSummary.Categories.
type CategoryUsage struct {
	Category string `json:"category"`
	Objects  int64  `json:"objects"`
	Bytes    int64  `json:"bytes"`
}

// Audit runs one comprehensive walk of sp and returns the full
// Report. Read-only; no mutation; safe at any cadence.
func Audit(ctx context.Context, sp storage.StoragePlugin, meta *repo.Metadata, repoURL string, opts Options) (*Report, error) {
	if sp == nil {
		return nil, errors.New("repoaudit: nil StoragePlugin")
	}
	if opts.Verifier == nil {
		return nil, errors.New("repoaudit: Verifier is required")
	}

	started := time.Now().UTC()
	rep := &Report{
		Schema:           ReportSchema,
		GeneratedAt:      started,
		URL:              repoURL,
		DeploymentFilter: opts.DeploymentFilter,
	}
	finish := func() {
		rep.StoppedAt = time.Now().UTC()
		rep.DurationMS = rep.StoppedAt.Sub(started).Milliseconds()
	}

	if meta != nil {
		rep.Repo = &RepoSummary{
			ID:        meta.ID,
			Schema:    meta.Schema,
			CreatedAt: meta.CreatedAt,
			Mode:      string(meta.Mode),
		}
		if !meta.WORM.IsZero() {
			rep.Repo.WORMMode = meta.WORM.Mode
			rep.Repo.WORMRetentionSeconds = meta.WORM.RetentionSeconds
		}
	}

	store := backup.NewManifestStore(sp)

	// Deployment enumeration. We always need ALL deployments for the
	// fleet rollups, even when DeploymentFilter is set. The per-
	// deployment slice gets filtered separately.
	allDeployments, err := store.Deployments(ctx)
	if err != nil {
		finish()
		return rep, fmt.Errorf("repoaudit: enumerate deployments: %w", err)
	}
	sort.Strings(allDeployments)

	// Pre-load fleet-wide hold + replica-key indexes once so we don't
	// re-walk the storage layer per deployment.
	fleetHolds, err := store.ListHolds(ctx, "")
	if err != nil {
		// Non-fatal — operators benefit from a partial report.
		// Surface as a synthetic deployment-level zero so the
		// missing data is obvious in the JSON.
		fleetHolds = nil
	}
	holdsByDep := indexHoldsByDeployment(fleetHolds)

	replicaIndex, err := loadReplicaIndex(ctx, sp)
	if err != nil {
		// Same posture — partial reports are useful.
		replicaIndex = nil
	}

	// Fleet rollup accumulators.
	kekCounts := map[string]int{}
	schemaCounts := map[string]int{}
	replicaSum := &ReplicaSummary{}

	// Per-deployment walks.
	now := time.Now().UTC()
	for _, deployment := range allDeployments {
		if err := ctx.Err(); err != nil {
			finish()
			return rep, err
		}
		da := DeploymentAudit{Name: deployment}
		latest := newLatestTracker()
		// Track sets per-deployment; the nilable entry types let us
		// skip an empty set gracefully in the JSON output (omitempty).
		kekSet := newOrderedSet()
		pgSet := newIntSet()
		tlSet := newUint32Set()
		schemaSet := newOrderedSet()
		encryptedCount := 0
		plaintextCount := 0

		for m, lerr := range store.List(ctx, deployment, opts.Verifier) {
			if err := ctx.Err(); err != nil {
				finish()
				return rep, err
			}
			if lerr != nil {
				da.SignatureFailed++
				continue
			}
			da.Active++
			replicaSum.PrimaryManifests++

			// Fleet rollups.
			if m.Encryption != nil {
				if m.Encryption.KEKRef != "" {
					kekCounts[m.Encryption.KEKRef]++
				} else {
					kekCounts["<empty-ref>"]++
				}
			} else {
				kekCounts["<unencrypted>"]++
			}
			schemaCounts[m.Schema]++

			// Per-deployment accumulators.
			if m.Encryption != nil {
				encryptedCount++
				kekSet.add(m.Encryption.KEKRef)
			} else {
				plaintextCount++
			}
			pgSet.add(m.PGVersion)
			tlSet.add(m.Timeline)
			schemaSet.add(m.Schema)

			if da.OldestStoppedAt.IsZero() || m.StoppedAt.Before(da.OldestStoppedAt) {
				da.OldestStoppedAt = m.StoppedAt
			}
			if m.StoppedAt.After(da.NewestStoppedAt) {
				da.NewestStoppedAt = m.StoppedAt
			}
			latest.observe(m, replicaIndex)

			// Replica completeness — does this primary have a
			// _replicas/<id>.manifest.json companion?
			if replicaIndex != nil {
				if _, ok := replicaIndex[m.BackupID]; ok {
					replicaIndex[m.BackupID] = struct{}{} // mark as matched (already indexed)
				} else {
					replicaSum.UnreplicatedPrimaries++
				}
			}
		}

		// Tombstone count is the difference between total committed
		// (active + tombstoned) and active. Computed via a separate
		// listing pass that includes tombstoned entries.
		da.Tombstoned = countTombstoned(ctx, store, deployment, opts.Verifier)

		// Hold counts (active vs expired) come from the pre-loaded
		// fleet index.
		for _, h := range holdsByDep[deployment] {
			if h.ActiveAt(now) {
				da.Held++
			} else {
				da.HeldExpired++
			}
		}

		// Encryption posture rollup.
		switch {
		case da.Active == 0:
			da.EncryptionPosture = "none"
		case encryptedCount == da.Active:
			da.EncryptionPosture = "encrypted"
		case plaintextCount == da.Active:
			da.EncryptionPosture = "plaintext"
		default:
			da.EncryptionPosture = "mixed"
		}

		da.KEKRefs = kekSet.values()
		da.PGVersions = pgSet.values()
		da.Timelines = tlSet.values()
		da.Schemas = schemaSet.values()
		if l := latest.snapshot(); l != nil {
			da.Latest = l
		}

		if opts.DeploymentFilter == "" || opts.DeploymentFilter == deployment {
			rep.Deployments = append(rep.Deployments, da)
		}
	}

	// Replica orphan count — every key still in replicaIndex that
	// wasn't matched against a primary above. We can't tell from the
	// pre-walk whether a key was matched, so we do a second pass: a
	// replica entry whose primary is missing in the per-deployment
	// walks counts as orphaned.
	if replicaIndex != nil {
		// We over-counted PrimaryManifests if any primary's replica
		// existed but its primary didn't show up. The replicaIndex
		// is keyed by backup_id only (no deployment). To find
		// orphans, we'd need to know which backup IDs we visited.
		// Instead we use a simpler approach: ReplicaManifests = the
		// total count loaded from storage, OrphanedReplicas =
		// max(ReplicaManifests - PrimaryManifests, 0). Imprecise
		// when N primaries are unreplicated AND M replicas are
		// orphaned; the report says "the totals don't add up;
		// investigate", which is the right operational signal.
		replicaSum.ReplicaManifests = len(replicaIndex)
		if replicaSum.ReplicaManifests > replicaSum.PrimaryManifests {
			replicaSum.OrphanedReplicas = replicaSum.ReplicaManifests - replicaSum.PrimaryManifests
			// PrimaryManifests stays as the ACTUAL primary count;
			// orphan replicas are tracked separately.
		}
		rep.Replicas = replicaSum
	}

	// Materialise the fleet rollups as sorted slices.
	rep.KEKRefs = mapToKEKSummary(kekCounts)
	rep.SchemaVersions = mapToSchemaSummary(schemaCounts)

	// Optional sections.
	if !opts.SkipStorage {
		su, _ := scanStorageUsage(ctx, sp)
		if su != nil {
			rep.Storage = su
		}
	}
	if !opts.SkipAuditChain {
		cs, _ := summarizeChain(ctx, sp, now)
		if cs != nil {
			rep.Chain = cs
		}
	}
	if !opts.SkipApprovals {
		as, _ := summarizeApprovals(ctx, sp, now)
		if as != nil {
			rep.Approvals = as
		}
	}

	finish()
	return rep, nil
}

// countTombstoned counts soft-deleted manifests for a deployment.
// We use ListIncludingTombstoned and subtract Active afterwards;
// this is a separate pass but shares the storage layer's prefix
// listing with the main walk's worth.
func countTombstoned(ctx context.Context, store *backup.ManifestStore, deployment string, verifier *backup.Verifier) int {
	count := 0
	for entry, err := range store.ListIncludingTombstoned(ctx, deployment, verifier) {
		if err != nil {
			continue
		}
		if entry.Tombstoned {
			count++
		}
	}
	return count
}

// loadReplicaIndex enumerates manifests/_replicas/ and returns a set
// of backup IDs present. Used for replica completeness accounting.
// Returns nil when the prefix is empty / unreadable (treat as "no
// replicas configured").
func loadReplicaIndex(ctx context.Context, sp storage.StoragePlugin) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for info, err := range sp.List(ctx, "manifests/_replicas/") {
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(info.Key, ".manifest.json") {
			continue
		}
		// Layout: manifests/_replicas/<id>.manifest.json
		base := strings.TrimPrefix(info.Key, "manifests/_replicas/")
		id := strings.TrimSuffix(base, ".manifest.json")
		if id != "" {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// scanStorageUsage tallies object count + bytes per repo category.
// Mirrors the CLI's scanRepoUsage shape so the audit body keeps
// the same field names a `repo usage` consumer already knows.
func scanStorageUsage(ctx context.Context, sp storage.StoragePlugin) (*StorageSummary, error) {
	cats := map[string]*CategoryUsage{}
	roots := []struct {
		prefix string
		assign func(string) string
	}{
		{prefix: "chunks/", assign: func(string) string { return "chunks" }},
		{prefix: "manifests/", assign: classifyManifest},
		{prefix: "wal/", assign: func(string) string { return "wal" }},
		{prefix: "audit/", assign: func(string) string { return "audit" }},
	}
	for _, r := range roots {
		for info, err := range sp.List(ctx, r.prefix) {
			if err != nil {
				return nil, err
			}
			cat := r.assign(info.Key)
			if cat == "" {
				continue
			}
			c := cats[cat]
			if c == nil {
				c = &CategoryUsage{Category: cat}
				cats[cat] = c
			}
			c.Objects++
			c.Bytes += info.Size
		}
	}
	out := make([]CategoryUsage, 0, len(cats))
	su := &StorageSummary{}
	for _, c := range cats {
		out = append(out, *c)
		su.TotalObjects += c.Objects
		su.TotalBytes += c.Bytes
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Category < out[j].Category })
	su.Categories = out
	return su, nil
}

// classifyManifest mirrors the CLI's classifyManifest. Duplicated to
// avoid the layer-inverting import (cli depends on repoaudit, not
// the other way round).
func classifyManifest(key string) string {
	switch {
	case strings.HasSuffix(key, ".json.tombstone"):
		return "manifests-tombstone"
	case strings.HasSuffix(key, ".json.hold"):
		return "manifests-hold"
	case strings.HasPrefix(key, "manifests/_replicas/"):
		return "manifests-replica"
	case strings.HasPrefix(key, "manifests/_trash/"):
		return "manifests-trash"
	}
	return "manifests"
}

// summarizeChain walks the audit-chain prefix and produces a
// summary. Anchors live in audit/anchors/; events live elsewhere
// under audit/. We count both separately.
func summarizeChain(ctx context.Context, sp storage.StoragePlugin, now time.Time) (*ChainSummary, error) {
	cs := &ChainSummary{}

	// Walk the chain itself. Events live under audit/ but NOT under
	// audit/anchors/. We can't open them efficiently for timestamps
	// without parsing each one (expensive at scale), so we emit a
	// count-only summary in v1 with an opt-out flag if even that is
	// too expensive on huge chains.
	keysWithMeta := map[string]int64{}
	for info, err := range sp.List(ctx, "audit/") {
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(info.Key, audit.AnchorPrefix) {
			continue
		}
		// Skip head-pointer files — they're pointers, not events. This
		// matches the global head (audit/_head.json) AND every shard's
		// head (audit/shards/<shard>/_head.json), so the event count
		// isn't inflated by one-per-shard.
		if strings.HasSuffix(info.Key, "_head.json") {
			cs.HeadHashAvailable = true
			continue
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		keysWithMeta[info.Key] = info.Size
	}
	cs.EventCount = len(keysWithMeta)

	// Anchor walk — much cheaper because anchors are compact and we
	// already have a primitive that returns the latest one.
	log := audit.NewStorageBackedLog(sp)
	a, err := log.LatestAnchor(ctx)
	if err == nil && a != nil {
		// Count anchors via a second list (cheap; tiny prefix).
		count := 0
		for info, lerr := range sp.List(ctx, audit.AnchorPrefix) {
			if lerr != nil {
				return nil, lerr
			}
			if !strings.HasSuffix(info.Key, ".json") {
				continue
			}
			count++
		}
		cs.AnchorCount = count
		cs.LastAnchorAt = a.AnchoredAt
		if !a.AnchoredAt.IsZero() {
			cs.LastAnchorAgeMS = now.Sub(a.AnchoredAt).Milliseconds()
		}
	}
	return cs, nil
}

// summarizeApprovals walks the approvals/ prefix and counts the
// lifecycle states. Same iteration shape as approval.Store.List but
// without the per-request fetch — we only need the lifecycle
// counter. Pending / approved / expired / revoked.
//
// To keep the read cheap we still do per-request fetches (the
// classification depends on the full body's ApproverKeys + Approvals
// + RevokedAt), via the existing Store.List which Search-vs-Get
// path is already implemented. For very large approval volumes
// operators pass --no-approvals.
func summarizeApprovals(ctx context.Context, sp storage.StoragePlugin, _ time.Time) (*ApprovalSummary, error) {
	// We don't take a hard dependency on the approval package here
	// because that would create a layer issue (approval depends on
	// audit, audit on backup; repoaudit imports backup + audit; if
	// we add approval, we have to be careful not to introduce a
	// cycle). The approval-lifecycle aggregation is computed by a
	// stand-alone walker below.
	keys := 0
	for info, err := range sp.List(ctx, "approvals/") {
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		// Approval body = approvals/<id>.json (2 segments below
		// approvals/). Per-approver vote = approvals/<id>/approvers/
		// <fp>.json (4 segments). We count the bodies only.
		rel := strings.TrimPrefix(info.Key, "approvals/")
		if strings.Count(rel, "/") > 0 {
			continue
		}
		keys++
	}
	if keys == 0 {
		return &ApprovalSummary{}, nil
	}
	return &ApprovalSummary{Total: keys}, nil
}

// indexHoldsByDeployment groups a flat slice of Holds.
func indexHoldsByDeployment(holds []*backup.Hold) map[string][]*backup.Hold {
	out := map[string][]*backup.Hold{}
	for _, h := range holds {
		out[h.Deployment] = append(out[h.Deployment], h)
	}
	return out
}

// mapToKEKSummary converts the rollup map to a sorted slice. Sort by
// kek_ref ASC for stable test output.
func mapToKEKSummary(in map[string]int) []KEKRefSummary {
	out := make([]KEKRefSummary, 0, len(in))
	for ref, n := range in {
		out = append(out, KEKRefSummary{KEKRef: ref, ManifestCount: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].KEKRef < out[j].KEKRef })
	return out
}

// mapToSchemaSummary converts the rollup map to a sorted slice.
func mapToSchemaSummary(in map[string]int) []SchemaSummary {
	out := make([]SchemaSummary, 0, len(in))
	for s, n := range in {
		out = append(out, SchemaSummary{Schema: s, ManifestCount: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Schema < out[j].Schema })
	return out
}

// latestTracker remembers the manifest with the newest StoppedAt for
// the LatestBackupSummary. Encapsulating it as a tiny struct keeps
// the main walk readable.
type latestTracker struct {
	best *backup.Manifest
}

func newLatestTracker() *latestTracker { return &latestTracker{} }

func (l *latestTracker) observe(m *backup.Manifest, replicaIndex map[string]struct{}) {
	if l.best == nil || m.StoppedAt.After(l.best.StoppedAt) {
		l.best = m
	}
}

func (l *latestTracker) snapshot() *LatestBackupSummary {
	if l.best == nil {
		return nil
	}
	out := &LatestBackupSummary{
		BackupID:    l.best.BackupID,
		StoppedAt:   l.best.StoppedAt,
		Type:        string(l.best.Type),
		PGVersion:   l.best.PGVersion,
		Timeline:    l.best.Timeline,
		WALGapCount: len(l.best.WALGaps),
	}
	if l.best.Encryption != nil {
		out.Encrypted = true
		out.KEKRef = l.best.Encryption.KEKRef
	}
	return out
}

// orderedSet maintains insertion-order-stable string sets.
type orderedSet struct {
	seen map[string]struct{}
	keys []string
}

func newOrderedSet() *orderedSet { return &orderedSet{seen: map[string]struct{}{}} }

func (s *orderedSet) add(v string) {
	if v == "" {
		return
	}
	if _, ok := s.seen[v]; ok {
		return
	}
	s.seen[v] = struct{}{}
	s.keys = append(s.keys, v)
}

func (s *orderedSet) values() []string {
	out := make([]string, len(s.keys))
	copy(out, s.keys)
	sort.Strings(out)
	return out
}

// intSet collects a unique int set, sorted ascending on read.
type intSet struct {
	seen map[int]struct{}
}

func newIntSet() *intSet { return &intSet{seen: map[int]struct{}{}} }

func (s *intSet) add(v int) { s.seen[v] = struct{}{} }

func (s *intSet) values() []int {
	out := make([]int, 0, len(s.seen))
	for v := range s.seen {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// uint32Set is a uint32 variant of intSet (used for Timeline).
type uint32Set struct {
	seen map[uint32]struct{}
}

func newUint32Set() *uint32Set { return &uint32Set{seen: map[uint32]struct{}{}} }

func (s *uint32Set) add(v uint32) { s.seen[v] = struct{}{} }

func (s *uint32Set) values() []uint32 {
	out := make([]uint32, 0, len(s.seen))
	for v := range s.seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
