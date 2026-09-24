package sharedkey_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/sharedkey"
)

// M94: a TRANSIENT KMS failure while unwrapping the canonical shared-DEK
// object was folded into "won't unwrap": ResolveOrMint fell through to
// the manifest scan, adopted an arbitrary legacy manifest DEK, and
// OVERWROTE the canonical object with it — every later writer then
// encrypted under a DEK the existing chunks were not written with. A
// transient failure must propagate, and the canonical object must be
// left exactly as it was.
func TestResolveOrMint_TransientUnwrapNeverOverwritesCanonical(t *testing.T) {
	const kekRef = "local:default"
	kek := testKEK(3)
	sp := newSP(t)
	ctx := context.Background()

	// The canonical shared DEK.
	first, err := sharedkey.ResolveOrMint(ctx, sp, kekRef, unwrapperFor(kek), wrapperFor(kek), time.Time{}, "")
	if err != nil || !first.Have {
		t.Fatalf("mint: %v %v", err, first.Have)
	}
	// A legacy manifest carrying a DIFFERENT DEK under the same ref.
	var legacy [encryption.KeyLen]byte
	for i := range legacy {
		legacy[i] = 0x5A
	}
	legacyWrap := mustWrap(t, kek, legacy)
	putEnvelopeManifest(t, sp, "manifests/db1/backups/old/manifest.json", kekRef, legacyWrap)

	// KMS is throttling: every unwrap of anything but the legacy wrap
	// fails transiently.
	throttled := errors.New("kms: ThrottlingException: rate exceeded")
	flaky := func(w []byte) ([]byte, error) {
		if string(w) == string(legacyWrap) {
			return unwrapperFor(kek)(w)
		}
		return nil, throttled
	}
	res, err := sharedkey.ResolveOrMint(ctx, sp, kekRef, flaky, wrapperFor(kek), time.Time{}, "")
	if err == nil {
		t.Fatalf("a transient unwrap failure resolved to have=%v unusable=%v; want the error propagated", res.Have, res.UnusableCandidate)
	}
	if !errors.Is(err, throttled) {
		t.Errorf("err = %v; want it to wrap the KMS error", err)
	}

	// The canonical DEK is untouched: a healthy resolve still returns it.
	again, err := sharedkey.ResolveOrMint(ctx, sp, kekRef, unwrapperFor(kek), wrapperFor(kek), time.Time{}, "")
	if err != nil || !again.Have || again.DEK != first.DEK {
		t.Fatalf("canonical shared DEK was replaced (err=%v): got %x want %x", err, again.DEK[:4], first.DEK[:4])
	}
}

// The legacy manifest scan must not skip past a candidate it could not
// ASK about and adopt a later one.
func TestResolve_TransientUnwrapPropagates(t *testing.T) {
	const kekRef = "local:default"
	kek := testKEK(4)
	sp := newSP(t)
	dek, _ := encryption.GenerateDEK()
	putEnvelopeManifest(t, sp, "manifests/db1/backups/a/manifest.json", kekRef, mustWrap(t, kek, dek))
	_, err := sharedkey.Resolve(context.Background(), sp, kekRef, func([]byte) ([]byte, error) {
		return nil, errors.New("dial tcp: i/o timeout")
	})
	if err == nil {
		t.Fatal("a transient unwrap error during the scan was swallowed")
	}
}
