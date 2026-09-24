package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/faultinject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func checkWalkWorld(t *testing.T, n int) (storage.StoragePlugin, *backup.Verifier, *backup.Signer, []string) {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
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
	var keys []string
	for i := 0; i < n; i++ {
		body := []byte{byte(i), 'x'}
		info, err := casdefault.New(sp).PutChunk(context.Background(), body)
		if err != nil {
			t.Fatal(err)
		}
		ts := time.Date(2026, 5, 1, 12, i, 0, 0, time.UTC)
		id := "db1.full." + ts.Format("20060102T150405Z")
		m := &backup.Manifest{
			Schema: backup.Schema, BackupID: id, Deployment: "db1", Tenant: "default",
			Type: backup.BackupTypeFull, PGVersion: 17, SystemIdentifier: "7000000000000000001",
			StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
			StartedAt: ts, StoppedAt: ts.Add(time.Minute),
			BackupLabel: "START WAL LOCATION: 0/3000028\n",
			Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Files: []backup.FileEntry{{Path: "data", Size: int64(len(body)), Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: info.Hash, Len: int64(len(body))}}}},
		}
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, "manifests/db1/backups/"+id+"/manifest.json")
	}
	return sp, verifier, signer, keys
}

// M8: a transient backend error reading one manifest (an S3 500) was
// classified as a signature failure — exit 9 "potential tampering".
// Only bytes that were fetched and fail verification are signature
// failures; an unreadable manifest is counted as such and the walk
// continues over the rest.
func TestCheckDeploymentManifests_TransientGetIsNotTampering(t *testing.T) {
	sp, verifier, _, keys := checkWalkWorld(t, 3)
	fi := faultinject.New(sp)
	fi.Activate([]faultinject.Rule{{
		Name: "s3-500", Ops: faultinject.OpGet, KeyPrefix: keys[0],
		Err: errors.New("InternalError: We encountered an internal error. Please try again. (status 500)"),
	}}, faultinject.ActivateOptions{})
	defer fi.Deactivate()

	w, err := checkDeploymentManifests(context.Background(), fi, "db1", verifier)
	if err != nil {
		t.Fatalf("a GET failure on one manifest aborted the walk: %v", err)
	}
	if w.sigFailed != 0 {
		t.Errorf("sigFailed = %d; a storage 500 was reported as tampering", w.sigFailed)
	}
	if w.unreadable != 1 || w.live != 2 {
		t.Errorf("unreadable=%d live=%d; want 1/2 (the walk must continue past it)", w.unreadable, w.live)
	}
}

// A manifest whose bytes WERE read and do not verify is still a
// signature failure.
func TestCheckDeploymentManifests_ForgedIsSignatureFailure(t *testing.T) {
	sp, _, _, _ := checkWalkWorld(t, 2)
	_, otherPub, _ := backup.GenerateKeypair(rand.Reader)
	otherVerifier, _ := backup.LoadVerifier(otherPub)
	w, err := checkDeploymentManifests(context.Background(), sp, "db1", otherVerifier)
	if err != nil {
		t.Fatal(err)
	}
	if w.sigFailed != 2 || w.unreadable != 0 {
		t.Errorf("sigFailed=%d unreadable=%d; want 2/0", w.sigFailed, w.unreadable)
	}
}
