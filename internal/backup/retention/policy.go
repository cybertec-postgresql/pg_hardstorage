// Package retention applies a pruning policy to a deployment's set of
// committed manifests, classifying each as kept or to-be-deleted.
//
// The package is intentionally pure: Policy.Apply takes a time and a
// slice of manifests and returns a Decision. No I/O, no storage. The
// caller (the rotate command, or the post-backup auto-rotation hook)
// turns the Decision into soft-deletes against the ManifestStore.
//
// Three policies ship today:
//
//   - GFSPolicy: grandfather-father-son. Keep N daily / N weekly / N
//     monthly / N yearly backups, picking the most-recent backup in
//     each bucket. The default for new repositories — appropriate for
//     the 90% case ("I want a year of monthly backups, a month of
//     weeklies, a week of dailies").
//
//   - SimplePolicy: keep every backup whose StoppedAt is within
//     KeepFor of now; delete the rest. The "I just want N days of
//     backups" knob.
//
//   - CountPolicy: keep the most recent N full backups; delete the
//     rest. The "keep last 14 backups, no time math" knob.
//
// All policies share two safety properties:
//
//   - The most recent backup is ALWAYS kept, regardless of policy
//     output. We will never leave a deployment with zero backups
//     just because a policy said so.
//
//   - Reasons are recorded per kept manifest so the rotate command
//     can show the operator WHY a backup is being kept ("daily-3,
//     monthly-1, newest"). Transparency at decision time is the
//     mechanism that lets operators trust automation.
package retention

import (
	"sort"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// Policy classifies a set of manifests into kept and deletable.
type Policy interface {
	// Name is the canonical lowercase policy name ("gfs", "simple",
	// "count"). Goes into Decision.PolicyName for audit.
	Name() string

	// Apply runs the policy against manifests at instant now. now is
	// passed explicitly (rather than read from time.Now) so tests can
	// pin time and reason about edge cases.
	Apply(now time.Time, manifests []*backup.Manifest) Decision
}

// Decision is the output of Policy.Apply. Keep + Delete partition
// the input; Reasons records why each kept manifest was kept (a
// space-separated list of bucket labels — e.g. "daily-1 weekly-1
// newest" — so the same backup can satisfy multiple buckets).
type Decision struct {
	PolicyName string
	Keep       []*backup.Manifest
	Delete     []*backup.Manifest

	// Reasons maps backup_id → why-kept tokens. A manifest is in
	// Reasons iff it's in Keep; a manifest with no entry is in Delete.
	Reasons map[string][]string
}

// keptCount returns how many manifests are kept. Convenience for tests
// and the CLI summary.
func (d Decision) KeptCount() int { return len(d.Keep) }

// deletedCount returns how many manifests will be deleted.
func (d Decision) DeletedCount() int { return len(d.Delete) }

// addReason records that backup_id is kept because of label. If the
// manifest already has at least one reason, the new one is appended
// (so multi-bucket matches accumulate naturally).
func (d *Decision) addReason(id, label string) {
	if d.Reasons == nil {
		d.Reasons = map[string][]string{}
	}
	d.Reasons[id] = append(d.Reasons[id], label)
}

// sortByStoppedAtDesc returns a copy of in sorted by StoppedAt
// descending (newest first). Tie-broken by BackupID for determinism.
// Used by every policy as a uniform first step.
func sortByStoppedAtDesc(in []*backup.Manifest) []*backup.Manifest {
	out := make([]*backup.Manifest, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].StoppedAt, out[j].StoppedAt
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].BackupID > out[j].BackupID
	})
	return out
}

// finalize partitions sorted (newest-first) by whether each entry has
// at least one reason recorded; manifests with reasons go to Keep,
// the rest to Delete. Always promotes the newest manifest into Keep
// even if no policy bucket claimed it (the safety-net rule). Caller
// has already populated Reasons via addReason.
//
// After the policy decides which manifests to keep, finalize runs a
// chain-aware promotion sweep: any kept manifest's parent_backup_id
// chain is also kept, transitively. Without this, a policy that
// keeps a recent incremental but ages out its full anchor would
// turn the incremental into a dangling chain link — restore would
// fail with chain.broken_tombstoned. Promoted parents carry the
// "chain-anchor" reason so the rotate command's transparency
// surface shows the operator why each kept manifest survives.
func finalize(d *Decision, sorted []*backup.Manifest) {
	if len(sorted) == 0 {
		return
	}
	newest := sorted[0]
	if _, ok := d.Reasons[newest.BackupID]; !ok {
		d.addReason(newest.BackupID, "newest")
	}

	promoteChainParents(d, sorted)

	for _, m := range sorted {
		if _, kept := d.Reasons[m.BackupID]; kept {
			d.Keep = append(d.Keep, m)
		} else {
			d.Delete = append(d.Delete, m)
		}
	}
}

// promoteChainParents extends d.Reasons so that for every manifest
// in d.Reasons (i.e. currently slated to be kept), the entire
// transitive parent_backup_id chain is also kept. The promotion
// reason is "chain-anchor" — distinct from time-bucket reasons
// (daily-N, weekly-N, ...) so an operator inspecting `rotate
// --dry-run` sees clearly that the policy didn't claim this
// manifest, the chain did.
//
// The walk uses a worklist seeded with the currently-kept IDs and
// runs to fixed-point. Real chains are shallow (full + a handful of
// incrementals before the next full), so this is O(N) in practice
// where N is the number of manifests; the worklist size is bounded
// by the manifest count.
//
// If a kept manifest references a parent_backup_id that's not in
// the input slice (e.g. the operator passed a partial slice, or the
// parent has already been hard-deleted) the loop simply stops at
// that link — no error, because retention's job is to decide what
// happens to manifests it CAN see, not to repair the past.
func promoteChainParents(d *Decision, sorted []*backup.Manifest) {
	byID := make(map[string]*backup.Manifest, len(sorted))
	for _, m := range sorted {
		byID[m.BackupID] = m
	}

	// Seed the worklist with currently-kept IDs.
	work := make([]string, 0, len(d.Reasons))
	for id := range d.Reasons {
		work = append(work, id)
	}
	// BFS to fixed-point. We append to `work` while iterating; using
	// an index walk keeps the code obvious. A bounded-depth guard
	// (matching combine.MaxChainDepth's spirit) protects against a
	// hand-crafted cycle in the input — defensive, since combine.Build
	// also catches cycles at restore time. The worklist length
	// natively caps at len(sorted) because each ID is added at most
	// once.
	for i := 0; i < len(work); i++ {
		m, ok := byID[work[i]]
		if !ok {
			continue
		}
		if m.ParentBackupID == "" {
			continue
		}
		if _, kept := d.Reasons[m.ParentBackupID]; kept {
			continue
		}
		// Parent must be in our input — retention only knows what
		// it was given. An absent parent might be hard-deleted, on
		// a different repo, or in a fleet member we didn't list;
		// synthesising a fake reason for it would silently corrupt
		// the Reasons map.
		if _, present := byID[m.ParentBackupID]; !present {
			continue
		}
		// Parent isn't kept yet; promote and queue.
		d.addReason(m.ParentBackupID, "chain-anchor")
		work = append(work, m.ParentBackupID)
	}
}
