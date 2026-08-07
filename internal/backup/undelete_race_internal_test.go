package backup

// undelete_race_internal_test.go — undelete racing gc's sweep.
//
// Undelete's pre-flight runs while the manifest is still HIDDEN, and a
// concurrent `repo gc --apply` sweep works from a reference snapshot
// taken before the undelete began — so a pre-flight pass guarantees
// nothing about the seconds that follow. The failure it left open:
// pre-flight passes, gc's loop deletes the chunk, the marker is
// removed, undelete reports restored=true, and the operator holds a
// backup that lists as live and cannot restore. Same TOCTOU family as
// the adopted-chunk commit gate (c31688b): the check belongs at the
// VISIBILITY point.
//
// Internal package test: the seam (undeleteTestHookAfterUnmark) is
// unexported on purpose — it stages a concurrent sweep at the exact
// window, which no external composition can do deterministically.

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func raceWorld(t *testing.T) (*ManifestStore, storage.StoragePlugin, *Signer, *Verifier) {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, pub, err := GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := LoadVerifier(pub)
	if err != nil {
		t.Fatal(err)
	}
	return NewManifestStore(sp), sp, signer, verifier
}

func commitRaceManifest(t *testing.T, store *ManifestStore, sp storage.StoragePlugin, signer *Signer, id string, body []byte) *Manifest {
	t.Helper()
	m := &Manifest{
		Schema:           Schema,
		BackupID:         id,
		Deployment:       "db1",
		Tenant:           "default",
		Type:             BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        time.Now().UTC().Add(-time.Hour),
		StoppedAt:        time.Now().UTC().Add(-time.Hour + time.Minute),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []FileEntry{{Path: "PG_VERSION", Size: int64(len(body)), Mode: 0o600,
			Chunks: []ChunkRef{{Hash: repo.HashOf(body), Offset: 0, Len: int64(len(body))}}}},
	}
	if _, err := repo.NewCAS(sp).PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), m, signer, CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestUndelete_SweptDuringUndelete_RetombstonesAndRefuses is the race.
func TestUndelete_SweptDuringUndelete_RetombstonesAndRefuses(t *testing.T) {
	store, sp, signer, _ := raceWorld(t)
	body := []byte("undelete-race-chunk")
	m := commitRaceManifest(t, store, sp, signer, "db1.full.R", body)

	if err := store.SoftDelete(context.Background(), "db1", m.BackupID, "manual", "mistake"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	markerBefore, err := store.readRaw(context.Background(), TombstonePath("db1", m.BackupID))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}

	// gc's sweep lands in the exact window: after the marker flip,
	// before the post-flip re-verification.
	fired := false
	undeleteTestHookAfterUnmark = func() {
		fired = true
		if derr := sp.Delete(context.Background(), repo.ChunkKey(repo.HashOf(body))); derr != nil {
			t.Errorf("staged sweep: %v", derr)
		}
	}
	t.Cleanup(func() { undeleteTestHookAfterUnmark = nil })

	restored, uerr := store.Undelete(context.Background(), "db1", m.BackupID)
	if !fired {
		t.Fatal("the seam never fired; the sweep was not staged and this test proves nothing")
	}
	if uerr == nil || restored {
		t.Fatalf("Undelete returned (restored=%v, err=%v) with the chunk swept in the "+
			"window.\n\nThe operator would hold a backup that lists as live and cannot "+
			"restore — the silent-phantom outcome this re-verification exists to prevent.",
			restored, uerr)
	}
	var cm *UndeleteChunksMissingError
	if !errors.As(uerr, &cm) {
		t.Fatalf("error is %T (%v), want UndeleteChunksMissingError — the CLI maps that "+
			"to conflict.chunks_missing, the same refusal the pre-flight gives", uerr, uerr)
	}

	// The manifest must be BACK in its prior state: tombstoned, with
	// the original marker bytes (policy, reason, timestamps intact).
	dead, derr := store.IsTombstoned(context.Background(), "db1", m.BackupID)
	if derr != nil || !dead {
		t.Fatalf("backup is not tombstoned after the refused undelete (dead=%v err=%v) — "+
			"the marker was not restored, leaving a half-resurrected phantom", dead, derr)
	}
	markerAfter, err := store.readRaw(context.Background(), TombstonePath("db1", m.BackupID))
	if err != nil {
		t.Fatalf("re-read marker: %v", err)
	}
	if !bytes.Equal(markerBefore, markerAfter) {
		t.Errorf("marker bytes changed across the round trip:\nbefore: %s\nafter:  %s\n\n"+
			"policy/reason/timestamps are audit context; the restore must be exact",
			markerBefore, markerAfter)
	}
}

// TestUndelete_CleanWindow_StaysRestored: with the seam armed but
// sweeping nothing, the undelete must succeed — the re-verification
// runs on every real undelete and a false refusal breaks recovery.
func TestUndelete_CleanWindow_StaysRestored(t *testing.T) {
	store, sp, signer, verifier := raceWorld(t)
	body := []byte("clean-window-chunk")
	m := commitRaceManifest(t, store, sp, signer, "db1.full.S", body)
	if err := store.SoftDelete(context.Background(), "db1", m.BackupID, "manual", "x"); err != nil {
		t.Fatal(err)
	}
	fired := false
	undeleteTestHookAfterUnmark = func() { fired = true }
	t.Cleanup(func() { undeleteTestHookAfterUnmark = nil })

	restored, err := store.Undelete(context.Background(), "db1", m.BackupID)
	if err != nil || !restored {
		t.Fatalf("clean undelete failed: restored=%v err=%v", restored, err)
	}
	if !fired {
		t.Fatal("seam never fired")
	}
	if _, err := store.Read(context.Background(), "db1", m.BackupID, verifier); err != nil {
		t.Errorf("resurrected manifest unreadable: %v", err)
	}
}
