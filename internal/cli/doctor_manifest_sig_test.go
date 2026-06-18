package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// commitManifestSignedBy plants a minimal full-backup manifest at w's repo,
// signed by an arbitrary signer (not necessarily w.signer). Used to forge the
// #103/#104 situation: a backup whose embedded signing key does NOT match the
// keyring doctor will verify against.
func commitManifestSignedBy(t *testing.T, w *readWorld, deployment string, signer *backup.Signer, when time.Time) string {
	t.Helper()
	id := deployment + ".full." + when.UTC().Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        170000,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        when,
		StoppedAt:        when.Add(time.Minute),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: 3, Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("17\n")), Offset: 0, Len: 3}}},
		},
	}
	if err := w.store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit %s: %v", id, err)
	}
	return id
}

func manifestSigConfigYAML(repoURL, deployment string) string {
	return "deployments:\n  " + deployment + ":\n    pg_connection: postgres://x\n    repo: " + repoURL + "\n"
}

// TestDoctor_ManifestSignature_KeyMismatch_Warns is the #104 regression: a
// backup signed by a DIFFERENT key than the current keyring holds (the lost /
// ephemeral-keyring situation from #103) must make doctor surface a
// `manifest_signature_mismatch` warning and trip `--exit-on-issues` (exit 10).
// Before #104 doctor reported healthy here because it only checked that *a*
// signing key existed, not that it matched the backups.
func TestDoctor_ManifestSignature_KeyMismatch_Warns(t *testing.T) {
	w := newReadWorld(t)

	// A signer whose keypair lives in a throwaway keyring dir — guaranteed
	// different from w's keyring (which doctor will verify against).
	foreign, _, err := keystore.LoadOrGenerate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	commitManifestSignedBy(t, w, "db1", foreign,
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	writeReadWorldConfig(t, w, manifestSigConfigYAML(w.repoURL, "db1"))

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`manifest_signature_mismatch`,
		`"severity": "warning"`,
		`"manifest_signatures"`,
		`"deployment": "db1"`,
		`"key_mismatch": 1`,
		"signed with a DIFFERENT key",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor missing %q:\n%s", want, stdout)
		}
	}

	// The warning must gate --exit-on-issues (exit 10).
	_, _, exit2 := runCLI(t, "doctor", "--exit-on-issues")
	if exit2 != int(output.ExitDoctorIssues) {
		t.Errorf("doctor --exit-on-issues exit=%d, want %d (ExitDoctorIssues)",
			exit2, int(output.ExitDoctorIssues))
	}
}

// TestDoctor_ManifestSignature_MatchingKey_Clean is the negative half of the
// #104 regression: a backup signed by the SAME key the keyring holds must NOT
// trip the signature check — no `manifest_signature_mismatch`, no
// `manifest_signature_failures`, and the deployment reports zero mismatches.
func TestDoctor_ManifestSignature_MatchingKey_Clean(t *testing.T) {
	w := newReadWorld(t)
	commitManifestSignedBy(t, w, "db1", w.signer,
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	writeReadWorldConfig(t, w, manifestSigConfigYAML(w.repoURL, "db1"))

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	for _, bad := range []string{
		"manifest_signature_mismatch",
		"manifest_signature_failures",
	} {
		if strings.Contains(stdout, bad) {
			t.Errorf("matching key should NOT emit %q:\n%s", bad, stdout)
		}
	}
	// The deployment is still reported, with a clean (zero-mismatch) tally.
	for _, want := range []string{
		`"manifest_signatures"`,
		`"deployment": "db1"`,
		`"key_mismatch": 0`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor missing %q:\n%s", want, stdout)
		}
	}
}

// TestDoctor_ManifestSignature_NoBackups_Silent: a deployment with no backups
// emits no manifest_signatures row (omitempty) and no signature issue — the
// "no backups" signal is repoCheckReport's territory.
func TestDoctor_ManifestSignature_NoBackups_Silent(t *testing.T) {
	w := newReadWorld(t)
	writeReadWorldConfig(t, w, manifestSigConfigYAML(w.repoURL, "db1"))

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	if strings.Contains(stdout, `"manifest_signatures"`) {
		t.Errorf("manifest_signatures should be omitted with no backups:\n%s", stdout)
	}
	if strings.Contains(stdout, "manifest_signature_mismatch") {
		t.Errorf("no backups should not emit a mismatch warning:\n%s", stdout)
	}
}
