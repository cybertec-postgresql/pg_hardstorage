package cli_test

// keyring_install_test.go — the initContainer copy, tested the way
// Kubernetes actually serves Secrets: files behind a ..data symlink
// farm, modes rewritten by fsGroup (group-read OR'd in), and the
// keystore gates demanding owner-only bits on the other side.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
)

// k8sSecretDir fabricates the mount layout kubelet builds: real files
// in a timestamped dir, a ..data symlink to it, and name symlinks
// through ..data — with the group-read modes fsGroup produces.
func k8sSecretDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	mount := t.TempDir()
	data := filepath.Join(mount, "..2026_08_09_data")
	if err := os.Mkdir(data, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(data, name), body, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(data, filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	for name := range files {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(mount, name)); err != nil {
			t.Fatal(err)
		}
	}
	return mount
}

func TestKeyringInstall_FsGroupMangledSecretBecomesLoadable(t *testing.T) {
	// Real keyring material: the loadability assertion below parses
	// content, not just modes.
	seed := t.TempDir()
	if _, _, err := keystore.LoadOrGenerate(seed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := keystore.LoadOrGenerateKEK(seed); err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(seed, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	src := k8sSecretDir(t, map[string][]byte{
		keystore.PrivateKeyFile: read(keystore.PrivateKeyFile),
		keystore.PublicKeyFile:  read(keystore.PublicKeyFile),
		keystore.KEKFileName:    read(keystore.KEKFileName),
	})
	dst := filepath.Join(t.TempDir(), "keyring")

	stdout, errb, exit := runCLI(t, "keyring", "install", "--from", src, "--to", dst, "-o", "text")
	if exit != 0 {
		t.Fatalf("install exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, errb)
	}
	for name, want := range map[string]os.FileMode{
		keystore.PrivateKeyFile: 0o600,
		keystore.KEKFileName:    0o600,
		keystore.PublicKeyFile:  0o644,
	} {
		info, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("%s not installed: %v", name, err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v — the whole point is surviving fsGroup's "+
				"group-read", name, info.Mode().Perm(), want)
		}
	}
	// The keystore must actually LOAD the result — the end-to-end
	// property the initContainer exists for.
	if _, _, err := keystore.LoadOrGenerate(dst); err != nil {
		t.Fatalf("keystore refused the installed keyring: %v", err)
	}
	if _, _, err := keystore.LoadOrGenerateKEK(dst); err != nil {
		t.Fatalf("KEK gate refused the installed kek.bin: %v", err)
	}
}

func TestKeyringInstall_SubsetIsLegitimate(t *testing.T) {
	// Plaintext-only deployment: signing keys, no KEK.
	src := k8sSecretDir(t, map[string][]byte{
		keystore.PrivateKeyFile: []byte("private-key-bytes-private-key-bytes-private-key"),
		keystore.PublicKeyFile:  []byte("public-key-bytes"),
	})
	dst := filepath.Join(t.TempDir(), "keyring")
	_, errb, exit := runCLI(t, "keyring", "install", "--from", src, "--to", dst, "-o", "text")
	if exit != 0 {
		t.Fatalf("subset install refused (%d): %s", exit, errb)
	}
	if _, err := os.Stat(filepath.Join(dst, keystore.KEKFileName)); !os.IsNotExist(err) {
		t.Errorf("kek.bin appeared from nowhere: err=%v", err)
	}
}

func TestKeyringInstall_EmptySourceRefused(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "keyring")
	_, errb, exit := runCLI(t, "keyring", "install", "--from", src, "--to", dst, "-o", "text")
	if exit == 0 {
		t.Fatal("empty source accepted — a misconfigured mount must fail the " +
			"initContainer loudly, not produce a keyless pod that limps along")
	}
	if !strings.Contains(errb, "install_empty_source") {
		t.Errorf("error should carry the typed code: %s", errb)
	}
}
