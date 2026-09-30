package recovery_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testfixture"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/verify/sandbox"
)

func (w *drillWorld) commitDrillBackupWithPGManifest(t *testing.T, tsPath string) {
	t.Helper()
	info, err := casdefault.New(w.sp).PutChunk(context.Background(), []byte("17\n"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Hour)
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.pgm", Deployment: "db1", Tenant: "default",
		Type: backup.BackupTypeFull, PGVersion: 17, SystemIdentifier: "7000000000000000001",
		StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
		StartedAt: now.Add(-time.Minute), StoppedAt: now,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      drillTablespaces([]string{tsPath}),
		PGBackupManifest: []byte(`{"PostgreSQL-Backup-Manifest-Version":1,"Files":[]}`),
		Files: []backup.FileEntry{{Path: "PG_VERSION", Size: 3, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: 3}}}},
	}
	testfixture.PlantArchivedWAL(t, w.sp, "db1", 1)
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Regression (M100 + M101):
//   - the Docker sandbox bind-mounted only the data dir, so the
//     restored pg_tblspc/<oid> symlinks (absolute, pointing at the
//     mapped scratch dirs) dangled inside the container and every drill
//     of a backup with a tablespace failed; the scratch tablespace dirs
//     were also never removed after the drill.
//   - ManifestCaptured was not passed, so a sandbox that could not see
//     the files was classified as "skipped" rather than failed.
func TestDrill_TablespaceScratchIsMountedAndCleanedUp(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackupWithPGManifest(t, "/srv/live/ts_fast")
	scratch := filepath.Join(t.TempDir(), "drill-scratch", "ts_fast")
	remap, err := restore.ParseTablespaceRemap([]string{"/srv/live/ts_fast=" + scratch})
	if err != nil {
		t.Fatal(err)
	}

	var seen sandbox.Options
	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{Verifier: w.verifier, TablespaceRemap: remap},
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			// What the restore does: fill the mapped tablespace dir.
			if err := os.MkdirAll(filepath.Join(scratch, "PG_17_x"), 0o700); err != nil {
				return nil, err
			}
			return &restore.Result{}, os.WriteFile(filepath.Join(scratch, "PG_17_x", "1259"), []byte("x"), 0o600)
		},
		func(ctx context.Context, opts sandbox.Options) (*sandbox.Result, error) {
			seen = opts
			return &sandbox.Result{Passed: true}, nil
		},
	))
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if r.Verdict != recovery.DrillVerdictPass {
		t.Errorf("verdict = %s, want pass", r.Verdict)
	}
	found := false
	for _, b := range seen.ExtraBinds {
		if b == scratch {
			found = true
		}
	}
	if !found {
		t.Errorf("sandbox ExtraBinds = %v, want the mapped tablespace dir %s mounted at its own path", seen.ExtraBinds, scratch)
	}
	if !seen.ManifestCaptured {
		t.Error("ManifestCaptured not set for a backup that carries backup_manifest")
	}
	if _, err := os.Stat(scratch); err == nil {
		t.Errorf("drill left its scratch tablespace dir %s behind", scratch)
	}
}
