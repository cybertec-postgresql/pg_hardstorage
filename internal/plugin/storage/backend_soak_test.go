// backend_soak_test.go — sustained, diversified soak of every
// container-backed storage plugin (s3, gcs, azblob, sftp, scp).
//
// The contract suites answer "does each operation obey its clause once,
// on a quiet server, with one small object?". They run in seconds. That
// leaves whole classes of failure untested — the ones that need TIME,
// CONCURRENCY, and INPUT DIVERSITY against a real server:
//
//   - Resource exhaustion. sshd enforces MaxSessions/MaxStartups; S3
//     clients pool connections. A plugin leaking even a fraction of
//     them degrades only after thousands of operations.
//   - Staging-file leaks. The SSH plugins commit through
//     "<dst>.hstmp-<rand>" temporaries; one leak per thousand
//     operations is invisible in a 14-case suite.
//   - Size-dependent paths. Zero-byte objects, and objects large enough
//     to cross a multipart threshold, take different code paths than
//     the ~10-byte bodies the contract suite uses.
//   - Key-encoding divergence. Spaces, '+', '%', '#', '?' and non-ASCII
//     survive an S3 URL round-trip differently than a shell-quoted scp
//     path or an SFTP path.
//   - Single-winner drift under sustained pressure — the guarantee the
//     shared-DEK mint, the backup lease and the audit chain rest on.
//
// The first version of this soak found a real bug within a second of
// its first run: RenameIfNotExists created the destination's parent on
// fs:// but not on scp:// or sftp://. Diversity is what finds these, so
// the workload varies size, key shape and operation mix rather than
// repeating one uniform round.
//
// Duration comes from PGHS_STORAGE_SOAK_MINUTES (default 2, so a plain
// `go test -tags integration` stays fast):
//
//	PGHS_STORAGE_SOAK_MINUTES=180 go test -tags integration \
//	    -timeout 4h -run TestBackendSoak ./internal/plugin/storage/
//
//go:build integration

package storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

const soakWorkers = 8

// bodySizes spans the size-dependent code paths: empty, tiny, typical
// chunk, and large enough to cross a multipart threshold.
var bodySizes = []int{0, 1, 1 << 10, 64 << 10, 1 << 20, 5 << 20}

// keyShapes stress per-backend key handling: URL-escaping on S3, shell
// quoting on scp, path handling on sftp. Every one is a legal key.
var keyShapes = []string{
	"plain",
	"with space",
	"plus+sign",
	"percent%25",
	"hash#frag",
	"question?mark",
	"unicode-Ünïcødé-日本語",
	"dots.in.name",
	"dash-and_underscore",
	strings.Repeat("long", 40),
}

type soakStats struct {
	puts        atomic.Int64
	gets        atomic.Int64
	stats       atomic.Int64
	deletes     atomic.Int64
	lists       atomic.Int64
	renames     atomic.Int64
	ifNotExists atomic.Int64
	overwrites  atomic.Int64
	badChecksum atomic.Int64
	bytes       atomic.Int64
	// contended counts IfNotExists races actually observed. If it stays
	// 0 the workers never overlapped and the single-winner assertions
	// proved nothing.
	contended atomic.Int64
	errs      atomic.Int64
}

func (s *soakStats) total() int64 {
	return s.puts.Load() + s.gets.Load() + s.stats.Load() + s.deletes.Load() +
		s.lists.Load() + s.renames.Load() + s.ifNotExists.Load() +
		s.overwrites.Load() + s.badChecksum.Load()
}

func soakDuration(t *testing.T) time.Duration {
	t.Helper()
	mins := 2
	if v := os.Getenv("PGHS_STORAGE_SOAK_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mins = n
		}
	}
	return time.Duration(mins) * time.Minute
}

// soakSchemes are the container-backed schemes the soak drives. It is
// derived from wiredSchemes (wiring_e2e_test.go) rather than written
// out again, so a backend added there is soaked too — the earlier
// version listed three by hand and silently left azblob and gcs
// unexercised even though both already had fixtures.
//
// file:// is excluded deliberately: it needs no container, has no
// network or session limits, and the local filesystem's behaviour under
// sustained concurrency is not what this soak exists to find.
func soakSchemes() []wiredScheme {
	var out []wiredScheme
	for _, w := range wiredSchemes {
		if w.sinkKind == "" {
			continue
		}
		out = append(out, w)
	}
	return out
}

// TestBackendSoak drives every container-backed scheme through the
// diversified workload.
//
// Sub-tests rather than separate top-level functions so the set cannot
// drift from wiredSchemes: adding a backend to the wiring table adds it
// here automatically.
func TestBackendSoak(t *testing.T) {
	for _, w := range soakSchemes() {
		t.Run(w.scheme, func(t *testing.T) {
			url := wiringURL(t, w)
			runBackendSoak(t, w.scheme, url)
		})
	}
}

// TestBackendSoak_CoversEveryContainerScheme fails when a scheme has a
// wiring fixture but no soak coverage — the gap that left azblob and
// gcs out of the first version.
func TestBackendSoak_CoversEveryContainerScheme(t *testing.T) {
	var missing []string
	covered := map[string]bool{}
	for _, w := range soakSchemes() {
		covered[w.scheme] = true
	}
	for _, w := range wiredSchemes {
		if w.sinkKind != "" && !covered[w.scheme] {
			missing = append(missing, w.scheme)
		}
	}
	if len(missing) > 0 {
		t.Errorf("scheme(s) %v have a container fixture but are not soaked", missing)
	}
	if len(covered) < 5 {
		t.Errorf("only %d container scheme(s) soaked; expected at least 5 "+
			"(s3, gcs, azblob, sftp, scp)", len(covered))
	}
}

func runBackendSoak(t *testing.T, backend, url string) {
	t.Helper()
	dur := soakDuration(t)
	seed := time.Now().UnixNano()
	if v := os.Getenv("PGHS_STORAGE_SOAK_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			seed = n
		}
	}
	t.Logf("%s soak: budget=%s workers=%d seed=%d url=%s",
		backend, dur, soakWorkers, seed, url)

	ctx := context.Background()
	// Open through storage.Open — the production entry point — so the
	// soak covers the wiring, not just the plugin.
	sp, err := storage.Open(ctx, url)
	if err != nil {
		t.Fatalf("storage.Open(%s): %v", backend, err)
	}
	defer sp.Close()

	caps := sp.Capabilities()
	var st soakStats
	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	var firstErr atomic.Value

	fail := func(op string, err error) {
		st.errs.Add(1)
		if firstErr.Load() == nil {
			firstErr.Store(fmt.Errorf("%s: %w", op, err))
		}
	}

	done := make(chan struct{})
	go func() {
		tk := time.NewTicker(5 * time.Minute)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-tk.C:
				t.Logf("%s soak: %d ops, %.1f GiB written, %s remaining, %d errs",
					backend, st.total(),
					float64(st.bytes.Load())/(1<<30),
					time.Until(deadline).Round(time.Second), st.errs.Load())
			}
		}
	}()

	for w := 0; w < soakWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed + int64(w)))
			round := 0
			for time.Now().Before(deadline) {
				round++
				if !soakRound(ctx, sp, caps, &st, fail, rng, w, round) {
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(done)

	elapsed := dur - time.Until(deadline)
	t.Logf("%s soak: %d ops in %s (%.0f ops/s, %.1f GiB) — put=%d get=%d stat=%d del=%d list=%d rename=%d ifne=%d overwrite=%d badsum=%d contended=%d errs=%d",
		backend, st.total(), elapsed.Round(time.Second),
		float64(st.total())/elapsed.Seconds(), float64(st.bytes.Load())/(1<<30),
		st.puts.Load(), st.gets.Load(), st.stats.Load(), st.deletes.Load(),
		st.lists.Load(), st.renames.Load(), st.ifNotExists.Load(),
		st.overwrites.Load(), st.badChecksum.Load(), st.contended.Load(), st.errs.Load())

	if e := firstErr.Load(); e != nil {
		t.Fatalf("%s soak failed after %d ops (seed=%d): %v",
			backend, st.total(), seed, e.(error))
	}
	if st.total() == 0 {
		t.Fatalf("%s soak performed no operations", backend)
	}
	if caps.ConditionalPut && st.contended.Load() == 0 {
		t.Errorf("%s soak: zero IfNotExists contention across %d ops — workers never "+
			"overlapped, so the single-winner invariant was not exercised", backend, st.total())
	}

	// Staging-file leak check: the SSH plugins commit via
	// "<dst>.hstmp-<rand>". None may survive a clean run.
	leaked, sample := 0, []string{}
	for oi, err := range sp.List(ctx, "soak/") {
		if err != nil {
			t.Fatalf("%s: final list: %v", backend, err)
		}
		if strings.Contains(oi.Key, ".hstmp-") {
			leaked++
			if len(sample) < 5 {
				sample = append(sample, oi.Key)
			}
		}
	}
	if leaked > 0 {
		t.Errorf("%s soak: %d staging file(s) leaked after %d ops, e.g. %v — "+
			"the commit path abandoned temporaries repo gc will not reclaim",
			backend, leaked, st.total(), sample)
	}
}

// soakRound performs one diversified round. Returns false to stop the
// worker (a failure was recorded).
func soakRound(ctx context.Context, sp storage.StoragePlugin,
	caps storage.Capabilities, st *soakStats,
	fail func(string, error), rng *rand.Rand, w, round int) bool {

	shape := keyShapes[rng.Intn(len(keyShapes))]
	size := bodySizes[rng.Intn(len(bodySizes))]
	key := fmt.Sprintf("soak/w%02d/%s/k%06d", w, shape, round)

	body := make([]byte, size)
	for i := range body {
		body[i] = byte('a' + (i+round)%26)
	}
	sum := sha256.Sum256(body)

	// 1. Put with checksum verification.
	if _, err := sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
		ContentSHA256: sum,
	}); err != nil {
		fail(fmt.Sprintf("put(size=%d,shape=%q)", size, shape), err)
		return false
	}
	st.puts.Add(1)
	st.bytes.Add(int64(size))

	// 2. Read back, byte-for-byte. Zero-byte objects included: a
	//    backend that turns an empty body into a missing object, or
	//    into a 1-byte object, fails here.
	rc, err := sp.Get(ctx, key)
	if err != nil {
		fail(fmt.Sprintf("get(size=%d,shape=%q)", size, shape), err)
		return false
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		fail("read", rerr)
		return false
	}
	if !bytes.Equal(got, body) {
		fail("compare", fmt.Errorf("key %q size %d: read %d bytes, content differs",
			key, size, len(got)))
		return false
	}
	st.gets.Add(1)

	// 3. Stat must agree with what we wrote.
	oi, err := sp.Stat(ctx, key)
	if err != nil {
		fail("stat", err)
		return false
	}
	if oi.Size != int64(size) {
		fail("stat-size", fmt.Errorf("key %q: Stat.Size = %d, wrote %d", key, oi.Size, size))
		return false
	}
	st.stats.Add(1)

	// 4. Overwrite with different content — last-writer-wins, no torn
	//    read. Skipped for empty bodies (nothing to distinguish).
	if size > 0 {
		body2 := bytes.Repeat([]byte{'Z'}, size)
		if _, err := sp.Put(ctx, key, bytes.NewReader(body2), storage.PutOptions{
			ContentLength: int64(len(body2)),
		}); err != nil {
			fail("overwrite", err)
			return false
		}
		rc2, err := sp.Get(ctx, key)
		if err != nil {
			fail("get-after-overwrite", err)
			return false
		}
		got2, _ := io.ReadAll(rc2)
		_ = rc2.Close()
		if !bytes.Equal(got2, body2) {
			fail("overwrite-compare",
				fmt.Errorf("key %q: overwrite not observed (torn or stale read)", key))
			return false
		}
		st.overwrites.Add(1)
		st.bytes.Add(int64(size))
	}

	// 5. Corrupt-checksum detection: declare a SHA that cannot match.
	//    Only backends advertising VerifiesContentSHA256 promise to
	//    reject — the others (S3, Azure, GCS, SFTP, SCP) deliberately
	//    ignore the field and lean on transport-layer integrity, and
	//    the CAS layer skips computing it for them. Asserting the
	//    rejection unconditionally would be testing a contract clause
	//    that five of six backends never signed up for.
	if caps.VerifiesContentSHA256 && round%97 == 0 && size > 0 {
		var wrong [32]byte
		wrong[0] = ^sum[0]
		badKey := key + ".badsum"
		_, err := sp.Put(ctx, badKey, bytes.NewReader(body), storage.PutOptions{
			ContentLength: int64(len(body)),
			ContentSHA256: wrong,
		})
		if err == nil {
			fail("checksum", fmt.Errorf(
				"key %q: Put accepted a body whose ContentSHA256 did not match", badKey))
			return false
		}
		if !errors.Is(err, storage.ErrChecksumMismatch) {
			fail("checksum-kind", fmt.Errorf("key %q: got %v, want ErrChecksumMismatch", badKey, err))
			return false
		}
		st.badChecksum.Add(1)
		// The rejected object must not be readable.
		if rcBad, gerr := sp.Get(ctx, badKey); gerr == nil {
			_ = rcBad.Close()
			fail("checksum-persisted",
				fmt.Errorf("key %q: rejected Put left a readable object", badKey))
			return false
		}
	}

	// 6. Cross-prefix atomic rename into a fresh committed space.
	dst := fmt.Sprintf("soak/committed/w%02d/%s/%06d", w, shape, round)
	if err := sp.RenameIfNotExists(ctx, key, dst); err != nil {
		fail("rename", err)
		return false
	}
	st.renames.Add(1)
	if _, err := sp.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		fail("rename-src", fmt.Errorf("key %q: still present after rename (err=%v)", key, err))
		return false
	}

	// 7. Single-winner contention: every worker races for the SAME key.
	if caps.ConditionalPut {
		shared := fmt.Sprintf("soak/shared/round-%06d", round)
		_, err := sp.Put(ctx, shared, strings.NewReader(fmt.Sprintf("w%d", w)),
			storage.PutOptions{IfNotExists: true})
		switch {
		case err == nil:
			st.ifNotExists.Add(1)
		case errors.Is(err, storage.ErrAlreadyExists):
			st.ifNotExists.Add(1)
			st.contended.Add(1)
		default:
			fail("ifnotexists", err)
			return false
		}
	}

	// 8. Periodic list + prune so the tree stays bounded over hours.
	//    List must also see the keys we committed — an encoding bug
	//    that mangles a key on write shows up as a short count here.
	if round%50 == 0 {
		seen := map[string]bool{}
		for oi, err := range sp.List(ctx, fmt.Sprintf("soak/committed/w%02d/", w)) {
			if err != nil {
				fail("list", err)
				return false
			}
			seen[oi.Key] = true
		}
		st.lists.Add(1)
		for r := round - 50; r < round; r++ {
			for _, sh := range keyShapes {
				old := fmt.Sprintf("soak/committed/w%02d/%s/%06d", w, sh, r)
				if !seen[old] {
					continue
				}
				if err := sp.Delete(ctx, old); err != nil {
					fail("delete", err)
					return false
				}
				st.deletes.Add(1)
				// Deleted objects must read as absent immediately.
				if _, err := sp.Stat(ctx, old); !errors.Is(err, storage.ErrNotFound) {
					fail("delete-visibility",
						fmt.Errorf("key %q: Stat after Delete = %v, want ErrNotFound", old, err))
					return false
				}
			}
		}
	}
	return true
}
