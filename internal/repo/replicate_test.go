package repo_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// twoRepos sets up a {src, dst} pair and returns their plugins. Both
// have HSREPO already.
func twoRepos(t *testing.T) (storage.StoragePlugin, storage.StoragePlugin) {
	t.Helper()
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + srcRoot}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + dstRoot}); err != nil {
		t.Fatal(err)
	}
	src := &fs.Plugin{}
	if err := src.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: srcRoot}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	dst := &fs.Plugin{}
	if err := dst.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: dstRoot}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close() })
	return src, dst
}

// putRaw is a small helper for planting bytes at a key.
func putRaw(t *testing.T, sp storage.StoragePlugin, key string, body []byte) {
	t.Helper()
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// putChunk plants a "chunk" at chunks/sha256/<canonical-key> whose
// hash matches the bytes — this is what every realistic repo's chunks
// look like (the layout is hash-keyed even when the body itself is
// just bytes for a test).
func putChunk(t *testing.T, sp storage.StoragePlugin, body []byte) repo.Hash {
	t.Helper()
	h := repo.Hash(sha256.Sum256(body))
	putRaw(t, sp, repo.ChunkKey(h), body)
	return h
}

// putManifest writes a minimal backup-manifest body referencing the
// listed chunk hashes. Replicate only decodes the chunk-reference
// shape, so we don't need a full backup.Manifest here.
func putManifest(t *testing.T, sp storage.StoragePlugin, deployment, backupID string, hashes []repo.Hash) {
	t.Helper()
	type chunkRef struct {
		Hash string `json:"hash"`
	}
	type file struct {
		Path   string     `json:"path"`
		Chunks []chunkRef `json:"chunks"`
	}
	body := struct {
		BackupID string `json:"backup_id"`
		Files    []file `json:"files"`
	}{
		BackupID: backupID,
		Files: []file{{
			Path: "data",
		}},
	}
	for _, h := range hashes {
		body.Files[0].Chunks = append(body.Files[0].Chunks, chunkRef{Hash: h.String()})
	}
	enc, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	putRaw(t, sp, fmt.Sprintf("manifests/%s/backups/%s/manifest.json", deployment, backupID), enc)
}

// putWALManifest writes a minimal WAL segment-manifest body.
func putWALManifest(t *testing.T, sp storage.StoragePlugin, deployment, tli, seg string, hashes []repo.Hash) {
	t.Helper()
	type chunkRef struct {
		Hash string `json:"hash"`
	}
	body := struct {
		Chunks []chunkRef `json:"chunks"`
	}{}
	for _, h := range hashes {
		body.Chunks = append(body.Chunks, chunkRef{Hash: h.String()})
	}
	enc, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	putRaw(t, sp, fmt.Sprintf("wal/%s/%s/%s.json", deployment, tli, seg), enc)
}

// statExists reports whether key is present at sp.
func statExists(t *testing.T, sp storage.StoragePlugin, key string) bool {
	t.Helper()
	_, err := sp.Stat(context.Background(), key)
	if err == nil {
		return true
	}
	if errors.Is(err, storage.ErrNotFound) {
		return false
	}
	t.Fatalf("stat %s: %v", key, err)
	return false
}

// TestReplicate_HappyPath covers the canonical case: one backup, two
// chunks, fresh destination. Everything should land at dst with one
// chunk + manifest copy each.
func TestReplicate_HappyPath(t *testing.T) {
	src, dst := twoRepos(t)
	chunk1 := putChunk(t, src, []byte("hello-chunk-one"))
	chunk2 := putChunk(t, src, []byte("hello-chunk-two"))
	putManifest(t, src, "db1", "db1.full.20260430T0900Z", []repo.Hash{chunk1, chunk2})

	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if res.ManifestsCopied != 1 || res.ManifestsConsidered != 1 {
		t.Errorf("manifests: copied=%d considered=%d, want 1/1", res.ManifestsCopied, res.ManifestsConsidered)
	}
	if res.ChunksCopied != 2 || res.ChunksConsidered != 2 {
		t.Errorf("chunks: copied=%d considered=%d, want 2/2", res.ChunksCopied, res.ChunksConsidered)
	}
	if res.ChunksFailed != 0 || res.ManifestsFailed != 0 {
		t.Errorf("unexpected failures: chunks=%d manifests=%d", res.ChunksFailed, res.ManifestsFailed)
	}

	// Both chunks present at dst.
	if !statExists(t, dst, repo.ChunkKey(chunk1)) {
		t.Error("chunk1 missing at dst")
	}
	if !statExists(t, dst, repo.ChunkKey(chunk2)) {
		t.Error("chunk2 missing at dst")
	}
	if !statExists(t, dst, "manifests/db1/backups/db1.full.20260430T0900Z/manifest.json") {
		t.Error("manifest missing at dst")
	}
}

// TestReplicate_Idempotent: a second run is a no-op (everything skipped).
func TestReplicate_Idempotent(t *testing.T) {
	src, dst := twoRepos(t)
	chunk := putChunk(t, src, []byte("idempotent"))
	putManifest(t, src, "db1", "db1.full.idem", []repo.Hash{chunk})

	first, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("first replicate: %v", err)
	}
	if first.ManifestsCopied != 1 || first.ChunksCopied != 1 {
		t.Errorf("first run: %+v", first)
	}

	second, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("second replicate: %v", err)
	}
	if second.ManifestsCopied != 0 || second.ChunksCopied != 0 {
		t.Errorf("second run should be all skips; got copied: manifest=%d chunk=%d",
			second.ManifestsCopied, second.ChunksCopied)
	}
	if second.ManifestsSkipped != 1 || second.ChunksSkipped != 1 {
		t.Errorf("second run skip counts wrong: %+v", second)
	}
}

// TestReplicate_MissingHSREPO refuses when dst isn't a real repo.
func TestReplicate_MissingHSREPO(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir() // no Init — plain empty directory
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + srcRoot}); err != nil {
		t.Fatal(err)
	}
	src := &fs.Plugin{}
	src.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: srcRoot}})
	defer src.Close()
	dst := &fs.Plugin{}
	dst.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: dstRoot}})
	defer dst.Close()

	_, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if !errors.Is(err, repo.ErrNotARepo) {
		t.Errorf("expected ErrNotARepo, got %v", err)
	}
}

// TestReplicate_TombstonedSkipped: a tombstoned backup at src is not
// replicated, even if its chunks/manifest still exist at src.
func TestReplicate_TombstonedSkipped(t *testing.T) {
	src, dst := twoRepos(t)
	chunkLive := putChunk(t, src, []byte("live"))
	chunkDead := putChunk(t, src, []byte("dead"))
	putManifest(t, src, "db1", "db1.full.alive", []repo.Hash{chunkLive})
	putManifest(t, src, "db1", "db1.full.gravestone", []repo.Hash{chunkDead})
	// Plant a tombstone marker for the dead one.
	putRaw(t, src, "manifests/db1/backups/db1.full.gravestone/manifest.json.tombstone",
		[]byte(`{"backup_id":"db1.full.gravestone"}`))

	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if res.ManifestsCopied != 1 {
		t.Errorf("ManifestsCopied=%d, want 1 (alive only)", res.ManifestsCopied)
	}
	if res.ManifestsTombstoned != 1 {
		t.Errorf("ManifestsTombstoned=%d, want 1", res.ManifestsTombstoned)
	}
	if statExists(t, dst, "manifests/db1/backups/db1.full.gravestone/manifest.json") {
		t.Error("tombstoned manifest should NOT be at dst")
	}
	// The dead chunk wasn't referenced by any non-tombstoned manifest
	// in this run, so it's correctly NOT at dst.
	if statExists(t, dst, repo.ChunkKey(chunkDead)) {
		t.Error("dead chunk should NOT be at dst (no live manifest references it)")
	}
}

// TestReplicate_PartialResume: one chunk already at dst (from a
// previous interrupted run), the other not. Replicate should copy
// only the missing chunk.
func TestReplicate_PartialResume(t *testing.T) {
	src, dst := twoRepos(t)
	chunk1 := putChunk(t, src, []byte("already-replicated"))
	chunk2 := putChunk(t, src, []byte("yet-to-go"))
	putManifest(t, src, "db1", "db1.full.partial", []repo.Hash{chunk1, chunk2})

	// Pre-plant chunk1 at dst (simulating a half-finished prior run).
	putRaw(t, dst, repo.ChunkKey(chunk1), []byte("already-replicated"))

	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if res.ChunksCopied != 1 {
		t.Errorf("ChunksCopied=%d, want 1", res.ChunksCopied)
	}
	if res.ChunksSkipped != 1 {
		t.Errorf("ChunksSkipped=%d, want 1", res.ChunksSkipped)
	}
	if res.ManifestsCopied != 1 {
		t.Errorf("ManifestsCopied=%d, want 1", res.ManifestsCopied)
	}
}

// TestReplicate_DryRun reports the work but writes nothing.
func TestReplicate_DryRun(t *testing.T) {
	src, dst := twoRepos(t)
	chunk := putChunk(t, src, []byte("dry"))
	putManifest(t, src, "db1", "db1.full.dry", []repo.Hash{chunk})

	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if res.ChunksCopied != 1 || res.ManifestsCopied != 1 {
		t.Errorf("dry-run should still report counts; got %+v", res)
	}
	if !res.DryRun {
		t.Error("DryRun flag not propagated to result")
	}
	// Nothing at dst.
	if statExists(t, dst, repo.ChunkKey(chunk)) {
		t.Error("dry-run wrote a chunk to dst")
	}
	if statExists(t, dst, "manifests/db1/backups/db1.full.dry/manifest.json") {
		t.Error("dry-run wrote a manifest to dst")
	}
}

// TestReplicate_ChunkMissingAtSource: manifest references a chunk
// the source doesn't have. Failure is recorded but the run continues.
func TestReplicate_ChunkMissingAtSource(t *testing.T) {
	src, dst := twoRepos(t)
	good := putChunk(t, src, []byte("good"))
	bogus, _ := repo.ParseHash("dead000000000000000000000000000000000000000000000000000000000000")
	putManifest(t, src, "db1", "db1.full.broken", []repo.Hash{good, bogus})

	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if res.ChunksCopied != 1 {
		t.Errorf("ChunksCopied=%d, want 1", res.ChunksCopied)
	}
	if res.ChunksMissing != 1 {
		t.Errorf("ChunksMissing=%d, want 1", res.ChunksMissing)
	}
	// The manifest still gets copied — broken-at-src is the source's
	// problem, not ours; at least the replica will tell the same
	// story.
	if res.ManifestsCopied != 1 {
		t.Errorf("ManifestsCopied=%d, want 1", res.ManifestsCopied)
	}
	if len(res.Failures) == 0 {
		t.Error("expected a Failure entry for the missing chunk")
	}
}

// TestReplicate_IncludeWAL also walks wal/<dep>/<tli>/ segment manifests.
func TestReplicate_IncludeWAL(t *testing.T) {
	src, dst := twoRepos(t)
	walChunk := putChunk(t, src, []byte("wal-data"))
	putWALManifest(t, src, "db1", "1", "00000001000000000000000A", []repo.Hash{walChunk})

	// Default (IncludeWAL=false): WAL is skipped.
	resOff, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("replicate (off): %v", err)
	}
	if resOff.WALManifestsCopied != 0 {
		t.Errorf("default should skip WAL; got WALManifestsCopied=%d", resOff.WALManifestsCopied)
	}

	// IncludeWAL=true: WAL manifest + its chunk both copied.
	resOn, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{IncludeWAL: true})
	if err != nil {
		t.Fatalf("replicate (on): %v", err)
	}
	if resOn.WALManifestsCopied != 1 {
		t.Errorf("WALManifestsCopied=%d, want 1", resOn.WALManifestsCopied)
	}
	if resOn.ChunksCopied != 1 {
		t.Errorf("ChunksCopied=%d, want 1 (the WAL chunk)", resOn.ChunksCopied)
	}
	if !statExists(t, dst, repo.ChunkKey(walChunk)) {
		t.Error("WAL chunk missing at dst")
	}
	if !statExists(t, dst, "wal/db1/1/00000001000000000000000A.json") {
		t.Error("WAL manifest missing at dst")
	}
}

// TestReplicate_IncludeWAL_CopiesAuxFiles pins the DR-completeness fix:
// IncludeWAL must copy not only the `.json` segment manifests but also
// the WAL auxiliary files — timeline `.history` (both the follower
// timeline store and the archive aux), `.backup`, and `.partial`. The
// `.history` is REQUIRED for a PITR at the DR site to navigate across a
// failover's timeline switch; without it the replica is unrecoverable
// past the switch. Previously the walk filtered to `.json` only and
// silently dropped every aux file.
func TestReplicate_IncludeWAL_CopiesAuxFiles(t *testing.T) {
	src, dst := twoRepos(t)
	walChunk := putChunk(t, src, []byte("wal-data"))
	putWALManifest(t, src, "db1", "1", "00000001000000000000000A", []repo.Hash{walChunk})

	// Aux files PG / the agent archive alongside the segments.
	auxFiles := map[string][]byte{
		"wal/db1/timelines/2.history":                []byte("1\t0/3000028\tafter failover\n"), // follower store
		"wal/db1/history/00000002.history":           []byte("1\t0/3000028\tarchive aux\n"),    // archive aux
		"wal/db1/1/000000010000000000000005.backup":  []byte("START WAL LOCATION: 0/5000028\n"),
		"wal/db1/1/00000001000000000000000B.partial": make([]byte, 4096),
	}
	for k, v := range auxFiles {
		putRaw(t, src, k, v)
	}

	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{IncludeWAL: true})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if res.WALAuxConsidered != len(auxFiles) || res.WALAuxCopied != len(auxFiles) {
		t.Errorf("WALAux considered=%d copied=%d, want %d each", res.WALAuxConsidered, res.WALAuxCopied, len(auxFiles))
	}
	for k := range auxFiles {
		if !statExists(t, dst, k) {
			t.Errorf("WAL aux file missing at dst: %s", k)
		}
	}
	// The segment manifest still copied too (not regressed).
	if !statExists(t, dst, "wal/db1/1/00000001000000000000000A.json") {
		t.Error("WAL segment manifest missing at dst")
	}
}

// TestReplicate_ReplicaSidecar: if a manifests/_replicas/<id>.manifest.json
// exists at src, we mirror it too. Failure to mirror does NOT bump
// ManifestsFailed (the primary is authoritative; replica is best-effort).
func TestReplicate_ReplicaSidecar(t *testing.T) {
	src, dst := twoRepos(t)
	chunk := putChunk(t, src, []byte("with-sidecar"))
	putManifest(t, src, "db1", "db1.full.sidecar", []repo.Hash{chunk})
	// The replica copy at the conventional path.
	putRaw(t, src, "manifests/_replicas/db1.full.sidecar.manifest.json",
		[]byte(`{"backup_id":"db1.full.sidecar"}`))

	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if res.ManifestsFailed != 0 {
		t.Errorf("unexpected ManifestsFailed=%d", res.ManifestsFailed)
	}
	if !statExists(t, dst, "manifests/_replicas/db1.full.sidecar.manifest.json") {
		t.Error("replica sidecar missing at dst")
	}
}

// TestReplicate_NilPlugins is the obvious validation guard.
func TestReplicate_NilPlugins(t *testing.T) {
	if _, err := repo.Replicate(context.Background(), nil, nil, repo.ReplicateOptions{}); err == nil {
		t.Error("expected error for nil plugins")
	}
}

// TestReplicate_OnProgressCallback fires once per top-level step.
func TestReplicate_OnProgressCallback(t *testing.T) {
	src, dst := twoRepos(t)
	chunk := putChunk(t, src, []byte("p"))
	putManifest(t, src, "db1", "db1.full.progress", []repo.Hash{chunk})

	var stages []string
	_, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{
		OnProgress: func(ev repo.ReplicateProgress) {
			stages = append(stages, ev.Stage)
		},
	})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if len(stages) != 1 || stages[0] != "manifest" {
		t.Errorf("progress stages=%v, want [manifest]", stages)
	}
}

// recordingWORMSP wraps a plugin to (a) advertise WORM capability so the
// retention guard passes and (b) capture the PutOptions of every write,
// so a test can assert the retention deadline/mode was applied.
type recordingWORMSP struct {
	storage.StoragePlugin
	mu   sync.Mutex
	puts map[string]storage.PutOptions
}

func (s *recordingWORMSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	s.mu.Lock()
	if s.puts == nil {
		s.puts = map[string]storage.PutOptions{}
	}
	s.puts[key] = opts
	s.mu.Unlock()
	return s.StoragePlugin.Put(ctx, key, r, opts)
}

func (s *recordingWORMSP) Capabilities() storage.Capabilities {
	c := s.StoragePlugin.Capabilities()
	c.WORM = true
	return c
}

// TestReplicate_AppliesDstWORMRetention pins the fix: when the
// destination repo is WORM-configured, every replicated object (chunk
// AND manifest) must be written with the retention deadline + mode, so a
// compliance DR replica is actually immutable rather than freely
// deletable.
func TestReplicate_AppliesDstWORMRetention(t *testing.T) {
	src, dst := twoRepos(t)
	rec := &recordingWORMSP{StoragePlugin: dst}

	chunk := putChunk(t, src, []byte("immutable-chunk-bytes"))
	putManifest(t, src, "db1", "db1.full.worm", []repo.Hash{chunk})

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	policy := &repo.WORMPolicy{Mode: "compliance", RetentionSeconds: 3600}
	res, err := repo.Replicate(context.Background(), src, rec, repo.ReplicateOptions{
		DstWORM: policy,
		Now:     now,
	})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if res.ChunksCopied != 1 || res.ManifestsCopied != 1 {
		t.Fatalf("expected 1 chunk + 1 manifest copied; got %d / %d", res.ChunksCopied, res.ManifestsCopied)
	}

	wantDeadline := now.Add(3600 * time.Second)
	for _, key := range []string{
		repo.ChunkKey(chunk),
		"manifests/db1/backups/db1.full.worm/manifest.json",
	} {
		opts, ok := rec.puts[key]
		if !ok {
			t.Errorf("no Put recorded for %s", key)
			continue
		}
		if !opts.RetainUntil.Equal(wantDeadline) {
			t.Errorf("%s RetainUntil = %v, want %v (retention not applied → replica is deletable)", key, opts.RetainUntil, wantDeadline)
		}
		if opts.RetentionMode != storage.WORMMode("compliance") {
			t.Errorf("%s RetentionMode = %q, want compliance", key, opts.RetentionMode)
		}
	}
}

// TestReplicate_RefusesUnenforceableWORM: a WORM-configured destination
// whose backend can't enforce retention (plain fs) must be refused, not
// silently produce a replica the operator believes is immutable.
func TestReplicate_RefusesUnenforceableWORM(t *testing.T) {
	src, dst := twoRepos(t) // dst is a plain fs repo: Capabilities().WORM == false

	chunk := putChunk(t, src, []byte("x"))
	putManifest(t, src, "db1", "db1.full.noworm", []repo.Hash{chunk})

	_, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{
		DstWORM: &repo.WORMPolicy{Mode: "compliance", RetentionSeconds: 3600},
	})
	if !errors.Is(err, repo.ErrRetentionUnenforceable) {
		t.Fatalf("err = %v, want ErrRetentionUnenforceable (must refuse a false-compliance replica)", err)
	}

	// With the explicit override, it proceeds.
	if _, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{
		DstWORM:             &repo.WORMPolicy{Mode: "compliance", RetentionSeconds: 3600},
		AllowUnenforcedWORM: true,
	}); err != nil {
		t.Fatalf("with AllowUnenforcedWORM: %v", err)
	}
}
