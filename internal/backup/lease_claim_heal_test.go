package backup

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// failLeaseOverwriteSP fails the next N non-exclusive Puts of the lease
// object — the reclaimer's overwrite after it has won the break claim.
type failLeaseOverwriteSP struct {
	storage.StoragePlugin
	key   string
	fails atomic.Int32
}

func (s *failLeaseOverwriteSP) Put(ctx context.Context, key string, r io.Reader, o storage.PutOptions) (storage.PutResult, error) {
	if key == s.key && !o.IfNotExists && s.fails.Load() > 0 {
		s.fails.Add(-1)
		return storage.PutResult{}, errors.New("transient: 503 slow down")
	}
	return s.StoragePlugin.Put(ctx, key, r, o)
}

// A transient Put failure right after winning the break claim returned
// an error but left the claim behind. The stale lease stayed on disk, so
// every later acquirer lost the same claim to nobody and reported
// conflict.backup_in_progress (exit 7) — scheduled backups silently
// stopped for good. The failing reclaimer must remove its claim.
func TestLease_OverwriteFailureReleasesBreakClaim(t *testing.T) {
	ctx := context.Background()
	base := newLeaseSP(t)
	clk := newClock()
	const ttl = 15 * time.Minute
	if _, err := AcquireBackupLease(ctx, base, "db1", LeaseOptions{Owner: "H", TTL: ttl, now: clk.now, settle: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	clk.advance(ttl + time.Minute) // H died; lease is stale

	sp := &failLeaseOverwriteSP{StoragePlugin: base, key: backupLeaseKey("db1")}
	sp.fails.Store(1)
	if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "A", TTL: ttl, now: clk.now, settle: time.Millisecond}); err == nil {
		t.Fatal("acquire with a failing overwrite reported success")
	}

	// The very next scheduled backup (no fault now) must get the lease.
	if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "B", TTL: ttl, now: clk.now, settle: time.Millisecond}); err != nil {
		t.Fatalf("backups wedged after one transient Put failure: %v", err)
	}
}

// A reclaimer that won the claim and then died (crash between claim and
// overwrite) wedged the succession until an operator deleted the claim
// by hand. A claim abandoned for longer than wedgeGrace — with its
// victim still the stored lease — must no longer block acquisition.
func TestLease_AbandonedBreakClaimExpires(t *testing.T) {
	ctx := context.Background()
	sp := newLeaseSP(t)
	clk := newClock()
	const ttl = 15 * time.Minute
	l, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "H", TTL: ttl, now: clk.now, settle: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(ttl + time.Minute)
	victim, err := l.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dead := &Lease{sp: sp, deployment: "db1", ttl: ttl, now: clk.now, settle: time.Millisecond}
	if err := dead.claimBreak(ctx, victim, "A-who-died"); err != nil {
		t.Fatal(err)
	}

	// Within the grace the claim still excludes (its winner may be mid-put).
	clk.advance(time.Minute)
	if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "B", TTL: ttl, now: clk.now, settle: time.Millisecond}); !errors.Is(err, ErrBackupInProgress) {
		t.Fatalf("fresh claim did not exclude: %v", err)
	}
	// Past it, the succession heals by itself — exactly once.
	clk.advance(wedgeGrace)
	if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "C", TTL: ttl, now: clk.now, settle: time.Millisecond}); err != nil {
		t.Fatalf("abandoned claim still wedges the deployment: %v", err)
	}
	if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "D", TTL: ttl, now: clk.now, settle: time.Millisecond}); !errors.Is(err, ErrBackupInProgress) {
		t.Fatalf("second acquirer after the heal: %v, want ErrBackupInProgress (C holds it)", err)
	}
}
