package cli

import (
	"bytes"
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func shredTestSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	root := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + root}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

func plantEncManifestAt(t *testing.T, sp storage.StoragePlugin, key, deployment, backupID, kekRef string) {
	t.Helper()
	m := &backup.Manifest{
		Schema:     backup.Schema,
		BackupID:   backupID,
		Deployment: deployment,
		Encryption: &backup.EncryptionInfo{Scheme: "aes-256-gcm", KEKRef: kekRef, WrappedDEK: "x", EnvelopeVersion: 1},
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
}

// TestScanAffectedBackups_IncludesStaleReplica pins the shred-scope fix:
// a backup whose PRIMARY was rotated off the KEK but whose REPLICA still
// holds it MUST be reported as affected by shredding that KEK — otherwise
// shred under-states its blast radius and strands the replica.
func TestScanAffectedBackups_IncludesStaleReplica(t *testing.T) {
	sp := shredTestSP(t)
	// Primary on the NEW kek, replica stranded on the OLD kek.
	plantEncManifestAt(t, sp, backup.PrimaryPath("db1", "b1"), "db1", "b1", "kek:new")
	plantEncManifestAt(t, sp, backup.ReplicaPath("b1"), "db1", "b1", "kek:old")

	affected, err := scanAffectedBackups(context.Background(), sp, "kek:old")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	found := false
	for _, id := range affected {
		if id == "b1" {
			found = true
		}
	}
	if !found {
		t.Errorf("scan for kek:old must report b1 (its replica still holds it); got %v", affected)
	}
	if len(affected) != 1 {
		t.Errorf("b1 must be reported exactly once (no double-count); got %v", affected)
	}

	// Scanning for the new kek finds the primary (once).
	if a, _ := scanAffectedBackups(context.Background(), sp, "kek:new"); len(a) != 1 || a[0] != "b1" {
		t.Errorf("scan for kek:new = %v, want [b1]", a)
	}
}
