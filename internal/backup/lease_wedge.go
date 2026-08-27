// lease_wedge.go — detecting a succession that died half-way.
//
// The break claim makes lease succession atomic: whoever CREATES the
// claim object owns the sole right to overwrite the stale lease. The
// cost of that design is a wedge nobody else can clear: a reclaimer
// that wins the claim and then dies BEFORE its overwrite leaves the
// stale lease on disk and the claim consumed. Every later acquirer
// re-reads the stale lease, tries the same grant-keyed claim, loses to
// a dead process, and reports "backup in progress" — forever.
//
// The window is tiny (claim → put, normally sub-millisecond) but the
// paths through it are frequent: since Release started tombstoning
// (see leaseBody.Released), EVERY acquire-after-release runs a
// succession, not just the rare crashed-holder reclaim. Automatic
// healing would need a second-order claim protocol whose own races
// would dwarf the problem, so the posture is: DETECT loudly, remediate
// by hand. InspectLeaseSuccession is the detector; `doctor` surfaces
// it with the exact object to delete.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// LeaseWedge describes one wedged succession.
type LeaseWedge struct {
	Deployment string
	// LeaseKey is the stale lease everyone keeps re-reading.
	LeaseKey string
	// ClaimKey is the consumed break claim whose winner died. Deleting
	// THIS object (after confirming no backup is actually running for
	// the deployment) un-wedges the succession: the next acquirer
	// re-claims and takes over.
	ClaimKey string
	// Breaker is who won the claim and then disappeared.
	Breaker string
	// ClaimedAt is when they won it.
	ClaimedAt time.Time
	// LeaseOwner / LeaseAcquiredAt identify the stale grant.
	LeaseOwner      string
	LeaseAcquiredAt time.Time
}

// wedgeGrace is how old a consumed claim must be — with its victim
// still the stored lease — before the succession is called wedged.
// The claim→overwrite gap in a live reclaimer is bounded by one Put
// plus the settle window (seconds); an hour of no successor with the
// claim consumed means its winner is gone. Deliberately generous:
// a false "wedged" sends an operator to delete a claim a live
// reclaimer still needs, which is worse than detecting late.
const wedgeGrace = time.Hour

// InspectLeaseSuccession reports whether deployment's lease
// succession is wedged: the stored lease is stale (expired or
// released), its grant's break claim exists, and the claim is old
// enough that its winner cannot still be mid-succession. Returns nil
// when healthy, held live, or simply released with no successor yet.
func InspectLeaseSuccession(ctx context.Context, sp storage.StoragePlugin, deployment string, now time.Time) (*LeaseWedge, error) {
	leaseKey := backupLeaseKey(deployment)
	rc, err := sp.Get(ctx, leaseKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil // never leased (or pre-tombstone release)
		}
		return nil, fmt.Errorf("backup: inspect lease for %q: %w", deployment, err)
	}
	var body leaseBody
	derr := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&body)
	_ = rc.Close()
	if derr != nil {
		return nil, fmt.Errorf("backup: inspect lease for %q: parse: %w", deployment, derr)
	}
	if !body.Released && now.Before(body.ExpiresAt) {
		return nil, nil // held live — nothing stale to succeed
	}

	claimKey := breakClaimKey(deployment, body)
	crc, err := sp.Get(ctx, claimKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil // stale but unclaimed — any acquirer can take it
		}
		return nil, fmt.Errorf("backup: inspect break claim for %q: %w", deployment, err)
	}
	var claim breakClaim
	derr = json.NewDecoder(io.LimitReader(crc, 1<<20)).Decode(&claim)
	_ = crc.Close()
	if derr != nil {
		return nil, fmt.Errorf("backup: inspect break claim for %q: parse: %w", deployment, derr)
	}
	if now.Sub(claim.ClaimedAt) < wedgeGrace {
		return nil, nil // succession in flight; give the winner time
	}
	return &LeaseWedge{
		Deployment:      deployment,
		LeaseKey:        leaseKey,
		ClaimKey:        claimKey,
		Breaker:         claim.Breaker,
		ClaimedAt:       claim.ClaimedAt,
		LeaseOwner:      body.Owner,
		LeaseAcquiredAt: body.AcquiredAt,
	}, nil
}
