// lease_backends_integration_test.go — the backup lease must actually
// exclude on every backend a repository can live on.
//
// The rest of the lease suite runs against fs://, where IfNotExists is
// O_EXCL and atomicity is free. Production repositories are s3, gcs,
// azblob, sftp and scp, and each implements conditional create by a
// different mechanism: a native precondition on the object stores,
// hardlink@openssh.com on sftp, `ln -T` on scp. "Atomic on fs" says
// nothing about any of them.
//
// That gap is not hypothetical. sftp advertises ConditionalPut only
// when the server offers hardlink@openssh.com, and falls back to
// stat-then-write otherwise — a fallback whose race is harmless for
// content-addressed chunks and fatal for a lock. Nothing caught it
// because nothing ran the lease anywhere but fs.
//
// These tests assert the property the lease exists for — at most one
// holder — directly against each backend, on both paths that can
// produce two holders: a fresh acquire, and the break of a lapsed
// lease.
//
//go:build integration

package backup

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"

	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/azblob"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/gcs"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/sftp"
)

// leaseBackends are the container-backed schemes a repository can use.
var leaseBackends = []struct{ scheme, sinkKind string }{
	{"s3", "s3-minio"},
	{"gcs", "gcs-fake"},
	{"azblob", "azurite"},
	{"sftp", "sftp"},
	{"scp", "ssh-exec"},
}

// openBackend brings up one backend and returns a plugin on it.
func openBackend(t *testing.T, sinkKind, scheme string) storage.StoragePlugin {
	t.Helper()
	rt, err := sink.New(sinkKind)
	if err != nil {
		t.Fatalf("sink.New(%q): %v", sinkKind, err)
	}
	if err := rt.Up(context.Background()); err != nil {
		t.Fatalf("%s sink up: %v", sinkKind, err)
	}
	t.Cleanup(func() { _ = rt.Down(context.Background()) })

	for k, v := range rt.EnvForAgent() {
		t.Setenv(k, v)
	}
	sp, err := storage.Open(context.Background(), rt.URL())
	if err != nil {
		t.Fatalf("storage.Open(%s): %v", scheme, err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// TestLease_ExclusionHoldsOnEveryBackend races N acquirers for one
// deployment's lease and requires exactly one winner.
//
// This is the property the lease exists for, asserted where it has to
// hold rather than where it is convenient to check.
func TestLease_ExclusionHoldsOnEveryBackend(t *testing.T) {
	const racers = 6

	for _, b := range leaseBackends {
		t.Run(b.scheme, func(t *testing.T) {
			sp := openBackend(t, b.sinkKind, b.scheme)

			// A backend that cannot enforce the lease must SAY so
			// rather than hand out a lock that locks nothing.
			if !sp.Capabilities().ConditionalPut {
				_, err := AcquireBackupLease(context.Background(), sp, "db1",
					LeaseOptions{Owner: "solo", TTL: time.Minute})
				if !errors.Is(err, ErrLeaseNotEnforceable) {
					t.Fatalf("%s does not advertise ConditionalPut, so the lease cannot "+
						"exclude — acquire must refuse with ErrLeaseNotEnforceable, got %v",
						b.scheme, err)
				}
				t.Skipf("%s cannot enforce a lease on this fixture; refusal verified", b.scheme)
			}

			var wg sync.WaitGroup
			errs := make([]error, racers)
			for i := 0; i < racers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_, errs[i] = AcquireBackupLease(context.Background(), sp, "db1",
						LeaseOptions{Owner: string(rune('A' + i)), TTL: time.Minute})
				}(i)
			}
			wg.Wait()

			var held int
			for i, err := range errs {
				switch {
				case err == nil:
					held++
				case errors.Is(err, ErrBackupInProgress):
					// The only acceptable loss.
				default:
					t.Errorf("racer %d: unexpected error %v", i, err)
				}
			}
			if held != 1 {
				t.Fatalf("%d of %d racers hold the lease on %s, want exactly 1 — the "+
					"backend's IfNotExists is not atomic, so two backups of one deployment "+
					"can run at once", held, racers, b.scheme)
			}
		})
	}
}

// TestLease_StaleBreakIsExclusiveOnEveryBackend covers the other path
// that can produce two holders, and the one the break claim was added
// for: several runners finding the SAME lapsed lease and all deciding
// to take it over.
//
// The claim is itself an IfNotExists create, so it is exactly as atomic
// as the backend underneath it — which is why this has to run here and
// not only against fs.
func TestLease_StaleBreakIsExclusiveOnEveryBackend(t *testing.T) {
	const racers = 6

	for _, b := range leaseBackends {
		t.Run(b.scheme, func(t *testing.T) {
			sp := openBackend(t, b.sinkKind, b.scheme)
			if !sp.Capabilities().ConditionalPut {
				t.Skipf("%s cannot enforce a lease; covered by the exclusion test", b.scheme)
			}
			ctx := context.Background()

			// A crashed holder: acquire with a TTL that has already
			// elapsed by the time the reclaimers run.
			if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{
				Owner: "crashed", TTL: time.Millisecond,
			}); err != nil {
				t.Fatalf("seed stale lease: %v", err)
			}
			time.Sleep(50 * time.Millisecond) // let it lapse for real

			var wg sync.WaitGroup
			errs := make([]error, racers)
			for i := 0; i < racers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_, errs[i] = AcquireBackupLease(ctx, sp, "db1",
						LeaseOptions{Owner: string(rune('A' + i)), TTL: time.Minute})
				}(i)
			}
			wg.Wait()

			var held int
			for i, err := range errs {
				switch {
				case err == nil:
					held++
				case errors.Is(err, ErrBackupInProgress):
				default:
					t.Errorf("reclaimer %d: unexpected error %v", i, err)
				}
			}
			if held != 1 {
				t.Fatalf("%d of %d reclaimers broke the same stale lease on %s, want exactly "+
					"1 — the break claim is an atomic create, so this means the backend's "+
					"IfNotExists admits more than one winner", held, racers, b.scheme)
			}
		})
	}
}

// TestLease_LateBreakIsRefusedOnEveryBackend is the one that actually
// exercises the break CLAIM per backend.
//
// The two tests above race reclaimers naturally, and a natural race
// resolves under the settle-verify alone — removing the claim entirely
// leaves them green. They prove each backend's IfNotExists is atomic;
// they do not prove the claim is there.
//
// This stages the interleaving the claim exists for: one reclaimer is
// held at its stale recheck until the other has completely finished,
// then released. Without the claim both write and both hold. It needs
// the internal hook, which is why this file is in package backup.
func TestLease_LateBreakIsRefusedOnEveryBackend(t *testing.T) {
	for _, b := range leaseBackends {
		t.Run(b.scheme, func(t *testing.T) {
			sp := openBackend(t, b.sinkKind, b.scheme)
			if !sp.Capabilities().ConditionalPut {
				t.Skipf("%s cannot enforce a lease; covered by the exclusion test", b.scheme)
			}
			ctx := context.Background()

			if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{
				Owner: "crashed", TTL: time.Millisecond,
			}); err != nil {
				t.Fatalf("seed stale lease: %v", err)
			}
			time.Sleep(50 * time.Millisecond)

			parkedAt := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			leaseHookAfterStaleRecheck = func() {
				first := false
				once.Do(func() { first = true })
				if first {
					close(parkedAt)
					<-release
				}
			}
			defer func() { leaseHookAfterStaleRecheck = nil }()

			acquire := func(owner string) <-chan error {
				ch := make(chan error, 1)
				go func() {
					_, err := AcquireBackupLease(ctx, sp, "db1",
						LeaseOptions{Owner: owner, TTL: time.Minute})
					ch <- err
				}()
				return ch
			}

			const budget = 60 * time.Second
			bCh := acquire("B")
			select {
			case <-parkedAt:
			case <-time.After(budget):
				t.Fatalf("B never reached the stale recheck on %s", b.scheme)
			}

			var aErr error
			select {
			case aErr = <-acquire("A"):
			case <-time.After(budget):
				t.Fatalf("A did not finish on %s", b.scheme)
			}

			close(release) // B proceeds, as late as it gets
			var bErr error
			select {
			case bErr = <-bCh:
			case <-time.After(budget):
				t.Fatalf("B did not finish on %s", b.scheme)
			}

			held := 0
			for _, e := range []error{aErr, bErr} {
				if e == nil {
					held++
				} else if !errors.Is(e, ErrBackupInProgress) {
					t.Errorf("unexpected error on %s: %v", b.scheme, e)
				}
			}
			if held != 1 {
				t.Fatalf("%d holders after a late break on %s, want 1 — the break claim is "+
					"not excluding on this backend, so a stalled reclaimer overwrites a live "+
					"lease and two backups run", held, b.scheme)
			}
		})
	}
}
