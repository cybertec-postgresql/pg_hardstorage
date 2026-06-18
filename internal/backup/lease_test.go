package backup

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

func newLeaseSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatalf("open fs: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// fakeClock is a mutable test clock injected via LeaseOptions.now.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *fakeClock                   { return &fakeClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)} }

// TestLease_SecondAcquireBlockedWhileLive: while one holder has a live
// lease, a second acquirer is refused with ErrBackupInProgress — and a
// different deployment is unaffected.
func TestLease_SecondAcquireBlockedWhileLive(t *testing.T) {
	sp := newLeaseSP(t)
	ctx := context.Background()
	clk := newClock()

	if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{
		Owner: "agent-A", TTL: 15 * time.Minute, now: clk.now,
	}); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	_, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{
		Owner: "agent-B", TTL: 15 * time.Minute, now: clk.now,
	})
	if !errors.Is(err, ErrBackupInProgress) {
		t.Fatalf("second acquire while live: got %v, want ErrBackupInProgress", err)
	}

	// A different deployment has its own lease namespace.
	if _, err := AcquireBackupLease(ctx, sp, "db2", LeaseOptions{
		Owner: "agent-B", now: clk.now,
	}); err != nil {
		t.Fatalf("acquire of a different deployment must succeed: %v", err)
	}
}

// TestLease_ReleaseFreesIt: after the holder releases, a new acquire
// succeeds.
func TestLease_ReleaseFreesIt(t *testing.T) {
	sp := newLeaseSP(t)
	ctx := context.Background()
	clk := newClock()

	l, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-A", now: clk.now})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Key must be gone.
	if _, err := sp.Stat(ctx, backupLeaseKey("db1")); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("lease key still present after release: %v", err)
	}
	// And a fresh acquire works.
	if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-B", now: clk.now}); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

// TestLease_StaleLeaseReclaimed: once a lease lapses past its expiry,
// another acquirer reclaims it; the original holder's Renew then
// reports ErrLeaseLost and its Release does not clobber the new holder.
func TestLease_StaleLeaseReclaimed(t *testing.T) {
	sp := newLeaseSP(t)
	ctx := context.Background()
	clk := newClock()

	a, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-A", TTL: 15 * time.Minute, now: clk.now})
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	// Time passes beyond A's TTL without a renewal: A is presumed dead.
	clk.advance(16 * time.Minute)

	b, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-B", TTL: 15 * time.Minute, now: clk.now})
	if err != nil {
		t.Fatalf("B should reclaim the stale lease: %v", err)
	}

	// A wakes up: its renewal must detect it lost the lease.
	if err := a.Renew(ctx); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("A.Renew after reclaim: got %v, want ErrLeaseLost", err)
	}
	// A's release must not delete B's lease.
	if err := a.Release(ctx); err != nil {
		t.Fatalf("A.Release should no-op cleanly: %v", err)
	}
	got, err := b.read(ctx)
	if err != nil {
		t.Fatalf("B's lease should survive A.Release: %v", err)
	}
	if got.Owner != "agent-B" {
		t.Errorf("lease owner = %q, want agent-B (A clobbered B's lease)", got.Owner)
	}
}

// TestLease_RenewExtendsWindow: a renewal pushes the expiry forward, so
// an acquirer arriving after the ORIGINAL expiry — but within the
// renewed window — is still blocked.
func TestLease_RenewExtendsWindow(t *testing.T) {
	sp := newLeaseSP(t)
	ctx := context.Background()
	clk := newClock()

	a, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-A", TTL: 15 * time.Minute, now: clk.now})
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	clk.advance(10 * time.Minute) // within original 15m window
	if err := a.Renew(ctx); err != nil {
		t.Fatalf("A.Renew: %v", err) // expiry now t0+25m
	}

	clk.advance(6 * time.Minute) // t0+16m: past the ORIGINAL expiry, within the renewed one
	_, err = AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-B", TTL: 15 * time.Minute, now: clk.now})
	if !errors.Is(err, ErrBackupInProgress) {
		t.Fatalf("renew should keep the lease live: got %v, want ErrBackupInProgress", err)
	}
}

// TestLease_CorruptLeaseRefused: an existing-but-unparseable lease is
// treated as live (we refuse) rather than silently broken — breaking a
// lease we can't read could clobber a running backup.
func TestLease_CorruptLeaseRefused(t *testing.T) {
	sp := newLeaseSP(t)
	ctx := context.Background()

	body := []byte("{ this is not a valid lease")
	if _, err := sp.Put(ctx, backupLeaseKey("db1"), bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("seed corrupt lease: %v", err)
	}

	_, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-A"})
	if err == nil {
		t.Fatal("acquire over a corrupt lease must fail, not silently break it")
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("unexpected error: %v", err)
	}
}
