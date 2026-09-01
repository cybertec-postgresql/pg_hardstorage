package keystore_test

// kek_durability_test.go — LoadOrGenerateKEK must not hand back a key
// it failed to persist.
//
// The caller uses the returned KEK immediately: it wraps the shared
// DEK, and every chunk of the backup is sealed under that DEK. If the
// on-disk kek.bin did not actually land, the backup is encrypted with a
// key that exists nowhere. Nothing detects it at the time — the backup
// verifies, the manifest commits, the operator gets exit 0 — and the
// loss surfaces at the one moment it cannot be tolerated.
//
// The original code wrote the file, fsynced it, and closed it with a
// bare `defer f.Close()`, so a Close-time ENOSPC/EDQUOT (the normal
// place a delayed-allocation or network filesystem reports a write it
// accepted earlier) was discarded. It also never fsynced the keyring
// directory, so the newly-created dentry could be lost outright to a
// power cut.
//
// A Close failure cannot be provoked portably from a Go test, so the
// static half of this lives in internal/fsutil's write-Close guard.
// What IS testable, and is the property that actually matters, is the
// end-to-end one: whatever the function returns must equal what a
// subsequent independent read of the file produces.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

func TestLoadOrGenerateKEK_ReturnedKeyIsTheKeyOnDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keyring")

	kek, generated, err := keystore.LoadOrGenerateKEK(dir)
	if err != nil {
		t.Fatalf("LoadOrGenerateKEK: %v", err)
	}
	if !generated {
		t.Fatal("first call reported generated=false on an empty keyring dir")
	}

	onDisk, err := os.ReadFile(filepath.Join(dir, keystore.KEKFileName))
	if err != nil {
		t.Fatalf("kek.bin unreadable after a call that reported success: %v\n"+
			"    the returned key is already being used to wrap a DEK; if the file is not "+
			"there, every backup taken with it is unrecoverable", err)
	}
	if len(onDisk) != encryption.KeyLen {
		t.Fatalf("kek.bin is %d bytes, want %d — the write did not complete",
			len(onDisk), encryption.KeyLen)
	}
	if !bytes.Equal(onDisk, kek[:]) {
		t.Fatal("kek.bin does not contain the key LoadOrGenerateKEK returned")
	}
}

// The keyring directory has to exist on disk, not just in the page
// cache, before the file inside it can be durable. The function creates
// it with MkdirAll; this pins that it is actually there and owner-only.
func TestLoadOrGenerateKEK_CreatesKeyringDirOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "keyring")

	if _, _, err := keystore.LoadOrGenerateKEK(dir); err != nil {
		t.Fatalf("LoadOrGenerateKEK: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("keyring dir absent after a successful call: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("keyring path is not a directory")
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("keyring dir mode %#o exposes group/other bits", perm)
	}
	fi, err := os.Stat(filepath.Join(dir, keystore.KEKFileName))
	if err != nil {
		t.Fatalf("kek.bin absent: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("kek.bin mode %#o exposes group/other bits — the load path refuses these", perm)
	}
}
