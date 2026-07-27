package keystore

import (
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// Local rotation stamps refs like "local:v2" on rotated manifests
// (kms rotate refuses old==new refs, so a new local ref is forced).
// Both resolvers used by restore/verify/drill must route ANY local:*
// ref to the keyring — restricting them to exactly "local:default"
// made every rotated backup unrestorable by any shipped code path.
func TestLocalRefs_ResolveToKeyring(t *testing.T) {
	dir := t.TempDir()
	kek, _, err := LoadOrGenerateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, ref := range []string{"", KEKRefLocal, "local:v2", "local:2026-rotation"} {
		got, err := KEKResolver(dir)(ref)
		if err != nil {
			t.Errorf("KEKResolver(%q): %v — rotated backups with this ref are unrestorable", ref, err)
			continue
		}
		if got != kek {
			t.Errorf("KEKResolver(%q) returned different key material", ref)
		}
	}

	// UnwrapDEK: wrap a DEK under the keyring KEK (same envelope
	// encryption.Wrap produces on manifests), then unwrap via a
	// rotated-style ref.
	var dek [encryption.KeyLen]byte
	for i := range dek {
		dek[i] = byte(i)
	}
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapDEK(context.Background(), "local:v2", wrapped, UnwrapOpts{KeyringDir: dir})
	if err != nil {
		t.Fatalf("UnwrapDEK(local:v2): %v — rotated manifests cannot be decrypted", err)
	}
	if string(got) != string(dek[:]) {
		t.Fatal("UnwrapDEK(local:v2) returned wrong DEK")
	}
}

// rotate.go (package backup) cannot import keystore (import cycle),
// so it mirrors KEKRefLocal as a private constant. Pin them together.
func TestLocalDefaultRefConstantsAgree(t *testing.T) {
	if got := backup.LocalDefaultKEKRefForTest(); got != KEKRefLocal {
		t.Fatalf("backup.localDefaultKEKRef = %q, keystore.KEKRefLocal = %q — the rotation alias slot would target the wrong ref", got, KEKRefLocal)
	}
}
