package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// commitManifestWithPGVersion plants a manifest at the given
// repo with a specific PG_VERSION_NUM-style pg_version (MMmmpp,
// e.g. 180000 for PG 18.0). Bypasses the keystore by using
// the existing readWorld signer.
func commitManifestWithPGVersion(t *testing.T, w *readWorld, deployment string, pgVersion int, when time.Time) string {
	t.Helper()
	id := deployment + ".full." + when.UTC().Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        pgVersion,
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
				Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("18\n")), Offset: 0, Len: 3}}},
		},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

// pgVersionConfigYAML produces a minimal pg_hardstorage.yaml
// body with one deployment pointing at w.repoURL. Reused
// across the PG-version doctor tests; passed to the existing
// writeReadWorldConfig helper.
func pgVersionConfigYAML(repoURL, deployment string) string {
	return "deployments:\n  " + deployment + ":\n    pg_connection: postgres://x\n    repo: " + repoURL + "\n"
}

// TestDoctor_PGVersion_PG18_Supported: a manifest with
// PG_VERSION_NUM=180000 surfaces in pg_versions[] with
// supported=true and emits NO Notice issue.
func TestDoctor_PGVersion_PG18_Supported(t *testing.T) {
	w := newReadWorld(t)
	commitManifestWithPGVersion(t, w, "db1", 180000,
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	writeReadWorldConfig(t, w, pgVersionConfigYAML(w.repoURL, "db1"))

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"pg_versions"`,
		`"deployment": "db1"`,
		`"pg_major": 18`,
		`"supported": true`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor missing %q:\n%s", want, stdout)
		}
	}
	// No unsupported_major issue should be emitted.
	if strings.Contains(stdout, "pg.unsupported_major") {
		t.Errorf("PG 18 should NOT trigger pg.unsupported_major:\n%s", stdout)
	}
}

// TestDoctor_PGVersion_PG14_UnsupportedNotice: a manifest at
// PG 14 (below MinSupportedMajor=15) surfaces both an entry
// in pg_versions[] with supported=false AND a Notice issue
// `pg.unsupported_major`.
func TestDoctor_PGVersion_PG14_UnsupportedNotice(t *testing.T) {
	w := newReadWorld(t)
	commitManifestWithPGVersion(t, w, "db1", 140000,
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	writeReadWorldConfig(t, w, pgVersionConfigYAML(w.repoURL, "db1"))

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"pg_major": 14`,
		`"supported": false`,
		`pg.unsupported_major`,
		`"severity": "notice"`,
		"outside the tested support window",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor missing %q:\n%s", want, stdout)
		}
	}
}

// TestDoctor_PGVersion_NoBackups_Silent: a deployment with
// no backups is silent — no row in pg_versions[], no issue.
// (The "you have no backups" warning is repoCheckReport's
// territory.)
func TestDoctor_PGVersion_NoBackups_Silent(t *testing.T) {
	w := newReadWorld(t)
	writeReadWorldConfig(t, w, pgVersionConfigYAML(w.repoURL, "db1"))

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	// pg_versions key is omitempty — must NOT appear when
	// no deployment had a manifest to read.
	if strings.Contains(stdout, `"pg_versions"`) {
		t.Errorf("pg_versions should be omitted when no manifests; got:\n%s", stdout)
	}
}
