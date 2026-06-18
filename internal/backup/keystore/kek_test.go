package keystore_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

func TestLoadOrGenerateKEK_GeneratesOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	kek, generated, err := keystore.LoadOrGenerateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Error("expected generated=true on first run")
	}
	var zero [encryption.KeyLen]byte
	if kek == zero {
		t.Error("generated KEK is all-zero")
	}
	// File must exist with mode 0600.
	info, err := os.Stat(filepath.Join(dir, keystore.KEKFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("KEK mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrGenerateKEK_ReturnsSameKeyOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	first, _, err := keystore.LoadOrGenerateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, generated, err := keystore.LoadOrGenerateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	if generated {
		t.Error("expected generated=false on second run")
	}
	if first != second {
		t.Error("KEK round-trip differs between calls")
	}
}

func TestLoadOrGenerateKEK_RejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, keystore.KEKFileName),
		[]byte("not 32 bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := keystore.LoadOrGenerateKEK(dir)
	if err == nil {
		t.Fatal("expected error on wrong-size KEK file")
	}
	if !strings.Contains(err.Error(), "want 32") {
		t.Errorf("error should mention expected size; got %v", err)
	}
}

func TestLoadOrGenerateKEK_RejectsEmptyDir(t *testing.T) {
	_, _, err := keystore.LoadOrGenerateKEK("")
	if err == nil {
		t.Fatal("expected error on empty dir")
	}
}

func TestKEKExists_ReportsTrueAfterGenerate(t *testing.T) {
	dir := t.TempDir()
	if keystore.KEKExists(dir) {
		t.Error("KEKExists true on empty dir")
	}
	_, _, _ = keystore.LoadOrGenerateKEK(dir)
	if !keystore.KEKExists(dir) {
		t.Error("KEKExists false after generate")
	}
}

func TestKEKResolver_Local(t *testing.T) {
	dir := t.TempDir()
	expected, _, _ := keystore.LoadOrGenerateKEK(dir)

	resolver := keystore.KEKResolver(dir)
	got, err := resolver(keystore.KEKRefLocal)
	if err != nil {
		t.Fatal(err)
	}
	if got != expected {
		t.Error("resolver returned wrong key")
	}
	// Empty ref also resolves to local for back-compat.
	got, err = resolver("")
	if err != nil || got != expected {
		t.Errorf("empty ref should resolve to local; got=%v err=%v", got, err)
	}
}

func TestKEKResolver_RejectsUnknownRef(t *testing.T) {
	dir := t.TempDir()
	_, _, _ = keystore.LoadOrGenerateKEK(dir)
	resolver := keystore.KEKResolver(dir)
	_, err := resolver("kms:aws:arn:foo")
	if err == nil {
		t.Fatal("expected error on unknown ref")
	}
}

func TestKEKResolver_FailsWhenMissing(t *testing.T) {
	dir := t.TempDir() // no kek.bin generated
	resolver := keystore.KEKResolver(dir)
	_, err := resolver(keystore.KEKRefLocal)
	if err == nil {
		t.Fatal("expected error when KEK file is absent")
	}
	// The OS error chain should still walk to fs.ErrNotExist for tools
	// that pivot on it.
	if !errors.Is(err, os.ErrNotExist) {
		t.Logf("note: resolver error is %v (not wrapping os.ErrNotExist)", err)
	}
}

// TestLoadOrGenerateKEK_RefusesWorldReadable: a kek.bin with mode
// 0644 (group/world readable) must be REFUSED on read. The
// signing key already enforces this; the KEK is at least as
// sensitive (it unwraps every backup's DEK) so the asymmetry has
// to go away. Regression guard for v8 audit Bug #15.
func TestLoadOrGenerateKEK_RefusesWorldReadable(t *testing.T) {
	dir := t.TempDir()
	kekPath := filepath.Join(dir, keystore.KEKFileName)

	// Plant a 32-byte KEK with the wrong perms.
	if err := os.WriteFile(kekPath, make([]byte, encryption.KeyLen), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := keystore.LoadOrGenerateKEK(dir)
	if err == nil {
		t.Fatal("expected refusal on mode-0644 KEK")
	}
	if !strings.Contains(err.Error(), "0600") {
		t.Errorf("error should name 0600 in the chmod hint; got %v", err)
	}
}

// TestKEKResolver_RefusesWorldReadable: same posture for the
// restore-time path. A KEK readable by other system users must
// not be silently consumed by the chunk-decrypt pipeline.
func TestKEKResolver_RefusesWorldReadable(t *testing.T) {
	dir := t.TempDir()
	kekPath := filepath.Join(dir, keystore.KEKFileName)
	if err := os.WriteFile(kekPath, make([]byte, encryption.KeyLen), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver := keystore.KEKResolver(dir)
	_, err := resolver(keystore.KEKRefLocal)
	if err == nil {
		t.Fatal("expected resolver refusal on mode-0644 KEK")
	}
	if !strings.Contains(err.Error(), "0600") {
		t.Errorf("error should name 0600 in the chmod hint; got %v", err)
	}
}

// TestLoadOrGenerateKEK_RefusesGroupReadable: even group-readable
// (mode 0640) should be refused. A leaky group membership is
// just as dangerous as world-readable in shared environments.
func TestLoadOrGenerateKEK_RefusesGroupReadable(t *testing.T) {
	dir := t.TempDir()
	kekPath := filepath.Join(dir, keystore.KEKFileName)
	if err := os.WriteFile(kekPath, make([]byte, encryption.KeyLen), 0o640); err != nil {
		t.Fatal(err)
	}
	_, _, err := keystore.LoadOrGenerateKEK(dir)
	if err == nil {
		t.Fatal("expected refusal on mode-0640 KEK")
	}
}
