package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
)

func TestKMS_Inspect_EmptyKeyring(t *testing.T) {
	configDir(t)
	out, _, exit := runCmd(t, "kms", "inspect", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		`"signing_key"`,
		`"signing_pub"`,
		`"kek"`,
		`"present": false`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestKMS_Inspect_PopulatedKeyring(t *testing.T) {
	dir := configDir(t)
	// configDir sets HOME so paths.Resolve finds keyring under
	// $HOME/.local/share/pg_hardstorage/keyring (or similar XDG path).
	// Generate the signing keypair + KEK via the same code paths
	// the runner uses.
	w := newReadWorld(t)
	defer w.cleanup()
	keyringDir := filepath.Join(dir, "..", "data", "pg_hardstorage", "keyring")
	// Different XDG layouts produce different paths — find the
	// keyring by searching for the signing-private file.
	keyringDir = findKeyringDir(t)

	// Plant a KEK so the test reports `kek.present=true`.
	if _, _, err := keystore.LoadOrGenerateKEK(keyringDir); err != nil {
		t.Fatal(err)
	}

	out, _, exit := runCmd(t, "kms", "inspect", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	// Signing keypair + KEK all present.
	wantSubstrs := []string{
		`"name": "manifest_signing.ed25519"`,
		`"name": "manifest_signing.pub"`,
		`"name": "kek.bin"`,
		`"fingerprint_sha256":`,
	}
	for _, w := range wantSubstrs {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	// Multiple "present": true should appear (signing priv, signing
	// pub, KEK).
	if strings.Count(out, `"present": true`) < 3 {
		t.Errorf("expected ≥3 present:true (signing priv + pub + KEK):\n%s", out)
	}
}

// A signing private key with a too-permissive mode should surface a
// Warning. Operationally this is the indicator someone copied the
// keyring with `cp -r` and lost the 0600 bit.
func TestKMS_Inspect_WarnsOnPermissivePrivKey(t *testing.T) {
	configDir(t)
	keyringDir := findKeyringDir(t)

	// Ensure the dir exists, then plant a fake private key with
	// 0644 — the inspect path's warning should fire.
	if err := os.MkdirAll(keyringDir, 0o700); err != nil {
		t.Fatal(err)
	}
	priv := filepath.Join(keyringDir, keystore.PrivateKeyFile)
	if err := os.WriteFile(priv, []byte("not-real-key-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, exit := runCmd(t, "kms", "inspect", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(out, `"warning":`) ||
		!strings.Contains(out, "too permissive") {
		t.Errorf("expected mode-permissive warning:\n%s", out)
	}
}

// findKeyringDir resolves the same path paths.Resolve would for the
// current configDir-set HOME. Used by tests that want to plant keys
// where the CLI will discover them.
func findKeyringDir(t *testing.T) string {
	t.Helper()
	// The configDir test helper sets PG_HARDSTORAGE_CONFIG_DIR; the
	// keyring lands under <data>/keyring in the XDG resolution. The
	// simplest way: ask paths.Resolve directly via a spawned cmd.
	out, _, exit := runCmd(t, "doctor", "--output", "json")
	if exit != 0 {
		t.Fatal("doctor failed inside findKeyringDir")
	}
	// Pull "keyring_dir" from the doctor JSON.
	const marker = `"keyring_dir": "`
	idx := strings.Index(out, marker)
	if idx < 0 {
		t.Fatalf("doctor output missing keyring_dir:\n%s", out)
	}
	rest := out[idx+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatal("malformed keyring_dir")
	}
	return rest[:end]
}
