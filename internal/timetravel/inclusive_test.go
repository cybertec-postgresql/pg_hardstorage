package timetravel

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// Regression (M105): timetravel built its restore.Recovery without
// setting Inclusive, so the zero value (false) rendered
// recovery_target_inclusive = false — an exclusive stop, the opposite
// of PG's and the CLI's default — and an LSN equal to a backup's stop
// LSN (reachable inclusively) was refused as target_unreachable.
func TestCreate_LSNTargetIsInclusive(t *testing.T) {
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	store := backup.NewManifestStore(sp)

	ts := time.Now().UTC().Add(-time.Hour)
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.a", Deployment: "db1",
		Type: backup.BackupTypeFull, PGVersion: 17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/1000028", StopLSN: "0/5000000", Timeline: 1,
		StartedAt: ts.Add(-time.Minute), StoppedAt: ts,
		BackupLabel: "START WAL LOCATION: 0/1000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:       []backup.FileEntry{},
	}
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	// Archive segments 1..5 so the WAL pre-flights see a contiguous
	// archive covering the backup and the target.
	for seg := uint64(1); seg <= 5; seg++ {
		name := walsink.SegmentFileName(1, seg, walsink.SegmentSize)
		start := pglogrepl.LSN(seg * uint64(walsink.SegmentSize))
		sm := &walsink.SegmentManifest{
			Schema: walsink.Schema, Deployment: "db1",
			SystemIdentifier: "7000000000000000001", Timeline: 1,
			SegmentNumber: seg, SegmentName: name,
			StartLSN:    start.String(),
			EndLSN:      (start + pglogrepl.LSN(walsink.SegmentSize)).String(),
			SegmentSize: walsink.SegmentSize,
		}
		raw, merr := sm.MarshalToBytes()
		if merr != nil {
			t.Fatal(merr)
		}
		if _, perr := sp.Put(context.Background(), walsink.SegmentPath("db1", 1, name),
			bytes.NewReader(raw), storage.PutOptions{ContentLength: int64(len(raw))}); perr != nil {
			t.Fatal(perr)
		}
	}

	target := filepath.Join(t.TempDir(), "tt")
	mgr := NewManager(filepath.Join(t.TempDir(), "state.json"), "/usr/bin/true")
	_, err = mgr.Create(context.Background(), CreateOptions{
		Name: "s1", Deployment: "db1", RepoURL: repoURL,
		TargetDir: target, At: "0/5000000", Verifier: verifier,
	})
	if err != nil {
		t.Fatalf("Create with LSN == backup stop LSN: %v (an inclusive stop at the stop LSN is reachable)", err)
	}
	conf, rerr := os.ReadFile(filepath.Join(target, "postgresql.auto.conf"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(conf), "recovery_target_inclusive = true") {
		t.Errorf("postgresql.auto.conf lacks recovery_target_inclusive = true:\n%s", conf)
	}
}
