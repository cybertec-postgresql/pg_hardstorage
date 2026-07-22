package sharedkey_test

import (
	"context"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/sharedkey"
)

func wrapperFor(kek [encryption.KeyLen]byte) sharedkey.Wrapper {
	return func(dek [encryption.KeyLen]byte) ([]byte, error) { return encryption.Wrap(kek, dek) }
}

// Regression for issue #31: N writers that call ResolveOrMint concurrently
// against a fresh repo (no prior DEK) MUST all converge on the SAME DEK.
// The old Resolve-then-mint path had each writer mint its own fresh DEK
// when neither had committed a manifest yet, so a WAL full-page image that
// deduped against a base-backup chunk was stored under a different DEK than
// the backup manifest referenced it by — leaving the backup unrestorable.
func TestResolveOrMint_ConcurrentWritersConverge(t *testing.T) {
	const kekRef = "local:default"
	kek := testKEK(7)
	unwrap := unwrapperFor(kek)
	wrap := wrapperFor(kek)

	const N = 24
	sp := newSP(t) // shared store — all writers race on the same shared-DEK object

	var wg sync.WaitGroup
	deks := make([][encryption.KeyLen]byte, N)
	errs := make([]error, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // maximise the race window
			res, err := sharedkey.ResolveOrMint(context.Background(), sp, kekRef, unwrap, wrap)
			if err != nil {
				errs[i] = err
				return
			}
			if !res.Have {
				t.Errorf("writer %d: no DEK", i)
				return
			}
			deks[i] = res.DEK
		}(i)
	}
	close(start)
	wg.Wait()

	var want [encryption.KeyLen]byte
	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		if i == 0 {
			want = deks[0]
			continue
		}
		if deks[i] != want {
			t.Fatalf("writer %d minted a DIVERGENT DEK: %x != %x (issue #31 race)", i, deks[i][:8], want[:8])
		}
	}
	// And a fresh resolve reads back the same committed DEK.
	res, err := sharedkey.ResolveOrMint(context.Background(), sp, kekRef, unwrap, wrap)
	if err != nil || !res.Have || res.DEK != want {
		t.Fatalf("post-hoc resolve = (%x, have=%v, err=%v), want %x", res.DEK[:8], res.Have, err, want[:8])
	}
}

// A legacy repo (a committed manifest carries a DEK, but no shared-DEK
// object exists yet) must ADOPT that manifest DEK, not mint a new one.
func TestResolveOrMint_AdoptsLegacyManifestDEK(t *testing.T) {
	const kekRef = "local:default"
	kek := testKEK(3)
	sp := newSP(t)

	var legacy [encryption.KeyLen]byte
	for i := range legacy {
		legacy[i] = 0xAB
	}
	wrapped := mustWrap(t, kek, legacy)
	putEnvelopeManifest(t, sp, "manifests/db1/backups/b1/manifest.json", kekRef, wrapped)

	res, err := sharedkey.ResolveOrMint(context.Background(), sp, kekRef, unwrapperFor(kek), wrapperFor(kek))
	if err != nil || !res.Have {
		t.Fatalf("adopt: err=%v have=%v", err, res.Have)
	}
	if res.DEK != legacy {
		t.Fatalf("adopted DEK = %x, want the legacy manifest DEK %x", res.DEK[:8], legacy[:8])
	}
}

// A shared-DEK object wrapped under a DIFFERENT KEK than the caller's must
// yield UnusableCandidate (fail), never a fresh mint — a fresh DEK would
// leave existing deduped chunks unrestorable.
func TestResolveOrMint_WrongKEKIsUnusable(t *testing.T) {
	const kekRef = "local:default"
	owner := testKEK(1)
	stranger := testKEK(9)
	sp := newSP(t)

	// Owner mints the shared DEK.
	if _, err := sharedkey.ResolveOrMint(context.Background(), sp, kekRef, unwrapperFor(owner), wrapperFor(owner)); err != nil {
		t.Fatalf("owner mint: %v", err)
	}
	// Stranger (wrong KEK) must not mint a divergent DEK.
	res, err := sharedkey.ResolveOrMint(context.Background(), sp, kekRef, unwrapperFor(stranger), wrapperFor(stranger))
	if err != nil {
		t.Fatalf("stranger: unexpected hard error %v", err)
	}
	if res.Have || !res.UnusableCandidate {
		t.Fatalf("stranger got have=%v unusable=%v, want unusable (must not fork a DEK)", res.Have, res.UnusableCandidate)
	}
}
