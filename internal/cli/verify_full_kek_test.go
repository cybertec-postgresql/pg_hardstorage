// verify_full_kek_test.go — pins the KEK resolver wiring for
// verify --full / standby create / timetravel restore.
//
// Pre-fix the package's resolveKEKForVerify was a hardcoded
// "no KEK known" sentinel, so any encrypted backup failed
// `verify --full` with "KEK resolver not wired" before the
// sandbox even started.  That's a #98-class doc-vs-code drift:
// the docs say verify --full works against encrypted backups,
// but the code refused them by construction.
//
// The fix routes resolveKEKForVerify through
// keystore.KEKResolver(p.Keyring.Value) — the same resolver
// the regular restore path uses.  This test pins that wiring
// so a future refactor doesn't silently reinstate the stub.
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
)

// TestResolveKEKForVerify_FindsLocalKEK: a KEK file written
// to the keystore dir must be resolvable via the package's
// shared helper.  Pins the contract verify --full / standby
// create / timetravel restore all depend on.
func TestResolveKEKForVerify_FindsLocalKEK(t *testing.T) {
	// Isolate to a tempdir so we don't depend on the
	// caller's keystore state.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	// Write a 32-byte KEK at the conventional keyring path
	// the keystore.KEKResolver looks for ($XDG_CONFIG_HOME/
	// pg_hardstorage/keyring/kek.bin).
	keyringDir := filepath.Join(home, ".config", "pg_hardstorage", "keyring")
	if err := os.MkdirAll(keyringDir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := [32]byte{}
	for i := range want {
		want[i] = byte(i) // deterministic, not all-zero
	}
	if err := os.WriteFile(filepath.Join(keyringDir, keystore.KEKFileName),
		want[:], 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := resolveKEKForVerify("local:default")
	if err != nil {
		t.Fatalf("resolveKEKForVerify: %v", err)
	}
	if got != want {
		t.Errorf("KEK mismatch: got %x want %x", got, want)
	}
}

// TestResolveKEKForVerify_DoesNotReturnStubError: the historic
// "KEK resolver not wired" sentinel string must NOT appear in
// any error this resolver returns.  A regression that
// reinstates the stub would surface here even if the function
// signature stays the same.
func TestResolveKEKForVerify_DoesNotReturnStubError(t *testing.T) {
	// Point at a non-existent home so the resolver fails
	// naturally (no KEK file).  We want to see the new failure
	// shape, not the stub's hardcoded sentinel string.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	_, err := resolveKEKForVerify("local:default")
	if err == nil {
		// Some keystore configurations on the test host might
		// successfully resolve via OS keychain.  That's fine —
		// the stub-detection test is only meaningful when the
		// resolver returns an error, so silently skip when it
		// happens to succeed.
		t.Skip("resolver succeeded on this host; stub-string check N/A")
	}
	if strings.Contains(err.Error(), "KEK resolver not wired") {
		t.Errorf("resolver regressed to the pre-fix stub sentinel: %v", err)
	}
}
