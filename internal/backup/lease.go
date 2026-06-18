// lease.go — per-deployment backup lease.
//
// A backup lease is a small marker the runner writes to the repo
// before it starts BASE_BACKUP and deletes when the backup ends.  It
// prevents two backups of the SAME deployment — possibly on different
// hosts / agents that only share the repo — from running concurrently
// (which would double the load on the source primary and litter the
// repo with redundant manifests).
//
// It is a crash-tolerant lock: the marker carries an ExpiresAt, so a
// holder that dies mid-backup never releases it, but the lease lapses
// and the next backup reclaims it automatically.  Mutual exclusion is
// enforced by the storage layer's atomic conditional put (IfNotExists)
// — the same primitive the manifest commit uses — so no external lock
// service is required.
//
// The lease lives under its own top-level prefix, isolated from the
// manifest / chunk / WAL / audit namespaces so no GC or listing pass
// ever trips on it:
//
//	leases/<deployment>/backup.json
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdio "io"
	"os"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// LeaseSchema is the wire-format identifier on every lease body.
const LeaseSchema = "pg_hardstorage.backup.lease.v1"

// DefaultLeaseTTL is how long a freshly-acquired or renewed lease
// stays valid without a renewal.  A holder that crashes is reclaimable
// once this window elapses.  It is generously above the renewal
// cadence (TTL/3) so a slow-but-live backup never loses its own lease,
// yet a dead holder is reclaimed in minutes rather than hours.
const DefaultLeaseTTL = 15 * time.Minute

// maxLeaseBodyBytes caps the lease read — the body is a few hundred
// bytes; anything larger is corruption or a misplaced object.
const maxLeaseBodyBytes = 64 << 10

var (
	// ErrBackupInProgress is returned by AcquireBackupLease when a
	// LIVE lease for the deployment is already held by someone else.
	ErrBackupInProgress = errors.New("backup: another backup is already in progress for this deployment")

	// ErrLeaseLost is returned by Renew (and surfaced by Maintain)
	// when the lease we held has been reclaimed by another holder —
	// i.e. we let it lapse and someone else took over.  The backup
	// should abort: continuing would risk two live backups.
	ErrLeaseLost = errors.New("backup: lease no longer held (reclaimed by another holder)")
)

func backupLeaseKey(deployment string) string {
	return "leases/" + deployment + "/backup.json"
}

// leaseBody is the persisted lease document.  Owner+AcquiredAt form
// the fencing token: Renew/Release act only while the stored token
// still matches the one we wrote.
type leaseBody struct {
	Schema     string    `json:"schema"`
	Deployment string    `json:"deployment"`
	Owner      string    `json:"owner"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// LeaseOptions tunes AcquireBackupLease.  The zero value uses
// DefaultLeaseTTL, time.Now, and a "<hostname>/pid-<pid>" owner.
type LeaseOptions struct {
	// Owner is the identity recorded in the lease (shown to a blocked
	// acquirer).  Empty defaults to "<hostname>/pid-<pid>".
	Owner string
	// TTL overrides DefaultLeaseTTL.  Zero means the default.
	TTL time.Duration
	// now is the clock, injected by tests.  Zero means time.Now.
	now func() time.Time
}

// Lease is a held per-deployment backup lease.  Renew extends it,
// Maintain keeps it alive in the background, Release frees it.
type Lease struct {
	sp         storage.StoragePlugin
	deployment string
	ttl        time.Duration
	now        func() time.Time

	mu   sync.Mutex
	body leaseBody // the document we last wrote (our fencing token)
}

func defaultLeaseOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/pid-%d", host, os.Getpid())
}

// AcquireBackupLease takes the lease for deployment, or fails.
//
//   - No lease present → atomic create-if-absent wins → returned held.
//   - A LIVE lease present → ErrBackupInProgress.
//   - A STALE (expired) lease present → broken and retaken; if another
//     reclaimer wins the race, ErrBackupInProgress.
//
// A lease that exists but cannot be parsed is treated as live (we
// refuse) rather than stale — silently breaking a lease we can't read
// could clobber a running backup.
func AcquireBackupLease(ctx context.Context, sp storage.StoragePlugin, deployment string, opts LeaseOptions) (*Lease, error) {
	if sp == nil {
		return nil, errors.New("backup: lease requires a storage plugin")
	}
	if deployment == "" {
		return nil, errors.New("backup: lease requires a deployment")
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	now := opts.now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	owner := opts.Owner
	if owner == "" {
		owner = defaultLeaseOwner()
	}

	l := &Lease{sp: sp, deployment: deployment, ttl: ttl, now: now}

	// Fast path: atomic create-if-absent.
	body := l.freshBody(owner)
	switch err := l.put(ctx, body, true); {
	case err == nil:
		l.setBody(body)
		return l, nil
	case !errors.Is(err, storage.ErrAlreadyExists):
		return nil, fmt.Errorf("backup: acquire lease for %q: %w", deployment, err)
	}

	// A lease exists — read it to decide live vs stale.
	existing, rerr := l.read(ctx)
	if rerr != nil {
		if errors.Is(rerr, storage.ErrNotFound) {
			// Released between our put and read; one more create try.
			body = l.freshBody(owner)
			if err := l.put(ctx, body, true); err != nil {
				if errors.Is(err, storage.ErrAlreadyExists) {
					return nil, ErrBackupInProgress
				}
				return nil, fmt.Errorf("backup: acquire lease for %q: %w", deployment, err)
			}
			l.setBody(body)
			return l, nil
		}
		return nil, fmt.Errorf("backup: a lease for %q exists but could not be read: %w", deployment, rerr)
	}
	if now().Before(existing.ExpiresAt) {
		return nil, fmt.Errorf("%w (held by %q, acquired %s, expires %s)",
			ErrBackupInProgress, existing.Owner,
			existing.AcquiredAt.Format(time.RFC3339), existing.ExpiresAt.Format(time.RFC3339))
	}

	// Stale: break and retake.  Delete then create-if-absent so exactly
	// one of several concurrent reclaimers wins.
	if derr := sp.Delete(ctx, backupLeaseKey(deployment)); derr != nil && !errors.Is(derr, storage.ErrNotFound) {
		return nil, fmt.Errorf("backup: break stale lease for %q: %w", deployment, derr)
	}
	body = l.freshBody(owner)
	if err := l.put(ctx, body, true); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			return nil, ErrBackupInProgress
		}
		return nil, fmt.Errorf("backup: retake stale lease for %q: %w", deployment, err)
	}
	l.setBody(body)
	return l, nil
}

// Renew extends the lease's expiry by its TTL.  It first confirms we
// still hold it (the stored Owner+AcquiredAt fencing token still
// matches ours); if another holder reclaimed a lease we let lapse,
// Renew returns ErrLeaseLost and the caller should abort.
func (l *Lease) Renew(ctx context.Context) error {
	l.mu.Lock()
	mine := l.body
	l.mu.Unlock()

	cur, err := l.read(ctx)
	if err != nil {
		return fmt.Errorf("backup: renew lease for %q: %w", l.deployment, err)
	}
	if cur.Owner != mine.Owner || !cur.AcquiredAt.Equal(mine.AcquiredAt) {
		return ErrLeaseLost
	}
	// Our fencing token still matches, but if the stored lease has already
	// EXPIRED we stalled past the TTL — a reclaimer (AcquireBackupLease's
	// stale-break path) may be taking over this very moment. The renew
	// below is an UNCONDITIONAL overwrite, so reviving an expired lease
	// here would clobber that concurrent reclaim and leave both holders
	// believing they own it (race-condition audit #4). A reclaimer can
	// only act once the lease is expired, so treating an expired
	// self-lease as lost closes the window: a healthy holder renews on the
	// TTL/3 cadence and never reaches expiry, while a stalled one aborts
	// rather than clobbering its successor.
	if !l.now().UTC().Before(cur.ExpiresAt) {
		return ErrLeaseLost
	}
	next := mine
	next.ExpiresAt = l.now().UTC().Add(l.ttl)
	if err := l.put(ctx, next, false); err != nil {
		return fmt.Errorf("backup: renew lease for %q: %w", l.deployment, err)
	}
	l.setBody(next)
	return nil
}

// Release frees the lease if we still hold it.  Best-effort and
// idempotent: an already-deleted lease, or one another holder has
// reclaimed, is left untouched.  Pass a fresh/background context so
// release still runs when the backup's own context was cancelled.
func (l *Lease) Release(ctx context.Context) error {
	l.mu.Lock()
	mine := l.body
	l.mu.Unlock()

	cur, err := l.read(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("backup: release lease for %q: %w", l.deployment, err)
	}
	if cur.Owner != mine.Owner || !cur.AcquiredAt.Equal(mine.AcquiredAt) {
		return nil // superseded — not ours to delete
	}
	if err := l.sp.Delete(ctx, backupLeaseKey(l.deployment)); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("backup: release lease for %q: %w", l.deployment, err)
	}
	return nil
}

// Maintain renews the lease on a TTL/3 cadence until ctx is cancelled.
// Run it in a goroutine for the backup's duration.
//
// A transient renewal error is reported via onError but does NOT stop
// the loop — a brief repo blip shouldn't abort a long backup, and the
// backup will fail on its own terms if the repo is truly unreachable.
// ErrLeaseLost DOES stop the loop (and is reported), because at that
// point another backup believes it owns the deployment.
func (l *Lease) Maintain(ctx context.Context, onError func(error)) {
	interval := l.ttl / 3
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Renew(ctx); err != nil {
				if onError != nil {
					onError(err)
				}
				if errors.Is(err, ErrLeaseLost) {
					return
				}
			}
		}
	}
}

func (l *Lease) freshBody(owner string) leaseBody {
	t := l.now().UTC()
	return leaseBody{
		Schema:     LeaseSchema,
		Deployment: l.deployment,
		Owner:      owner,
		AcquiredAt: t,
		ExpiresAt:  t.Add(l.ttl),
	}
}

func (l *Lease) setBody(b leaseBody) {
	l.mu.Lock()
	l.body = b
	l.mu.Unlock()
}

func (l *Lease) put(ctx context.Context, b leaseBody, ifNotExists bool) error {
	enc, err := json.Marshal(&b)
	if err != nil {
		return err
	}
	_, err = l.sp.Put(ctx, backupLeaseKey(l.deployment), bytes.NewReader(enc), storage.PutOptions{
		ContentLength: int64(len(enc)),
		IfNotExists:   ifNotExists,
	})
	return err
}

func (l *Lease) read(ctx context.Context) (leaseBody, error) {
	rc, err := l.sp.Get(ctx, backupLeaseKey(l.deployment))
	if err != nil {
		return leaseBody{}, err
	}
	defer rc.Close()
	raw, err := stdio.ReadAll(stdio.LimitReader(rc, maxLeaseBodyBytes))
	if err != nil {
		return leaseBody{}, err
	}
	var b leaseBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return leaseBody{}, fmt.Errorf("decode lease: %w", err)
	}
	return b, nil
}
