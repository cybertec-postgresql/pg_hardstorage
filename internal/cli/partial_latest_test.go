package cli

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestLatestBackupID_PicksByTimeNotBackupID pins that `partial inspect`'s
// "latest" resolution selects the newest backup by StoppedAt time, not
// the lexicographic max of BackupID. An ID is "<dep>.<type>.<ts>.<seq>"
// and the type segment sorts "full" < "incremental_lsn" < "snapshot"
// before the timestamp, so max(BackupID) would pick an older
// incremental over a newer full and inspect the wrong backup.
func TestLatestBackupID_PicksByTimeNotBackupID(t *testing.T) {
	ctx := context.Background()
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	_ = verifier

	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)

	mk := func(id string, typ backup.BackupType, stoppedAt time.Time) *backup.Manifest {
		return &backup.Manifest{
			Schema:           backup.Schema,
			BackupID:         id,
			Deployment:       "db1",
			Type:             typ,
			PGVersion:        170,
			SystemIdentifier: "7000000000000000001",
			StartLSN:         "0/3000028",
			StopLSN:          "0/30001A0",
			Timeline:         1,
			StartedAt:        stoppedAt.Add(-time.Minute),
			StoppedAt:        stoppedAt,
			BackupLabel:      "START WAL LOCATION: 0/3000028\n",
			Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			PGBackupManifest: []byte(`{"PostgreSQL-Backup-Manifest-Version":1}`),
		}
	}

	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	newerFull := mk("db1.full.20260430T120000Z.aaaa", backup.BackupTypeFull, t0.Add(48*time.Hour))
	olderInc := mk("db1.incremental_lsn.20260428T120000Z.bbbb", backup.BackupTypeIncremental, t0)
	olderInc.ParentBackupID = newerFull.BackupID
	for _, m := range []*backup.Manifest{newerFull, olderInc} {
		if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", m.BackupID, err)
		}
	}
	if !(olderInc.BackupID > newerFull.BackupID) {
		t.Fatalf("setup invalid: want %q > %q lexically", olderInc.BackupID, newerFull.BackupID)
	}

	// A bare &cobra.Command{} returns a nil Context(); cobra only
	// populates it during Execute. latestBackupID only needs the
	// context, so set it explicitly (production callers run under
	// RunE, where cobra has already set it).
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	got, err := latestBackupID(cmd, store, "db1")
	if err != nil {
		t.Fatalf("latestBackupID: %v", err)
	}
	if got != newerFull.BackupID {
		t.Errorf("latest = %q; want the newer full %q (by StoppedAt, not lexicographic BackupID)", got, newerFull.BackupID)
	}
}
