package standby

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestResolveBackup_LatestPicksByTimeNotBackupID pins the fix for the
// standby latest-resolution bug: "latest" must select the most recent
// backup by the manifest's StoppedAt time, NOT the lexicographic max
// of BackupID. Because an ID is "<dep>.<type>.<ts>.<seq>" and the type
// segment sorts "full" < "incremental_lsn" < "snapshot" before the
// timestamp, the old lexicographic loop picked an OLDER incremental
// over a NEWER full, seeding the standby from the wrong backup.
func TestResolveBackup_LatestPicksByTimeNotBackupID(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)

	commit := func(m *backup.Manifest) {
		t.Helper()
		if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", m.BackupID, err)
		}
	}

	mk := func(id string, typ backup.BackupType, stoppedAt time.Time, pgVer int) *backup.Manifest {
		return &backup.Manifest{
			Schema:           backup.Schema,
			BackupID:         id,
			Deployment:       "db1",
			Type:             typ,
			PGVersion:        pgVer,
			SystemIdentifier: "7388123456789012345",
			StartLSN:         "0/3000028",
			StopLSN:          "0/30001A0",
			Timeline:         1,
			Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			StartedAt:        stoppedAt.Add(-time.Minute),
			StoppedAt:        stoppedAt,
			BackupLabel:      "START WAL LOCATION: 0/3000028\n",
			PGBackupManifest: []byte(`{"PostgreSQL-Backup-Manifest-Version":1}`),
		}
	}

	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	// Newer FULL — the true latest by time. PGVersion 170 to verify the
	// resolver also returns the right manifest's version.
	newerFull := mk("db1.full.20260430T120000Z.aaaa", backup.BackupTypeFull, t0.Add(48*time.Hour), 170)
	// Older INCREMENTAL — lexicographically GREATER (i > f) but two days
	// earlier. The buggy max(BackupID) would pick this one.
	olderInc := mk("db1.incremental_lsn.20260428T120000Z.bbbb", backup.BackupTypeIncremental, t0, 160)
	olderInc.ParentBackupID = newerFull.BackupID

	commit(newerFull)
	commit(olderInc)

	// Sanity: confirm the lexicographic hazard is real for these IDs.
	if !(olderInc.BackupID > newerFull.BackupID) {
		t.Fatalf("test setup invalid: expected %q > %q lexically", olderInc.BackupID, newerFull.BackupID)
	}

	m := NewManager(filepath.Join(t.TempDir(), "state.json"), "/bin/true")
	gotID, gotVer, err := m.resolveBackup(ctx, repoURL, "db1", "latest", verifier)
	if err != nil {
		t.Fatalf("resolveBackup: %v", err)
	}
	if gotID != newerFull.BackupID {
		t.Errorf("resolved latest = %q (pgVer %d); want the newer full %q — latest must be by StoppedAt time, not lexicographic BackupID",
			gotID, gotVer, newerFull.BackupID)
	}
	if gotVer != 170 {
		t.Errorf("pgVersion = %d; want 170 (the newer full's version)", gotVer)
	}
}
