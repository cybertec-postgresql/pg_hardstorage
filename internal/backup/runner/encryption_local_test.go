package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
)

// The agent's schedule engine and the control-plane executor resolve
// encryption through this helper. Before it existed they built
// TakeOptions with no Encryption at all — every scheduled backup was
// silently plaintext in an --encrypt repo, and plaintext-hash dedup
// then welded plaintext manifests onto encrypted chunks (crypto-shred
// guarantee broken) and vice versa (restore failure).
func TestLocalEncryptionFromKeyring(t *testing.T) {
	t.Run("no_kek_means_plaintext", func(t *testing.T) {
		cfg, err := LocalEncryptionFromKeyring(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if cfg != nil {
			t.Fatalf("empty keyring: cfg = %+v, want nil", cfg)
		}
	})

	t.Run("kek_present_means_encrypt", func(t *testing.T) {
		dir := t.TempDir()
		// Materialise a KEK the way init --encrypt does.
		if _, generated, err := keystore.LoadOrGenerateKEK(dir); err != nil || !generated {
			t.Fatalf("generate KEK: generated=%v err=%v", generated, err)
		}
		cfg, err := LocalEncryptionFromKeyring(dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg == nil {
			t.Fatal("KEK present but helper chose plaintext — scheduled backups would silently not encrypt")
		}
		if cfg.KEKRef != keystore.KEKRefLocal {
			t.Errorf("KEKRef = %q, want %q", cfg.KEKRef, keystore.KEKRefLocal)
		}
		var zero [32]byte
		if cfg.KEK == zero {
			t.Error("KEK is all-zero — keyring key was not actually loaded")
		}
	})

	t.Run("corrupt_kek_is_an_error_not_plaintext", func(t *testing.T) {
		dir := t.TempDir()
		// A present-but-invalid KEK must fail the backup loudly, never
		// silently fall back to plaintext.
		if err := os.WriteFile(filepath.Join(dir, keystore.KEKFileName), []byte("short"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LocalEncryptionFromKeyring(dir); err == nil {
			t.Fatal("corrupt KEK accepted (or silently ignored) — want an error")
		}
	})
}
