package repo_test

// cas_adopt_guard_test.go — dedup must not adopt a chunk this backup's
// DEK cannot read.
//
// Chunk keys are global to a repository (chunks/sha256/<hash>.chk) but
// the shared DEK is per-KEKRef. A backup under one KEKRef that dedups
// against chunks written under another therefore commits a manifest
// referencing chunks it cannot decrypt: the backup exits 0 and fails
// only at restore.
//
// These run against fs:// so the invariant is checked in
// milliseconds, deterministically, with no container — the end-to-end
// proof lives in internal/cli's KEKRef lifecycle suite, but this is
// what pins the CAS behaviour itself.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func adoptTestStore(t *testing.T) storage.StoragePlugin {
	t.Helper()
	p := &fs.Plugin{}
	u, err := url.Parse("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("open fs store: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func adoptEncryptor(t *testing.T, fill byte) encryption.Encryptor {
	t.Helper()
	key := make([]byte, encryption.KeyLen)
	for i := range key {
		key[i] = fill
	}
	e, err := aesgcm.New(key)
	if err != nil {
		t.Fatalf("aesgcm.New: %v", err)
	}
	return e
}

// TestCAS_DedupAcrossDEKsIsRefused is the regression test for the
// defect. Two CASes over ONE store with DIFFERENT DEKs: the second
// must refuse to adopt the first's chunk rather than silently
// deduplicating against bytes it cannot decrypt.
func TestCAS_DedupAcrossDEKsIsRefused(t *testing.T) {
	ctx := context.Background()
	sp := adoptTestStore(t)
	body := bytes.Repeat([]byte("cross-kek-dedup"), 512)

	first := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xA1)))
	if _, err := first.PutChunk(ctx, body); err != nil {
		t.Fatalf("first PutChunk: %v", err)
	}

	// A second backup under a different KEKRef resolves a different
	// shared DEK. Its chunker hashes the same plaintext and finds the
	// key already present.
	second := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xB2)))
	_, err := second.PutChunk(ctx, body)
	if err == nil {
		t.Fatal("PutChunk adopted a chunk encrypted under a different DEK — the " +
			"manifest would reference chunks it cannot decrypt, and the backup would " +
			"fail only at restore")
	}
	msg := err.Error()
	for _, want := range []string{
		"does not decrypt with this backup's data key",
		"kms rotate",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message lacks %q — an operator needs the cause and a "+
				"remedy, not just a failure:\n%s", want, msg)
		}
	}
}

// TestCAS_DedupWithinOneDEKStillWorks is the other half, and the one
// that matters for cost: the guard must not disturb ordinary dedup.
// Every incremental backup depends on adopting its predecessor's
// chunks, so a guard that refused those would be far worse than the
// bug it fixes.
func TestCAS_DedupWithinOneDEKStillWorks(t *testing.T) {
	ctx := context.Background()
	sp := adoptTestStore(t)
	body := bytes.Repeat([]byte("same-dek-dedup"), 512)

	enc := adoptEncryptor(t, 0xC3)
	first := repo.NewCAS(sp, repo.WithEncryptor(enc))
	if _, err := first.PutChunk(ctx, body); err != nil {
		t.Fatalf("first PutChunk: %v", err)
	}

	// A separate CAS instance with the SAME key — a later backup in the
	// same repo under the same KEKRef.
	second := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xC3)))
	info, err := second.PutChunk(ctx, body)
	if err != nil {
		t.Fatalf("dedup within one DEK was refused: %v", err)
	}
	if !info.Deduped {
		t.Error("chunk was re-written rather than deduplicated; the guard has " +
			"defeated dedup, which is the whole point of content addressing")
	}
}

// TestCAS_UnencryptedRepoUnaffected pins that the guard costs nothing
// where it cannot apply: with no DEK in play there is no cross-DEK
// question, and the probe must not run at all.
func TestCAS_UnencryptedRepoUnaffected(t *testing.T) {
	ctx := context.Background()
	sp := adoptTestStore(t)
	body := bytes.Repeat([]byte("plaintext"), 512)

	first := repo.NewCAS(sp)
	if _, err := first.PutChunk(ctx, body); err != nil {
		t.Fatalf("first PutChunk: %v", err)
	}
	second := repo.NewCAS(sp)
	info, err := second.PutChunk(ctx, body)
	if err != nil {
		t.Fatalf("unencrypted dedup refused: %v", err)
	}
	if !info.Deduped {
		t.Error("unencrypted chunk was re-written rather than deduplicated")
	}
}

// TestCAS_AdoptProbeRunsOncePerCAS pins the cost model. The check is a
// property of (repo, DEK), not of any single chunk, so it must resolve
// once — verifying every dedup hit would read and decrypt each
// deduplicated chunk, which is exactly the work dedup exists to avoid.
func TestCAS_AdoptProbeRunsOncePerCAS(t *testing.T) {
	ctx := context.Background()
	backing := adoptTestStore(t)
	counter := &probeStore{StoragePlugin: backing}

	enc := adoptEncryptor(t, 0xD4)
	writer := repo.NewCAS(backing, repo.WithEncryptor(enc))
	var bodies [][]byte
	for i := 0; i < 5; i++ {
		b := bytes.Repeat([]byte{byte('a' + i)}, 4096)
		bodies = append(bodies, b)
		if _, err := writer.PutChunk(ctx, b); err != nil {
			t.Fatalf("seed PutChunk: %v", err)
		}
	}

	// A fresh CAS over the counting store adopts all five.
	reader := repo.NewCAS(counter, repo.WithEncryptor(adoptEncryptor(t, 0xD4)))
	for _, b := range bodies {
		if _, err := reader.PutChunk(ctx, b); err != nil {
			t.Fatalf("adopting PutChunk: %v", err)
		}
	}
	if got := counter.Gets(); got != 1 {
		t.Errorf("the adopt probe issued %d Get(s) across 5 adoptions, want exactly 1 — "+
			"a per-chunk probe would read and decrypt every deduplicated chunk", got)
	}
}

// TestCAS_AdoptProbeTransientErrorAdopts pins a deliberate fail-open.
//
// The guard concludes only on ErrAuthenticationFailed or
// ErrChecksumMismatch. Anything else — a blip, a throttle, a 503 —
// says nothing about key custody, and refusing on it would turn every
// backend hiccup into a failed backup. So adoption proceeds.
//
// That is the right trade, but it is a trade: while the backend is
// erroring, cross-KEK adoption goes through unchecked. It is pinned
// here so the choice stays visible and deliberate rather than being
// discovered later as a hole.
func TestCAS_AdoptProbeTransientErrorAdopts(t *testing.T) {
	ctx := context.Background()
	backing := adoptTestStore(t)
	bodies := seedChunks(t, backing, adoptEncryptor(t, 0xA1), 1)

	sp := &probeStore{StoragePlugin: backing}
	sp.failFirst(1, errTransient)

	// A DIFFERENT DEK — the guard would refuse this if it could see.
	cas := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xB2)))
	info, err := cas.PutChunk(ctx, bodies[0])
	if err != nil {
		t.Fatalf("PutChunk refused on a transient probe failure: %v\n"+
			"A backend blip is not evidence about key custody; refusing on it would "+
			"fail backups whenever the store is briefly unhappy", err)
	}
	if !info.Deduped {
		t.Error("chunk was rewritten rather than adopted")
	}
	if got := sp.Gets(); got != 1 {
		t.Errorf("probe issued %d Get(s), want 1", got)
	}
}

// TestCAS_AdoptProbeRearmsAfterTransientError is the other half, and
// the one that matters: an inconclusive probe must NOT be cached.
//
// If a blip marked the guard resolved, one unlucky moment would disarm
// it for the rest of the backup and every later dedup hit would adopt
// unchecked. So the next adoption re-probes — and here that second
// probe is the one that catches the wrong key.
func TestCAS_AdoptProbeRearmsAfterTransientError(t *testing.T) {
	ctx := context.Background()
	backing := adoptTestStore(t)
	bodies := seedChunks(t, backing, adoptEncryptor(t, 0xA1), 2)

	sp := &probeStore{StoragePlugin: backing}
	sp.failFirst(1, errTransient) // only the FIRST probe is blind

	cas := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xB2)))

	if _, err := cas.PutChunk(ctx, bodies[0]); err != nil {
		t.Fatalf("first PutChunk (blind probe) should adopt: %v", err)
	}
	_, err := cas.PutChunk(ctx, bodies[1])
	if err == nil {
		t.Fatal("second PutChunk adopted a chunk encrypted under a different DEK — the " +
			"guard cached an INCONCLUSIVE result, so one transient error disarmed it for " +
			"the rest of this CAS and every later dedup hit adopts unchecked")
	}
	if !strings.Contains(err.Error(), "does not decrypt with this backup's data key") {
		t.Errorf("unexpected refusal: %v", err)
	}
	if got := sp.Gets(); got != 2 {
		t.Errorf("probe issued %d Get(s), want 2 — the guard must re-probe after an "+
			"inconclusive result, not treat it as settled", got)
	}
}

// TestCAS_AdoptRefusalIsCachedNotReprobed is the converse: a
// CONCLUSIVE verdict is cached.
//
// A refusal is a property of (repo, DEK), not of the chunk that
// happened to reveal it, so re-probing on every subsequent dedup hit
// would read and decrypt a chunk per hit to re-derive an answer
// already known — the exact work dedup exists to avoid, on the path
// that is already failing.
func TestCAS_AdoptRefusalIsCachedNotReprobed(t *testing.T) {
	ctx := context.Background()
	backing := adoptTestStore(t)
	bodies := seedChunks(t, backing, adoptEncryptor(t, 0xA1), 3)

	sp := &probeStore{StoragePlugin: backing}
	cas := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xB2)))

	for i, b := range bodies {
		if _, err := cas.PutChunk(ctx, b); err == nil {
			t.Fatalf("PutChunk %d adopted a chunk under a foreign DEK", i)
		}
	}
	if got := sp.Gets(); got != 1 {
		t.Errorf("guard issued %d Get(s) across 3 refusals, want exactly 1 — a settled "+
			"verdict must be reused, not re-derived per chunk", got)
	}
}

// TestCAS_AdoptProbeIsSingleFlight covers the concurrency the guard is
// actually used under: a backup chunks in parallel, so many goroutines
// reach a dedup hit at once.
//
// ensureAdoptable holds its mutex across a network Get, which is what
// makes one probe serve everybody. Both outcomes are checked, because
// the failure modes differ: with a matching DEK a broken guard shows up
// as N probes instead of 1, and with a foreign one it shows up as some
// goroutines adopting while others refuse.
func TestCAS_AdoptProbeIsSingleFlight(t *testing.T) {
	const workers = 8

	t.Run("MatchingDEK", func(t *testing.T) {
		ctx := context.Background()
		backing := adoptTestStore(t)
		bodies := seedChunks(t, backing, adoptEncryptor(t, 0xC3), workers)

		sp := &probeStore{StoragePlugin: backing}
		cas := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xC3)))

		errs := make([]error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				info, err := cas.PutChunk(ctx, bodies[i])
				if err == nil && !info.Deduped {
					err = fmt.Errorf("chunk %d rewritten rather than deduplicated", i)
				}
				errs[i] = err
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
			}
		}
		if got := sp.Gets(); got != 1 {
			t.Errorf("%d concurrent adoptions issued %d probes, want 1 — the guard is a "+
				"property of (repo, DEK), and probing per goroutine would read and decrypt "+
				"a chunk for each one", workers, got)
		}
	})

	t.Run("ForeignDEK", func(t *testing.T) {
		ctx := context.Background()
		backing := adoptTestStore(t)
		bodies := seedChunks(t, backing, adoptEncryptor(t, 0xC3), workers)

		sp := &probeStore{StoragePlugin: backing}
		cas := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xD4)))

		errs := make([]error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = cas.PutChunk(ctx, bodies[i])
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err == nil {
				t.Errorf("worker %d adopted a chunk under a foreign DEK while its peers "+
					"refused — the verdict must be the same for every goroutine, or whether "+
					"a backup is corrupt comes down to scheduling", i)
				continue
			}
			if !strings.Contains(err.Error(), "does not decrypt with this backup's data key") {
				t.Errorf("worker %d: unexpected error: %v", i, err)
			}
		}
		if got := sp.Gets(); got != 1 {
			t.Errorf("%d concurrent refusals issued %d probes, want 1", workers, got)
		}
	})
}

// TestCAS_AdoptGuardIgnoresFreshWrites is the false-positive check.
//
// The guard exists to stop ADOPTION of someone else's chunks. A chunk
// the repo does not hold raises no such question: it is written under
// this backup's own DEK and is readable by definition. If the guard
// ever fired here it would break the ordinary case — a new deployment
// writing into a repository that already holds another tenant's data
// could not take a first backup at all.
func TestCAS_AdoptGuardIgnoresFreshWrites(t *testing.T) {
	ctx := context.Background()
	backing := adoptTestStore(t)
	// The repo already holds another KEKRef's chunks...
	seedChunks(t, backing, adoptEncryptor(t, 0xA1), 2)

	sp := &probeStore{StoragePlugin: backing}
	cas := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xB2)))

	// ...but THIS body is new, so nothing is being adopted.
	fresh := bytes.Repeat([]byte("never-seen-before"), 512)
	info, err := cas.PutChunk(ctx, fresh)
	if err != nil {
		t.Fatalf("a fresh chunk was refused: %v\nNothing is being adopted here — the "+
			"chunk is written under this backup's own DEK", err)
	}
	if info.Deduped {
		t.Error("a chunk the repo did not hold was reported as deduplicated")
	}
	if got := sp.Gets(); got != 0 {
		t.Errorf("the guard probed %d time(s) on a fresh write; it must run only on "+
			"adoption", got)
	}
}

// probeStore makes the adopt probe observable and steerable: it counts
// Get calls, and can fail the first n of them with an error of the
// test's choosing.
//
// Counting is mutex-guarded rather than a plain int because the
// single-flight test drives it from many goroutines at once under
// -race.
type probeStore struct {
	storage.StoragePlugin

	mu       sync.Mutex
	gets     int
	failNext int   // Gets still to fail before delegating
	failWith error // what those Gets return
}

// failFirst makes the next n Get calls return err instead of reaching
// the backend.
func (p *probeStore) failFirst(n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failNext, p.failWith = n, err
}

func (p *probeStore) Gets() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gets
}

func (p *probeStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p.mu.Lock()
	p.gets++
	if p.failNext > 0 {
		p.failNext--
		err := p.failWith
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Unlock()
	return p.StoragePlugin.Get(ctx, key)
}

// The probes below fail with errTransient from review79_transient_test.go:
// a stand-in for a blip, a throttle, a 503 — a failure that says nothing
// about who holds which key. It is deliberately neither
// encryption.ErrAuthenticationFailed nor storage.ErrChecksumMismatch,
// the two errors the guard treats as conclusive.

// seedChunks writes n distinct chunks under enc and returns their
// bodies, so a later CAS can try to adopt them.
func seedChunks(t *testing.T, sp storage.StoragePlugin, enc encryption.Encryptor, n int) [][]byte {
	t.Helper()
	writer := repo.NewCAS(sp, repo.WithEncryptor(enc))
	bodies := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		b := bytes.Repeat([]byte(fmt.Sprintf("seed-%03d-", i)), 512)
		if _, err := writer.PutChunk(context.Background(), b); err != nil {
			t.Fatalf("seed chunk %d: %v", i, err)
		}
		bodies = append(bodies, b)
	}
	return bodies
}
