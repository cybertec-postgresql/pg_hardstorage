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
//	leases/<deployment>/backup.json          the lease itself
//	leases/<deployment>/breaks/<token>.json  break claims (see below)
//
// Breaking a lapsed lease is mutually exclusive through the same
// atomic create: a reclaimer must first CREATE a claim object named
// after the lease it intends to break, so of all the reclaimers that
// judged one lease stale, exactly one may act on that judgement — no
// matter how their reads and writes interleave.
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/invariant"
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

	// ErrLeaseNotEnforceable is returned by AcquireBackupLease when the
	// repository's backend cannot create an object atomically, so the
	// lease cannot actually exclude a second backup.
	//
	// The lease is built entirely on Put(IfNotExists): the initial
	// acquire, and the claim that makes breaking a lapsed lease
	// exclusive. A backend without ConditionalPut emulates it with
	// stat-then-write, which is a check followed by an unrelated
	// action — two runners can pass the check together and both
	// proceed. The lease would still be WRITTEN, and would still look
	// correct in the repo; it simply would not exclude anything.
	//
	// Refusing mirrors how the CAS treats unenforceable WORM: an
	// operator who believes they have a guarantee they do not have is
	// worse off than one whose backup stops with a reason. Set
	// LeaseOptions.AllowUnenforceable to proceed anyway.
	ErrLeaseNotEnforceable = errors.New("backup: repository backend cannot enforce the backup lease (no atomic conditional put)")

	// ErrLeaseLost is returned by Renew (and surfaced by Maintain)
	// when the lease we held has been reclaimed by another holder —
	// i.e. we let it lapse and someone else took over.  The backup
	// should abort: continuing would risk two live backups.
	ErrLeaseLost = errors.New("backup: lease no longer held (reclaimed by another holder)")
)

func backupLeaseKey(deployment string) string {
	return "leases/" + deployment + "/backup.json"
}

// breakClaimKey names the object a reclaimer must atomically CREATE
// before it may overwrite a stale lease.
//
// The key is derived from the identity of the lease being broken
// (Owner + AcquiredAt — the same fencing token Renew and Release use),
// so every reclaimer that observed the SAME stale lease competes for
// the SAME key. Create-if-absent is atomic, so exactly one of them can
// win it, no matter how their reads and writes interleave or how long
// any of them stalls.
//
// It lives under its own sub-prefix so listing the lease itself is
// unaffected.
func breakClaimKey(deployment string, victim leaseBody) string {
	h := sha256.Sum256([]byte(victim.Owner + "\x00" +
		victim.AcquiredAt.UTC().Format(time.RFC3339Nano)))
	return "leases/" + deployment + "/breaks/" + hex.EncodeToString(h[:8]) + ".json"
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

// defaultLeaseSettle is the post-write verification window used by the
// stale-break and renew paths (see the settle-verify comments below).
// Long enough to cover a competing writer whose write raced ours by a
// scheduling quantum; short enough to be irrelevant next to a backup's
// runtime.
const defaultLeaseSettle = 400 * time.Millisecond

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
	// settle overrides defaultLeaseSettle, injected by tests.
	settle time.Duration
	// AllowUnenforceable proceeds even when the backend cannot create
	// objects atomically, accepting that the lease excludes nothing.
	// Only sensible for a deployment where exactly one backup runner
	// can possibly exist, and the operator knows it.
	AllowUnenforceable bool
}

// Test hooks: gate specific interleavings deterministically in race
// tests.  Always nil in production.
var (
	leaseHookAfterStaleRecheck func() // between the stale recheck and the overwrite
	leaseHookBeforeRenewPut    func() // between Renew's expiry check and its put
)

// Lease is a held per-deployment backup lease.  Renew extends it,
// Maintain keeps it alive in the background, Release frees it.
type Lease struct {
	sp         storage.StoragePlugin
	deployment string
	ttl        time.Duration
	now        func() time.Time
	settle     time.Duration

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
	settle := opts.settle
	if settle <= 0 {
		settle = defaultLeaseSettle
	}

	// The whole design rests on Put(IfNotExists) being atomic. A
	// backend that only emulates it cannot exclude anything, and a
	// lease that does not exclude is worse than none: it reads as a
	// guarantee in the repo and in `repo gc`, so nobody looks again.
	if !opts.AllowUnenforceable && !sp.Capabilities().ConditionalPut {
		return nil, fmt.Errorf("%w: backend %q emulates IfNotExists with stat-then-write, "+
			"so two runners can hold this lease at once and back up %q concurrently. "+
			"For SFTP this means the server does not advertise hardlink@openssh.com; "+
			"use a server that does, or a different repository backend. Set "+
			"LeaseOptions.AllowUnenforceable only where exactly one runner can exist",
			ErrLeaseNotEnforceable, sp.Name(), deployment)
	}

	l := &Lease{sp: sp, deployment: deployment, ttl: ttl, now: now, settle: settle}

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

	// Stale: break and retake.
	//
	// The earlier design here (Delete, then create-if-absent) was racy:
	// two reclaimers could both judge the lease stale, reclaimer A could
	// complete Delete+create — holding a LIVE lease — and reclaimer B's
	// still-pending unconditional Delete then destroyed A's fresh lease,
	// after which B's create succeeded too. Both returned held, and two
	// backups of the same deployment ran concurrently (concurrency audit,
	// demonstrated under -race).
	//
	// The rewrite never deletes. Sequence:
	//
	//  1. RECHECK: re-read immediately before acting. Only proceed if the
	//     stored lease is still the exact stale body we first observed —
	//     a reclaimer that already broke it (fresh token) or a lease that
	//     changed in any way sends us to ErrBackupInProgress instead.
	//  2. OVERWRITE in place (no delete): concurrent reclaimers can only
	//     overwrite each other; nobody can destroy a winner's lease.
	//  2b. BREAK CLAIM: atomically create an object named after the
	//     lease being broken. Every reclaimer that saw this same stale
	//     lease races for this same key, and create-if-absent admits
	//     exactly one of them. This is what makes the break mutually
	//     exclusive; it does not depend on how the reclaimers'
	//     reads and writes interleave, or on any of them being prompt.
	//  3. SETTLE-VERIFY: wait `settle`, re-read, and only return held if
	//     the stored fencing token is OURS. With the claim in place no
	//     other RECLAIMER can be writing, so this now guards only
	//     against a stalled holder's Renew landing on top of us — the
	//     case TestLease_RenewCannotClobberReclaimer covers.
	//
	// This previously had a residual window: the sequence was
	// recheck → overwrite → settle-verify, with nothing atomic in it, so
	// a reclaimer that stalled longer than `settle` between its recheck
	// and its write could clobber a winner that had already verified,
	// and BOTH would report the lease held. Two backups of one
	// deployment would then run. `settle` made that unlikely rather than
	// impossible, and the stale-reclaim race test hit it on a loaded CI
	// runner — reporting a mutual-exclusion violation that was real.
	//
	// The break claim closes it. A stalled reclaimer's write can no
	// longer land at all: it must win the claim first, and the claim was
	// taken the moment the winner passed this point.
	//
	// Claims are never deleted. Removing one would let a reclaimer still
	// holding that stale token re-win it and overwrite a live lease —
	// exactly the window being closed. They are only written when a
	// crashed holder is reclaimed (a released lease leaves none), and
	// each is a few hundred bytes, so the accumulation is proportional
	// to crashes rather than to backups.
	recheck, rcerr := l.read(ctx)
	if rcerr != nil {
		if errors.Is(rcerr, storage.ErrNotFound) {
			// Released/broken since we judged it stale — plain create.
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
		return nil, fmt.Errorf("backup: recheck stale lease for %q: %w", deployment, rcerr)
	}
	if recheck.Owner != existing.Owner || !recheck.AcquiredAt.Equal(existing.AcquiredAt) ||
		now().Before(recheck.ExpiresAt) {
		// Someone else already broke + retook it (or the holder revived).
		return nil, ErrBackupInProgress
	}
	if leaseHookAfterStaleRecheck != nil {
		leaseHookAfterStaleRecheck()
	}
	// Win the right to break THIS lease before touching it.
	if err := l.claimBreak(ctx, recheck, owner); err != nil {
		return nil, err
	}
	body = l.freshBody(owner)
	if err := l.put(ctx, body, false); err != nil {
		return nil, fmt.Errorf("backup: retake stale lease for %q: %w", deployment, err)
	}
	if err := l.settleVerify(ctx, body); err != nil {
		return nil, err
	}
	l.setBody(body)
	return l, nil
}

// settleVerify waits the settle window and confirms the stored lease
// still carries our fencing token. Returns ErrBackupInProgress when a
// competing writer won.
func (l *Lease) settleVerify(ctx context.Context, mine leaseBody) error {
	if l.settle > 0 {
		t := time.NewTimer(l.settle)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	cur, err := l.read(ctx)
	if err != nil {
		return fmt.Errorf("backup: verify lease for %q after write: %w", l.deployment, err)
	}
	if cur.Owner != mine.Owner || !cur.AcquiredAt.Equal(mine.AcquiredAt) {
		return ErrBackupInProgress
	}
	return nil
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
	// Same posture, one step earlier: if expiry is INSIDE the settle
	// margin, a reclaimer may pass its own staleness check before our
	// overwrite lands. Don't write into that window — treat the lease
	// as lost and let the caller abort (a healthy holder renews at
	// TTL/3 and never gets this close to expiry).
	if !l.now().UTC().Add(2 * l.settle).Before(cur.ExpiresAt) {
		return ErrLeaseLost
	}
	if leaseHookBeforeRenewPut != nil {
		leaseHookBeforeRenewPut()
	}
	next := mine
	next.ExpiresAt = l.now().UTC().Add(l.ttl)
	// Fencing monotonicity: a renewal must strictly EXTEND the lease.
	// Writing an expiry at-or-before the stored one would shrink the
	// mutual-exclusion window mid-backup and invite a second writer —
	// impossible unless the TTL/clock plumbing in this file is broken,
	// which is exactly when we must not keep going.
	invariant.Assert(next.ExpiresAt.After(cur.ExpiresAt),
		"lease renewal for %q does not extend expiry (cur %s, next %s, ttl %s)",
		l.deployment, cur.ExpiresAt.Format(time.RFC3339Nano), next.ExpiresAt.Format(time.RFC3339Nano), l.ttl)
	if err := l.put(ctx, next, false); err != nil {
		return fmt.Errorf("backup: renew lease for %q: %w", l.deployment, err)
	}
	// Settle-verify: if a reclaimer's overwrite raced ours, only the
	// last writer's token survives — anyone else must abort. (The
	// reclaimer performs the same verify, so exactly one side keeps
	// the lease for any interleaving within the settle window.)
	if err := l.settleVerify(ctx, next); err != nil {
		if errors.Is(err, ErrBackupInProgress) {
			return ErrLeaseLost
		}
		return err
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

// breakClaim records who broke which lease. Written once, never
// rewritten; the object's EXISTENCE is the lock, the body is for a
// human reading the repo afterwards.
type breakClaim struct {
	Schema      string    `json:"schema"`
	Deployment  string    `json:"deployment"`
	BrokenOwner string    `json:"broken_owner"`
	BrokenAt    time.Time `json:"broken_acquired_at"`
	Breaker     string    `json:"breaker"`
	ClaimedAt   time.Time `json:"claimed_at"`
}

// claimBreak takes the exclusive right to break `victim`.
//
// Returns ErrBackupInProgress when another reclaimer already holds the
// claim — it is breaking, or has broken, this same lease, so we must
// not. Any other error is reported as-is: failing to establish
// exclusivity must never be read as having established it.
func (l *Lease) claimBreak(ctx context.Context, victim leaseBody, owner string) error {
	enc, err := json.Marshal(&breakClaim{
		Schema:      LeaseSchema,
		Deployment:  l.deployment,
		BrokenOwner: victim.Owner,
		BrokenAt:    victim.AcquiredAt.UTC(),
		Breaker:     owner,
		ClaimedAt:   l.now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("backup: encode break claim for %q: %w", l.deployment, err)
	}
	_, err = l.sp.Put(ctx, breakClaimKey(l.deployment, victim), bytes.NewReader(enc),
		storage.PutOptions{ContentLength: int64(len(enc)), IfNotExists: true})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrAlreadyExists):
		// Someone else is already breaking this exact lease.
		return ErrBackupInProgress
	default:
		return fmt.Errorf("backup: claim break of stale lease for %q: %w", l.deployment, err)
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
