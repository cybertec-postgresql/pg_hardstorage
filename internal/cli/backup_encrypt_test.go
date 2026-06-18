package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
)

func TestResolveBackupEncryption_NoKEK_NoEncrypt(t *testing.T) {
	dir := t.TempDir() // empty
	cfg, err := resolveBackupEncryption(context.Background(), dir, false, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Errorf("no KEK + no flags should yield nil EncryptionConfig; got %+v", cfg)
	}
}

func TestResolveBackupEncryption_KEKPresent_AutoEnables(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := keystore.LoadOrGenerateKEK(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveBackupEncryption(context.Background(), dir, false, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("KEK present + no flags should auto-enable encryption")
	}
	if cfg.KEKRef != keystore.KEKRefLocal {
		t.Errorf("KEKRef = %q, want %q", cfg.KEKRef, keystore.KEKRefLocal)
	}
}

func TestResolveBackupEncryption_NoEncryptOverridesKEK(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := keystore.LoadOrGenerateKEK(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveBackupEncryption(context.Background(), dir, false, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Errorf("--no-encrypt should disable encryption even with KEK present; got %+v", cfg)
	}
}

func TestResolveBackupEncryption_EncryptWithoutKEK_Errors(t *testing.T) {
	dir := t.TempDir() // empty
	_, err := resolveBackupEncryption(context.Background(), dir, true, false, "", nil)
	if err == nil {
		t.Fatal("expected error when --encrypt set without KEK")
	}
	if !strings.Contains(err.Error(), "KEK") {
		t.Errorf("error should mention KEK; got %v", err)
	}
}

func TestResolveBackupEncryption_BothFlags_Conflict(t *testing.T) {
	dir := t.TempDir()
	_, err := resolveBackupEncryption(context.Background(), dir, true, true, "", nil)
	if err == nil {
		t.Fatal("expected error on conflicting flags")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutually-exclusive; got %v", err)
	}
}
