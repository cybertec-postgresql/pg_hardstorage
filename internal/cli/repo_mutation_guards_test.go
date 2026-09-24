package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func setReadOnly(t *testing.T, repoURL string) {
	t.Helper()
	if _, err := repo.SetMode(context.Background(), repo.SetModeOptions{URL: repoURL, Mode: repo.ModeReadOnly}); err != nil {
		t.Fatal(err)
	}
}

// M1: every mutating repair / wipe / import path refuses on a repository
// the operator locked read-only. Each used to write straight through it.
func TestReadOnlyRepo_MutatingRepairWipeImportRefuse(t *testing.T) {
	w := newReadWorld(t)
	id := commitManifestSignedBy(t, w, "db1", w.signer, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	replica := newReadWorld(t)
	bundleFile := filepath.Join(t.TempDir(), "b.tar")
	src := newReadWorld(t)
	commitMinimalManifest(t, src, "db1", "a", 1)
	if _, errb, exit := runCLI(t, "repo", "bundle", "export", "--repo", src.repoURL,
		"--deployment", "db1", "--out", bundleFile); exit != int(output.ExitOK) {
		t.Fatalf("bundle export: %s", errb)
	}
	setReadOnly(t, w.repoURL)

	for name, args := range map[string][]string{
		"repair attestation":  {"repair", "attestation", "db1", id, "--repo", w.repoURL, "--force"},
		"repair manifest":     {"repair", "manifest", "db1", id, "--repo", w.repoURL, "--force"},
		"repair chunks apply": {"repair", "chunks", "--orphans", "--apply", "--repo", w.repoURL},
		"repair scrub heal":   {"repair", "scrub", "--repo", w.repoURL, "--heal", "--replica", replica.repoURL},
		"repo wipe force":     {"repo", "wipe", w.repoURL, "--force", "--yes"},
		"repo bundle import":  {"repo", "bundle", "import", "--to", w.repoURL, "--in", bundleFile},
	} {
		t.Run(name, func(t *testing.T) {
			_, errb, exit := runCLI(t, append(args, "-o", "json")...)
			if exit != int(output.ExitConflict) || !strings.Contains(errb, "conflict.repo_read_only") {
				t.Fatalf("%s on a read-only repo: exit=%d, want 7 conflict.repo_read_only\n%s", name, exit, errb)
			}
		})
	}
	// The repository is intact.
	if _, err := backup.NewManifestStore(w.sp).Read(context.Background(), "db1", id, w.verifier); err != nil {
		t.Fatalf("manifest damaged on a read-only repo: %v", err)
	}
}

// M1: `repo wipe --force --yes` deleted backups under an active legal
// hold — the one thing every other path in the product refuses.
func TestRepoWipe_RefusesWhileLegalHoldActive(t *testing.T) {
	w := newReadWorld(t)
	id := commitManifestSignedBy(t, w, "db1", w.signer, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	if err := backup.NewManifestStore(w.sp).PutHold(context.Background(), "db1", id, "legal@acme", "litigation #7"); err != nil {
		t.Fatal(err)
	}
	_, errb, exit := runCLI(t, "repo", "wipe", w.repoURL, "--force", "--yes", "-o", "json")
	if exit != int(output.ExitConflict) || !strings.Contains(errb, "conflict.legal_hold") {
		t.Fatalf("wipe with an active hold: exit=%d, want 7 conflict.legal_hold\n%s", exit, errb)
	}
	if !strings.Contains(errb, "db1/"+id) {
		t.Errorf("refusal should name the held backup:\n%s", errb)
	}
	if _, err := backup.NewManifestStore(w.sp).Read(context.Background(), "db1", id, w.verifier); err != nil {
		t.Fatalf("held backup was deleted: %v", err)
	}
	// --force can never override a hold.
	_, errb, exit = runCLI(t, "repo", "wipe", w.repoURL, "--force", "--yes", "--override-legal-holds", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Fatalf("--override-legal-holds with --force: exit=%d, want 2\n%s", exit, errb)
	}
}

// H2: `repair attestation` verified the manifest only against the key
// embedded in its own body — attacker-controlled — and then re-signed it
// with the operator key. A manifest signed by an unknown key must be
// refused unless that key is a trusted retired operator key.
func TestRepairAttestation_RefusesUntrustedEmbeddedKey(t *testing.T) {
	w := newReadWorld(t)
	attackerDir := t.TempDir()
	attacker, _, err := keystore.LoadOrGenerate(attackerDir)
	if err != nil {
		t.Fatal(err)
	}
	id := commitManifestSignedBy(t, w, "db1", attacker, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	before := readKeyBytes(t, w.sp, backup.PrimaryPath("db1", id))

	_, errb, exit := runCLI(t, "repair", "attestation", "db1", id, "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) || !strings.Contains(errb, "verify.attestation_untrusted_key") {
		t.Fatalf("self-signed forgery: exit=%d, want 9 verify.attestation_untrusted_key\n%s", exit, errb)
	}
	if after := readKeyBytes(t, w.sp, backup.PrimaryPath("db1", id)); string(after) != string(before) {
		t.Fatal("the forged manifest was rewritten")
	}
	if _, err := backup.NewManifestStore(w.sp).Read(context.Background(), "db1", id, w.verifier); err == nil {
		t.Fatal("the forged manifest now verifies under the operator key — laundered")
	}

	// The same key installed as a trusted retired key under the keyring
	// is the legitimate rotation case and is accepted.
	kr := keyringDirOf(t)
	if err := os.MkdirAll(filepath.Join(kr, "trusted-keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(filepath.Join(attackerDir, keystore.PublicKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kr, "trusted-keys", "previous.pem"), pub, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errb, exit := runCLI(t, "repair", "attestation", "db1", id, "--repo", w.repoURL, "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("trusted retired key: exit=%d\n%s", exit, errb)
	}
}

// keyringDirOf returns the keyring directory the CLI resolves for this
// test's environment (the same one newReadWorld's signer lives in).
func keyringDirOf(t *testing.T) string {
	t.Helper()
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return p.Keyring.Value
}
