package repo_test

// `repo replicate --verify` decides whether a destination faithfully
// mirrors a source. Its chunk half rests on parseChunkHashes, a local
// anonymous decode of `files[].chunks[].hash` that exists because
// importing internal/backup into internal/repo is a cycle. The source
// comment asserts the coupling and nothing enforced it:
//
//	The fields we need (`files[].chunks[].hash`) are stable per the
//	v1 contract.
//
// If backup.Manifest's tags drift, json.Unmarshal still SUCCEEDS,
// parseChunkHashes returns zero hashes, and the verify loop never runs:
// ChunksConsidered stays 0, ChunksMissing stays 0, and AnyMissing() —
// which is ManifestsMissing + ChunksMissing + ... > 0 — is false. The
// command reports the replica consistent having examined none of its
// chunks.
//
// A DR verification that certifies a replica it never looked at is
// worse than no verification: the operator stops checking.
//
// There is already a FuzzParseChunkHashes, but a fuzzer proves the
// parser does not crash, not that it reads the fields the producer
// writes.

import (
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestVerifyReplicate_CountsChunksFromARealBackupManifest(t *testing.T) {
	w := setupRVWorld(t)
	w.commitToBoth(t, "db1", 1, []byte("chunk-payload-one"), true)

	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP,
		repo.ReplicateVerifyOptions{SampleRate: 1})
	if err != nil {
		t.Fatalf("VerifyReplicate: %v", err)
	}
	if r.ManifestsConsidered == 0 {
		t.Fatal("no manifests considered; the fixture did not plant one")
	}
	if r.ChunksConsidered == 0 {
		t.Fatalf("verified a replica with ChunksConsidered=0 while its manifest references "+
			"chunks — parseChunkHashes and backup.Manifest have drifted apart, so `repo "+
			"replicate --verify` reports %q having examined no chunk at all", r.Verdict)
	}
	if r.Verdict != repo.VerdictConsistent {
		t.Errorf("Verdict = %q, want consistent for a faithful replica", r.Verdict)
	}
}

// The other half of the contract, and the reason it needs care: with
// the chunk genuinely absent at the destination, the same decode must
// produce a finding. Replicate first, then DELETE the chunk from the
// destination — so the manifest IS present there and ManifestsMissing
// cannot supply the finding on its own.
//
// The obvious version of this test (replicate=false, leaving both the
// manifest and the chunk absent) passes even when parseChunkHashes
// returns nothing, because ManifestsMissing alone trips AnyMissing.
// Verified: under a renamed ChunkRef tag that version stayed green.
func TestVerifyReplicate_MissingChunkIsFoundViaTheRealManifest(t *testing.T) {
	w := setupRVWorld(t)
	w.commitToBoth(t, "db1", 1, []byte("chunk-payload-deleted-at-dst"), true)

	// Remove every chunk from the destination, leaving its manifests.
	removed := 0
	for info, err := range w.dstSP.List(context.Background(), "chunks/") {
		if err != nil {
			t.Fatal(err)
		}
		if err := w.dstSP.Delete(context.Background(), info.Key); err != nil {
			t.Fatal(err)
		}
		removed++
	}
	if removed == 0 {
		t.Fatal("fixture planted no chunks at the destination")
	}

	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP,
		repo.ReplicateVerifyOptions{SampleRate: 1})
	if err != nil {
		t.Fatalf("VerifyReplicate: %v", err)
	}
	if r.ManifestsMissing != 0 {
		t.Fatalf("fixture removed a manifest as well (%d missing); this test must isolate the "+
			"CHUNK path", r.ManifestsMissing)
	}
	if r.ChunksMissing == 0 {
		t.Fatalf("every chunk was deleted from the destination and verify found none missing "+
			"(considered %d, verdict %q) — the decode produced nothing to check, so the "+
			"replica is certified against a manifest whose chunks were never looked at",
			r.ChunksConsidered, r.Verdict)
	}
	if !r.AnyMissing() {
		t.Error("AnyMissing() is false despite missing chunks")
	}
}
