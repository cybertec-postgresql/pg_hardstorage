package keystore

import (
	"os"
	"path/filepath"
	"testing"
)

// A crash between ShredKEK's zero-overwrite and its unlink left a
// 32-byte kek.bin of zeros that both loaders accepted as a key: the
// next backup was "encrypted" under a publicly known KEK. An all-zero
// key must be refused, and a shred interrupted after its first step
// must never leave anything at kek.bin that loads.
func TestKEK_AllZeroRefusedAndShredRenamesFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KEKFileName)
	if err := os.WriteFile(path, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrGenerateKEK(dir); err == nil {
		t.Fatal("LoadOrGenerateKEK accepted an all-zero kek.bin")
	}
	if _, err := KEKResolver(dir)(KEKRefLocal); err == nil {
		t.Fatal("KEKResolver accepted an all-zero kek.bin")
	}

	// Simulate a shred that crashed right after moving the key aside:
	// the canonical path must be gone, and a re-run finishes the job.
	if err := os.WriteFile(path, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(dir, shredPendingName)); err != nil {
		t.Fatal(err)
	}
	if err := ShredKEK(dir); err != nil {
		t.Fatalf("re-run of an interrupted shred: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, shredPendingName)); !os.IsNotExist(err) {
		t.Fatalf("interrupted shred left its pending file: %v", err)
	}
}
