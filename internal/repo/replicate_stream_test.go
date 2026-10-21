package repo_test

import (
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

const streamedKey = "manifests/db1/backups/db1.full.20260430T0900Z/manifest.json"

func TestReplicate_Streamed_HappyIdempotentDryRun(t *testing.T) {
	defer repo.SetReplicateStreamThresholdForTest(1)()
	src, dst := twoRepos(t)
	c1 := putChunk(t, src, []byte("one"))
	c2 := putChunk(t, src, []byte("two"))
	// c1 repeated: de-duplication must copy it once.
	putManifest(t, src, "db1", "db1.full.20260430T0900Z", []repo.Hash{c1, c2, c1})

	dry, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{DryRun: true})
	if err != nil || dry.ManifestsCopied != 1 || dry.ChunksCopied != 2 {
		t.Fatalf("dry run: %+v err=%v", dry, err)
	}
	if statExists(t, dst, streamedKey) || statExists(t, dst, repo.ChunkKey(c1)) {
		t.Fatal("dry run wrote to dst")
	}

	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestsCopied != 1 || res.ChunksCopied != 2 || res.ManifestsFailed != 0 || res.ChunksFailed != 0 {
		t.Fatalf("first run: %+v", res)
	}
	for _, k := range []string{streamedKey, repo.ChunkKey(c1), repo.ChunkKey(c2)} {
		if !statExists(t, dst, k) {
			t.Fatalf("%s missing at dst", k)
		}
	}
	res, err = repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestsCopied != 0 || res.ManifestsSkipped != 1 || res.ManifestsRefreshed != 0 {
		t.Fatalf("second run should skip: %+v", res)
	}
}

func TestReplicate_Streamed_MissingChunkWithholdsManifest(t *testing.T) {
	defer repo.SetReplicateStreamThresholdForTest(1)()
	src, dst := twoRepos(t)
	c1 := putChunk(t, src, []byte("present"))
	ghost := repo.HashOf([]byte("never stored"))
	putManifest(t, src, "db1", "db1.full.20260430T0900Z", []repo.Hash{c1, ghost})

	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestsFailed != 1 || res.ChunksMissing != 1 {
		t.Fatalf("want 1 failed manifest + 1 missing chunk: %+v", res)
	}
	if statExists(t, dst, streamedKey) {
		t.Fatal("manifest committed over a missing chunk")
	}
}

func TestReplicate_Streamed_RewrittenSourceIsRefreshed(t *testing.T) {
	defer repo.SetReplicateStreamThresholdForTest(1)()
	src, dst := twoRepos(t)
	c1 := putChunk(t, src, []byte("a"))
	putManifest(t, src, "db1", "db1.full.20260430T0900Z", []repo.Hash{c1})
	if _, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Source rewritten in place (kms rotate); same chunk set, different bytes.
	if err := src.Delete(context.Background(), streamedKey); err != nil {
		t.Fatal(err)
	}
	putManifest(t, src, "db1", "db1.full.20260430T0900Z", []repo.Hash{c1, c1})
	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestsRefreshed != 1 || res.ManifestsFailed != 0 {
		t.Fatalf("want refresh: %+v", res)
	}
}
