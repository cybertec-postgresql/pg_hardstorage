package repo_test

// `wal prune`'s keep-floor answers "is this segment newer than the
// operator's --keep-since cutoff?" from the segment manifest's
// created_at. When created_at is unknown — a legacy manifest without
// the field, or one whose producer renamed the tag — the branch used to
// be skipped entirely, so the floor silently protected nothing and the
// LSN rule deleted WAL the operator had explicitly asked to retain.
//
// gc already decided which way an unknown timestamp rounds, in the same
// repository, for the same kind of decision:
//
//	When ModTime is the zero value (backend doesn't expose it) we
//	conservatively treat the tombstone as YOUNG (still in grace) so
//	silent data loss is impossible.
//
// Two deletion paths must not disagree about that.
//
// The contract tests below additionally bind walprune's partial-decode
// shapes to the types that PRODUCE the manifests it reads. Those
// decoders are local anonymous structs (importing walsink or backup
// into internal/repo is a cycle), so nothing otherwise stops the
// producers' JSON tags from drifting away from them — and every field
// they read drives a delete-or-keep decision.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// plantWALSegNoCreatedAt writes a segment manifest with NO created_at
// field at all — the legacy / drifted shape.
func plantWALSegNoCreatedAt(t *testing.T, sp storage.StoragePlugin, deployment string, tli uint32, segName, endLSN string) {
	t.Helper()
	body := map[string]any{
		"end_lsn": endLSN,
		"chunks":  []map[string]any{{"hash": "", "len": int64(1024)}},
	}
	enc, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	key := walsink.SegmentPath(deployment, tli, segName)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(enc),
		storage.PutOptions{ContentLength: int64(len(enc))}); err != nil {
		t.Fatalf("plant wal manifest: %v", err)
	}
}

// The regression: an unknown age under an active keep-floor must not be
// deleted.
func TestWALPrune_UnknownCreatedAtIsKeptUnderAFloor(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	now := time.Now().UTC()
	plantBackupManifest(t, sp, "db1", "db1.full.aaa", "0/05000000", now.Add(-1*time.Hour))

	// Below the LSN frontier, so the primary rule would delete it —
	// but its age is unknown, so the floor cannot clear it for deletion.
	plantWALSegNoCreatedAt(t, sp, "db1", 1, "000000010000000000000001", "0/02000000")

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment:    "db1",
		KeepFloorTime: now.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.SegmentsDeleted != 0 {
		t.Errorf("deleted %d segment(s) whose age is unknown while a keep-floor is active — "+
			"--keep-since silently protects nothing when created_at is missing, and WAL the "+
			"operator asked to retain is gone", res.SegmentsDeleted)
	}
	if res.SegmentsKeptByUnknownAge != 1 {
		t.Errorf("SegmentsKeptByUnknownAge = %d, want 1 — the operator must be able to see WHY "+
			"prune stopped reclaiming space", res.SegmentsKeptByUnknownAge)
	}
	if !segExists(t, sp, "db1", 1, "000000010000000000000001") {
		t.Error("the unknown-age segment was deleted")
	}
}

// Without a floor there is nothing to be conservative about: the LSN
// rule stands alone and an unknown age must not start protecting
// segments, or prune would never reclaim anything on a legacy repo.
func TestWALPrune_UnknownCreatedAtIsNotProtectedWithoutAFloor(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	now := time.Now().UTC()
	plantBackupManifest(t, sp, "db1", "db1.full.aaa", "0/05000000", now.Add(-1*time.Hour))
	plantWALSegNoCreatedAt(t, sp, "db1", 1, "000000010000000000000001", "0/02000000")

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{Deployment: "db1"})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.SegmentsDeleted != 1 {
		t.Errorf("deleted %d, want 1 — with no keep-floor set, an unknown created_at must not "+
			"protect a segment the LSN rule cleared", res.SegmentsDeleted)
	}
	if res.SegmentsKeptByUnknownAge != 0 {
		t.Errorf("SegmentsKeptByUnknownAge = %d with no floor set", res.SegmentsKeptByUnknownAge)
	}
}

// A KNOWN old age must still be deleted, or the fix would turn the
// keep-floor into "keep everything".
func TestWALPrune_KnownOldAgeIsStillDeletedUnderAFloor(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	now := time.Now().UTC()
	plantBackupManifest(t, sp, "db1", "db1.full.aaa", "0/05000000", now.Add(-1*time.Hour))
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000001",
		"0/02000000", now.Add(-72*time.Hour), []int64{1024})

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment:    "db1",
		KeepFloorTime: now.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.SegmentsDeleted != 1 {
		t.Errorf("deleted %d, want 1 — a segment provably older than the floor must still go, "+
			"or --keep-since means \"keep everything\"", res.SegmentsDeleted)
	}
	if res.SegmentsKeptByUnknownAge != 0 {
		t.Errorf("SegmentsKeptByUnknownAge = %d for a manifest with a known timestamp",
			res.SegmentsKeptByUnknownAge)
	}
}

// Contract: walprune's walSegmentDecode must read a REAL
// walsink.SegmentManifest. Every field it takes drives a decision —
// end_lsn the delete/keep rule, created_at the floor.
func TestWALPrune_DecodesARealWALSinkManifest(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	now := time.Now().UTC()
	plantBackupManifest(t, sp, "db1", "db1.full.aaa", "0/05000000", now.Add(-1*time.Hour))

	segName := walsink.SegmentFileName(1, 1, walsink.SegmentSize)
	m := &walsink.SegmentManifest{
		Schema: walsink.Schema, Deployment: "db1", Timeline: 1,
		SegmentNumber: 1, SegmentName: segName,
		StartLSN: "0/1000000", EndLSN: "0/2000000",
		SegmentSize: walsink.SegmentSize,
		Chunks:      []walsink.ChunkRef{{Hash: repo.Hash{}, Offset: 0, Len: 4096}},
		// Old enough that a 24h floor must NOT save it: this asserts
		// created_at was actually read, not defaulted to zero (which
		// would now keep it).
		CreatedAt: now.Add(-72 * time.Hour),
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Put(context.Background(), walsink.SegmentPath("db1", 1, segName),
		bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment:    "db1",
		KeepFloorTime: now.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.SegmentsDeleted != 1 || res.SegmentsKeptByUnknownAge != 0 {
		t.Fatalf("deleted=%d keptByUnknownAge=%d, want 1 and 0 — walprune's decoder and "+
			"walsink.SegmentManifest have drifted apart; end_lsn or created_at is no longer "+
			"being read, and every prune decision rests on those two fields",
			res.SegmentsDeleted, res.SegmentsKeptByUnknownAge)
	}
	if res.BytesDeleted != 4096 {
		t.Errorf("BytesDeleted = %d, want 4096 — the chunk len field is not being read",
			res.BytesDeleted)
	}
}

// Contract: the frontier decoder must read a REAL backup.Manifest.
// start_lsn IS the prune floor — a drift that zeroed it would either
// abort every prune or, worse, move the floor.
func TestWALPrune_FrontierDecodesARealBackupManifest(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	now := time.Now().UTC()

	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.real", Deployment: "db1",
		Type: backup.BackupTypeFull, StartLSN: "0/05000000", StoppedAt: now.Add(-time.Hour),
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Put(context.Background(), backup.PrimaryPath("db1", "db1.full.real"),
		bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	// Below the real frontier → deletable. If start_lsn were not read,
	// the frontier would be wrong and this assertion moves.
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000001",
		"0/02000000", now.Add(-72*time.Hour), []int64{1024})
	// Above the frontier → must survive.
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000009",
		"0/09000000", now.Add(-72*time.Hour), []int64{1024})

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{Deployment: "db1"})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.FrontierBackupID != "db1.full.real" || res.FrontierLSN != "0/5000000" {
		t.Fatalf("frontier = %q @ %q, want db1.full.real @ 0/5000000 — the frontier decoder and "+
			"backup.Manifest have drifted apart, and the frontier IS the prune floor",
			res.FrontierBackupID, res.FrontierLSN)
	}
	if res.SegmentsDeleted != 1 {
		t.Errorf("deleted %d, want 1", res.SegmentsDeleted)
	}
	if !segExists(t, sp, "db1", 1, "000000010000000000000009") {
		t.Error("a segment above the frontier was deleted")
	}
}
