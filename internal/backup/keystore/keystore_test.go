package keystore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
)

func TestLoadOrGenerate_FreshDirectory(t *testing.T) {
	dir := t.TempDir()
	signer, verifier, err := keystore.LoadOrGenerate(dir)
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if signer == nil || verifier == nil {
		t.Fatal("signer or verifier nil")
	}
	// Both files were created.
	for _, name := range []string{keystore.PrivateKeyFile, keystore.PublicKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s missing after generation: %v", name, err)
		}
	}
	// Private key must be exactly 0600.
	info, _ := os.Stat(filepath.Join(dir, keystore.PrivateKeyFile))
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("private key mode = %#o, want 0600", got)
	}
}

func TestLoadOrGenerate_Idempotent(t *testing.T) {
	dir := t.TempDir()
	signer1, _, err := keystore.LoadOrGenerate(dir)
	if err != nil {
		t.Fatal(err)
	}
	signer2, _, err := keystore.LoadOrGenerate(dir)
	if err != nil {
		t.Fatalf("second LoadOrGenerate: %v", err)
	}
	// Sign the same payload with both signers; results must match —
	// ed25519 is deterministic and the second call must reuse the same key.
	payload := []byte("idempotency-witness")
	a := signer1.Sign(payload)
	b := signer2.Sign(payload)
	if string(a) != string(b) {
		t.Error("two LoadOrGenerate calls returned different signers (key was regenerated)")
	}
}

func TestLoadOrGenerate_RefusesLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	// Generate first to create the files.
	if _, _, err := keystore.LoadOrGenerate(dir); err != nil {
		t.Fatal(err)
	}
	// Loosen the private-key permissions.
	if err := os.Chmod(filepath.Join(dir, keystore.PrivateKeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := keystore.LoadOrGenerate(dir)
	if err == nil {
		t.Fatal("expected error on world-readable private key")
	}
	if !strings.Contains(err.Error(), "0600") && !strings.Contains(err.Error(), "mode") {
		t.Errorf("error should explain the mode requirement; got %v", err)
	}
}

func TestLoadOrGenerate_RefusesUnpairedFiles(t *testing.T) {
	dir := t.TempDir()
	// Create only the private file; the public is missing.
	if err := os.WriteFile(filepath.Join(dir, keystore.PrivateKeyFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := keystore.LoadOrGenerate(dir)
	if err == nil {
		t.Fatal("expected error on unpaired files")
	}
	if !strings.Contains(err.Error(), "pair") {
		t.Errorf("error should mention pair: %v", err)
	}
}

func TestLoadOrGenerate_RefusesGarbagePEM(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{keystore.PrivateKeyFile, keystore.PublicKeyFile} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not pem"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := keystore.LoadOrGenerate(dir)
	if err == nil {
		t.Error("garbage PEM should error on parse")
	}
}

func TestLoadOrGenerate_CreatesKeyringDir(t *testing.T) {
	parent := t.TempDir()
	keyring := filepath.Join(parent, "nested", "keyring")
	if _, _, err := keystore.LoadOrGenerate(keyring); err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	info, err := os.Stat(keyring)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Error("keyring path should be a directory")
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Errorf("keyring directory mode = %#o; want no group/world bits", got)
	}
}

func TestLoadOrGenerate_NoTmpLeak(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := keystore.LoadOrGenerate(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf(".tmp file leaked: %s", e.Name())
		}
	}
}
