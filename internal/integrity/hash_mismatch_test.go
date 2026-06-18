package integrity_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/integrity"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestExecute_DetectsHashMismatch is the core bit-rot-detection
// guarantee: a chunk whose stored bytes no longer hash to its
// content-address must surface as a hash_mismatch (not pass silently,
// not crash). The existing DetectsMissingChunk covers a *deleted*
// chunk; this covers a *corrupted* one. We simulate corruption by
// overwriting chunk A's object with chunk B's valid envelope — so A's
// key resolves, decompresses cleanly, but the plaintext hashes to B,
// not A. Requires a CAS (content verification) in the engine.
func TestExecute_DetectsHashMismatch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	m := f.commitBackup(t, "db1", "x", 3)

	hA := m.Files[0].Chunks[0].Hash
	hB := m.Files[0].Chunks[1].Hash
	if hA == hB {
		t.Fatal("test needs two distinct chunks")
	}

	// Read chunk B's on-disk envelope and write it over chunk A's key.
	rc, err := f.sp.Get(ctx, repo.ChunkKey(hB))
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	bBytes, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if err := f.sp.Delete(ctx, repo.ChunkKey(hA)); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	if _, err := f.sp.Put(ctx, repo.ChunkKey(hA), bytes.NewReader(bBytes),
		storage.PutOptions{ContentLength: int64(len(bBytes))}); err != nil {
		t.Fatalf("overwrite A: %v", err)
	}

	eng := integrity.NewEngine(integrity.EngineOptions{
		Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier, CAS: f.cas,
	})
	r, err := eng.Execute(ctx, "", integrity.Strategy{Mode: "content-full"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if r.Status != integrity.StatusFoundIssues {
		t.Errorf("Status = %s, want found_issues", r.Status)
	}
	if r.Chunks.Mismatched != 1 {
		t.Errorf("Mismatched = %d, want 1", r.Chunks.Mismatched)
	}
	// The two intact chunks (B and the third) still verify.
	if r.Chunks.Verified != 2 {
		t.Errorf("Verified = %d, want 2", r.Chunks.Verified)
	}
	var found bool
	for _, fl := range r.Chunks.Failures {
		if fl.ChunkHash == hA.String() {
			found = true
			if fl.Reason != "hash_mismatch" {
				t.Errorf("Reason = %q, want hash_mismatch", fl.Reason)
			}
		}
	}
	if !found {
		t.Errorf("no hash_mismatch failure recorded for corrupted chunk %s; failures=%+v",
			hA.String(), r.Chunks.Failures)
	}
}
