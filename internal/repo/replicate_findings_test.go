package repo_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// Tombstones are scoped to deployment + id. A tombstone for db1/X must
// neither withhold nor excuse db2's LIVE backup that happens to carry id X.
func TestReplicate_TombstoneScopedToDeployment(t *testing.T) {
	ctx := context.Background()
	src, dst := twoRepos(t)
	c1 := putChunk(t, src, []byte("db1 data"))
	c2 := putChunk(t, src, []byte("db2 data"))
	putManifest(t, src, "db1", "shared-id", []repo.Hash{c1})
	putManifest(t, src, "db2", "shared-id", []repo.Hash{c2})
	putRaw(t, src, "manifests/db1/backups/shared-id/manifest.json.tombstone", []byte("{}"))

	res, err := repo.Replicate(ctx, src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestsTombstoned != 1 || res.ManifestsCopied != 1 {
		t.Fatalf("tombstoned=%d copied=%d; want 1/1", res.ManifestsTombstoned, res.ManifestsCopied)
	}
	if !statExists(t, dst, "manifests/db2/backups/shared-id/manifest.json") {
		t.Fatal("db2's live backup was withheld from the replica by db1's tombstone")
	}

	// Verify must check db2's backup, not excuse it. Drop its chunk at dst.
	if err := dst.Delete(ctx, repo.ChunkKey(c2)); err != nil {
		t.Fatal(err)
	}
	vr, err := repo.VerifyReplicate(ctx, src, dst, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if vr.ChunksMissing != 1 || vr.Verdict != repo.VerdictBroken {
		t.Fatalf("verify: chunks_missing=%d verdict=%s; want 1/broken (db2's backup was excused by db1's tombstone)", vr.ChunksMissing, vr.Verdict)
	}
}

// A manifest naming an unparseable chunk hash must not be replicated
// with that reference silently dropped (a replica backup that claims to
// be restorable and is not).
func TestReplicate_UnparseableHashFailsClosed(t *testing.T) {
	ctx := context.Background()
	src, dst := twoRepos(t)
	good := putChunk(t, src, []byte("good"))
	body := `{"files":[{"chunks":[{"hash":"` + good.String() + `"},{"hash":"zz-not-a-hash"}]}]}`
	putRaw(t, src, "manifests/db1/backups/b1/manifest.json", []byte(body))

	res, err := repo.Replicate(ctx, src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestsFailed != 1 || res.ManifestsCopied != 0 {
		t.Fatalf("failed=%d copied=%d; want 1/0", res.ManifestsFailed, res.ManifestsCopied)
	}
	if statExists(t, dst, "manifests/db1/backups/b1/manifest.json") {
		t.Fatal("a manifest with an unparseable chunk reference reached the replica")
	}
}

// M89: a manifest rewritten in place at src (kms rotate re-wraps the DEK,
// same size) must be refreshed at the replica, and verify must see the
// difference even when the sizes match.
func TestReplicate_RefreshesRewrittenManifest(t *testing.T) {
	ctx := context.Background()
	src, dst := twoRepos(t)
	c := putChunk(t, src, []byte("payload"))
	key := "manifests/db1/backups/b1/manifest.json"
	v1 := `{"wrapped_dek":"AAAA","files":[{"chunks":[{"hash":"` + c.String() + `"}]}]}`
	v2 := `{"wrapped_dek":"BBBB","files":[{"chunks":[{"hash":"` + c.String() + `"}]}]}`
	putRaw(t, src, key, []byte(v1))
	if _, err := repo.Replicate(ctx, src, dst, repo.ReplicateOptions{}); err != nil {
		t.Fatal(err)
	}
	putRaw(t, src, key, []byte(v2)) // KEK rotation, same length

	vr, err := repo.VerifyReplicate(ctx, src, dst, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if vr.ManifestsContentDrift != 1 || vr.Verdict == repo.VerdictConsistent {
		t.Fatalf("verify called a stale (same-size) manifest %s; drift=%d", vr.Verdict, vr.ManifestsContentDrift)
	}

	res, err := repo.Replicate(ctx, src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestsRefreshed != 1 || res.ManifestsSkipped != 0 {
		t.Fatalf("refreshed=%d skipped=%d; want 1/0", res.ManifestsRefreshed, res.ManifestsSkipped)
	}
	got := readBody(t, dst, key)
	if got != v2 {
		t.Fatalf("replica still holds %s", got)
	}
	vr, err = repo.VerifyReplicate(ctx, src, dst, repo.ReplicateVerifyOptions{})
	if err != nil || vr.Verdict != repo.VerdictConsistent {
		t.Fatalf("after refresh: %v %v", vr.Verdict, err)
	}
	// Identical bytes remain a plain skip.
	res, err = repo.Replicate(ctx, src, dst, repo.ReplicateOptions{})
	if err != nil || res.ManifestsSkipped != 1 || res.ManifestsRefreshed != 0 {
		t.Fatalf("rerun: skipped=%d refreshed=%d err=%v", res.ManifestsSkipped, res.ManifestsRefreshed, err)
	}
}

// A refresh the replica refuses (WORM) is reported, not counted as skipped.
func TestReplicate_RefreshBlockedIsReported(t *testing.T) {
	ctx := context.Background()
	src, dst := twoRepos(t)
	c := putChunk(t, src, []byte("payload"))
	key := "manifests/db1/backups/b1/manifest.json"
	putRaw(t, src, key, []byte(`{"v":1,"files":[{"chunks":[{"hash":"`+c.String()+`"}]}]}`))
	if _, err := repo.Replicate(ctx, src, dst, repo.ReplicateOptions{}); err != nil {
		t.Fatal(err)
	}
	putRaw(t, src, key, []byte(`{"v":2,"files":[{"chunks":[{"hash":"`+c.String()+`"}]}]}`))
	res, err := repo.Replicate(ctx, src, &lockedDst{StoragePlugin: dst}, repo.ReplicateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestsFailed != 1 || res.ManifestsSkipped != 0 {
		t.Fatalf("failed=%d skipped=%d; want the blocked refresh reported as a failure", res.ManifestsFailed, res.ManifestsSkipped)
	}
	if len(res.Failures) == 0 || !strings.Contains(res.Failures[0].Err, "STALE") {
		t.Fatalf("failure detail should name the stale replica copy: %+v", res.Failures)
	}
}

// lockedDst refuses any non-conditional overwrite of manifests.
type lockedDst struct{ storage.StoragePlugin }

func (l *lockedDst) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if !opts.IfNotExists && strings.HasPrefix(key, "manifests/") {
		return storage.PutResult{}, errors.New("object is WORM-locked")
	}
	return l.StoragePlugin.Put(ctx, key, r, opts)
}

// M90: verify must check the chunks of WAL segments, count manifests it
// could not parse, and attribute WAL manifest drift to WAL.
func TestVerifyReplicate_WALChunksAndUnparseable(t *testing.T) {
	ctx := context.Background()
	src, dst := twoRepos(t)
	wc := putChunk(t, src, []byte("wal bytes"))
	putWALManifest(t, src, "db1", "00000001", "000000010000000000000001", []repo.Hash{wc})
	if _, err := repo.Replicate(ctx, src, dst, repo.ReplicateOptions{IncludeWAL: true}); err != nil {
		t.Fatal(err)
	}
	if err := dst.Delete(ctx, repo.ChunkKey(wc)); err != nil {
		t.Fatal(err)
	}
	vr, err := repo.VerifyReplicate(ctx, src, dst, repo.ReplicateVerifyOptions{IncludeWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	if vr.ChunksMissing != 1 || vr.Verdict != repo.VerdictBroken {
		t.Fatalf("WAL chunk missing at replica: chunks_missing=%d verdict=%s; want 1/broken", vr.ChunksMissing, vr.Verdict)
	}

	// Unparseable src manifest (present at both ends): not consistent.
	src2, dst2 := twoRepos(t)
	bad := []byte(`{"files":"not-a-list"}`)
	putRaw(t, src2, "manifests/db1/backups/b1/manifest.json", bad)
	putRaw(t, dst2, "manifests/db1/backups/b1/manifest.json", bad)
	vr, err = repo.VerifyReplicate(ctx, src2, dst2, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if vr.ManifestsUnparseable != 1 || vr.Verdict == repo.VerdictConsistent {
		t.Fatalf("unparseable=%d verdict=%s; its chunks were never checked", vr.ManifestsUnparseable, vr.Verdict)
	}

	// WAL segment manifest drift counts as WAL drift, not chunk drift.
	src3, dst3 := twoRepos(t)
	c3 := putChunk(t, src3, []byte("w"))
	putWALManifest(t, src3, "db1", "00000001", "000000010000000000000002", []repo.Hash{c3})
	if _, err := repo.Replicate(ctx, src3, dst3, repo.ReplicateOptions{IncludeWAL: true}); err != nil {
		t.Fatal(err)
	}
	putRaw(t, dst3, "wal/db1/00000001/000000010000000000000002.json",
		bytes.Replace(readBodyBytes(t, dst3, "wal/db1/00000001/000000010000000000000002.json"), []byte("chunks"), []byte("chunkz"), 1))
	vr, err = repo.VerifyReplicate(ctx, src3, dst3, repo.ReplicateVerifyOptions{IncludeWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	if vr.WALManifestsContentDrift != 1 || vr.ChunksContentDrift != 0 {
		t.Fatalf("wal drift=%d chunk drift=%d; want 1/0", vr.WALManifestsContentDrift, vr.ChunksContentDrift)
	}
}

// H44 (replica side): a chunk already at a WORM destination gets this
// run's retention deadline, not whatever an earlier run left on it.
func TestReplicate_ExtendsRetentionOfExistingDstChunk(t *testing.T) {
	ctx := context.Background()
	src, dst := twoRepos(t)
	rec := &retentionRecorder{StoragePlugin: dst}
	c := putChunk(t, src, []byte("shared"))
	putRaw(t, dst, repo.ChunkKey(c), []byte("shared")) // already replicated earlier
	putManifest(t, src, "db1", "b1", []repo.Hash{c})

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	policy := &repo.WORMPolicy{Mode: "compliance", RetentionSeconds: 86400 * 365}
	res, err := repo.Replicate(ctx, src, rec, repo.ReplicateOptions{DstWORM: policy, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if res.ChunksSkipped != 1 || res.ManifestsCopied != 1 {
		t.Fatalf("skipped=%d copied=%d", res.ChunksSkipped, res.ManifestsCopied)
	}
	got := rec.calls(repo.ChunkKey(c))
	if len(got) != 1 || !got[0].Equal(policy.RetainUntil(now)) {
		t.Fatalf("SetRetention on the existing dst chunk = %v; want one call to %v", got, policy.RetainUntil(now))
	}
}

func readBodyBytes(t *testing.T, sp storage.StoragePlugin, key string) []byte {
	t.Helper()
	rc, err := sp.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func readBody(t *testing.T, sp storage.StoragePlugin, key string) string {
	return string(readBodyBytes(t, sp, key))
}
