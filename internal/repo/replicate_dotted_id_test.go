package repo_test

// replicate_dotted_id_test.go — Replicate must copy a backup whose ID (or a
// segment whose deployment) contains ".tmp." — validateStorageID permits
// dots. A full-key ".tmp." skip dropped such committed objects from the copy,
// so the DR replica silently omitted a backup: data loss on a DR failover.

import (
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestReplicate_CopiesBackupWithTmpInID(t *testing.T) {
	src, dst := twoRepos(t)
	ctx := context.Background()

	// Backup ID contains ".tmp." (valid). Chunk + manifest on src.
	h := putChunk(t, src, []byte("dotted-id-backup-payload"))
	putManifest(t, src, "db1", "db1.full.tmp.abc", []repo.Hash{h})

	// A WAL segment under a deployment whose NAME contains ".tmp.".
	hw := putChunk(t, src, []byte("dotted-dep-wal-payload"))
	putWALManifest(t, src, "dep.tmp.x", "00000001", "000000010000000000000009", []repo.Hash{hw})

	if _, err := repo.Replicate(ctx, src, dst, repo.ReplicateOptions{IncludeWAL: true}); err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	// The committed manifest and its chunk MUST have landed at dst.
	if !statExists(t, dst, "manifests/db1/backups/db1.full.tmp.abc/manifest.json") {
		t.Fatal("backup with '.tmp.' in ID was NOT replicated — DR replica silently omits it (data loss)")
	}
	if !statExists(t, dst, repo.ChunkKey(h)) {
		t.Fatal("chunk of the '.tmp.'-ID backup was NOT replicated (its manifest was skipped, so it looked unreferenced)")
	}
	if !statExists(t, dst, "wal/dep.tmp.x/00000001/000000010000000000000009.json") {
		t.Fatal("WAL segment under a '.tmp.' deployment was NOT replicated (DR replica can't PITR across it)")
	}
	if !statExists(t, dst, repo.ChunkKey(hw)) {
		t.Fatal("chunk of the '.tmp.'-deployment WAL segment was NOT replicated")
	}
}
