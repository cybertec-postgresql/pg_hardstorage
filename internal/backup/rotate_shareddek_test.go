package backup_test

import (
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/sharedkey"
)

// `kms rotate` used to rewrap every manifest but leave the
// authoritative shared-DEK object (keys/shared-dek/) under the
// retired KEK. The next backup's ResolveOrMint found an object it
// could not unwrap and every future backup and `wal stream`
// hard-failed (UnusableCandidate) until the operator deleted an
// undocumented internal object. Rotation must migrate the object —
// same DEK, new wrap — and, for local custody, keep the fixed
// "local:default" ref (which the CLI/agent stamp on every backup)
// resolving to that same DEK so dedup never welds new manifests onto
// chunks sealed with a different key.
func TestRotateKEK_MigratesSharedDEKObject(t *testing.T) {
	w := setupRotateWorld(t)
	ctx := context.Background()
	oldKEK, newKEK := mkKEK(t), mkKEK(t)

	unwrapWith := func(kek [encryption.KeyLen]byte) sharedkey.Unwrapper {
		return func(wrapped []byte) ([]byte, error) {
			d, err := encryption.Unwrap(kek, wrapped)
			if err != nil {
				return nil, err
			}
			return d[:], nil
		}
	}
	wrapWith := func(kek [encryption.KeyLen]byte) sharedkey.Wrapper {
		return func(dek [encryption.KeyLen]byte) ([]byte, error) {
			return encryption.Wrap(kek, dek)
		}
	}

	// Seed: one encrypted manifest + the shared-DEK object, both
	// under local:default / oldKEK — the post-#31 steady state.
	w.commitEncrypted(t, "db1", "db1.full.20260430T120000Z.aaaa", oldKEK, "local:default", 0)
	seed, err := sharedkey.ResolveOrMint(ctx, w.sp, "local:default", unwrapWith(oldKEK), wrapWith(oldKEK))
	if err != nil || !seed.Have {
		t.Fatalf("seed shared DEK: have=%v err=%v", seed.Have, err)
	}

	res, err := backup.RotateKEK(ctx, w.sp, backup.RotateKEKOptions{
		OldKEKRef: "local:default", OldKEK: oldKEK,
		NewKEKRef: "local:v2", NewKEK: newKEK,
		Signer: w.signer, Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("RotateKEK: %v", err)
	}
	if res.Rotated != 1 || res.Failed != 0 {
		t.Fatalf("rotated=%d failed=%d, want 1/0", res.Rotated, res.Failed)
	}
	if !res.SharedDEKMigrated {
		t.Fatal("SharedDEKMigrated=false — the shared-DEK object was left under the retired KEK")
	}

	// The NEXT backup resolves under the new ref with the new KEK and
	// must get the SAME DEK (chunk dedup depends on it).
	got, err := sharedkey.ResolveOrMint(ctx, w.sp, "local:v2", unwrapWith(newKEK), wrapWith(newKEK))
	if err != nil {
		t.Fatal(err)
	}
	if got.UnusableCandidate {
		t.Fatal("local:v2 resolve → UnusableCandidate: backups are bricked after rotation")
	}
	if !got.Have || got.DEK != seed.DEK {
		t.Fatalf("local:v2 DEK changed across rotation (have=%v) — dedup would weld manifests onto chunks sealed with the old DEK", got.Have)
	}

	// The CLI/agent stamp the FIXED ref local:default on every local
	// backup; that slot must also resolve to the SAME DEK under the
	// new KEK (the alias slot rotation writes).
	got2, err := sharedkey.ResolveOrMint(ctx, w.sp, "local:default", unwrapWith(newKEK), wrapWith(newKEK))
	if err != nil {
		t.Fatal(err)
	}
	if got2.UnusableCandidate || !got2.Have || got2.DEK != seed.DEK {
		t.Fatalf("local:default after rotation: have=%v unusable=%v sameDEK=%v — the next scheduled backup would brick or fork the DEK",
			got2.Have, got2.UnusableCandidate, got2.DEK == seed.DEK)
	}
}

// A stale shared-DEK object that will not unwrap (a rotation that
// predates the migration fix) must no longer hard-fail every backup:
// ResolveOrMint falls through to the manifest scan and adopts the
// DEK from a freshly-rotated manifest.
func TestResolveOrMint_StaleObjectFallsThroughToManifests(t *testing.T) {
	w := setupRotateWorld(t)
	ctx := context.Background()
	oldKEK, newKEK := mkKEK(t), mkKEK(t)

	unwrapNew := func(wrapped []byte) ([]byte, error) {
		d, err := encryption.Unwrap(newKEK, wrapped)
		if err != nil {
			return nil, err
		}
		return d[:], nil
	}
	wrapNew := func(dek [encryption.KeyLen]byte) ([]byte, error) {
		return encryption.Wrap(newKEK, dek)
	}
	unwrapOld := func(wrapped []byte) ([]byte, error) {
		d, err := encryption.Unwrap(oldKEK, wrapped)
		if err != nil {
			return nil, err
		}
		return d[:], nil
	}
	wrapOld := func(dek [encryption.KeyLen]byte) ([]byte, error) {
		return encryption.Wrap(oldKEK, dek)
	}

	// Plant a STALE shared-DEK object wrapped under the OLD KEK (what
	// a pre-fix rotation leaves behind): mint it into an empty repo…
	if seed, err := sharedkey.ResolveOrMint(ctx, w.sp, "local:default", unwrapOld, wrapOld); err != nil || !seed.Have {
		t.Fatalf("seed stale object: have=%v err=%v", seed.Have, err)
	}
	// …then commit a manifest wrapped under the NEW KEK at the same
	// ref (as a completed legacy manifest rotation leaves them).
	w.commitEncrypted(t, "db1", "db1.full.20260430T120000Z.bbbb", newKEK, "local:default", 0)

	got, err := sharedkey.ResolveOrMint(ctx, w.sp, "local:default", unwrapNew, wrapNew)
	if err != nil {
		t.Fatal(err)
	}
	if got.UnusableCandidate {
		t.Fatal("stale object still hard-fails ResolveOrMint — pre-fix behaviour (every backup bricked)")
	}
	if !got.Have {
		t.Fatal("expected DEK adopted from the rotated manifest")
	}
}
