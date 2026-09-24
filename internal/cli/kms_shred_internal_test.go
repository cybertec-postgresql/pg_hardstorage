package cli

import (
	"bytes"
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
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

	scope, err := scanAffectedBackups(context.Background(), sp, "kek:old")
	affected := scope.IDs
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
	if s, _ := scanAffectedBackups(context.Background(), sp, "kek:new"); len(s.IDs) != 1 || s.IDs[0] != "b1" {
		t.Errorf("scan for kek:new = %v, want [b1]", s.IDs)
	}
}

// The shred target is the local kek.bin, which the keystore resolves for
// EVERY local:* ref. After a local rotation the manifests say
// "local:v2"; the scan matched only the literal "local:default", skipped
// tombstoned backups and ignored WAL, so dry-run and the audit record
// said "0 affected" for a repo the shred was about to make entirely
// unrecoverable.
func TestScanAffectedBackups_CountsEveryRefResolvingToTheLocalKEK(t *testing.T) {
	sp := shredTestSP(t)
	ctx := context.Background()
	plantEncManifestAt(t, sp, backup.PrimaryPath("db1", "rotated"), "db1", "rotated", "local:v2")
	plantEncManifestAt(t, sp, backup.PrimaryPath("db1", "legacy"), "db1", "legacy", "local:default")
	plantEncManifestAt(t, sp, backup.PrimaryPath("db1", "dead"), "db1", "dead", "local:v2")
	plantEncManifestAt(t, sp, backup.PrimaryPath("db1", "cloud"), "db1", "cloud", "aws-kms://alias/x")
	tomb := []byte(`{"reason":"test"}`)
	if _, err := sp.Put(ctx, backup.TombstonePath("db1", "dead"), bytes.NewReader(tomb),
		storage.PutOptions{ContentLength: int64(len(tomb))}); err != nil {
		t.Fatal(err)
	}
	for i, ref := range []string{"local:v2", "local:default", "aws-kms://alias/x"} {
		seg := &walsink.SegmentManifest{
			Schema: walsink.Schema, Deployment: "db1", Timeline: 1,
			SegmentName: walsink.SegmentFileName(1, uint64(i+1), walsink.SegmentSize),
			Encryption:  &walsink.EncryptionInfo{Scheme: "aes-256-gcm", KEKRef: ref, WrappedDEK: "x", EnvelopeVersion: 1},
		}
		raw, _ := seg.MarshalToBytes()
		if _, err := sp.Put(ctx, walsink.SegmentPath("db1", 1, seg.SegmentName), bytes.NewReader(raw),
			storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
			t.Fatal(err)
		}
	}

	scope, err := scanAffectedBackups(ctx, sp, keystore.KEKRefLocal)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, id := range scope.IDs {
		got[id] = true
	}
	if len(scope.IDs) != 3 || !got["rotated"] || !got["legacy"] || !got["dead"] {
		t.Errorf("affected = %v, want rotated+legacy+dead (not the cloud-KMS backup)", scope.IDs)
	}
	if scope.WALSegments != 2 {
		t.Errorf("affected WAL segments = %d, want 2", scope.WALSegments)
	}
}
