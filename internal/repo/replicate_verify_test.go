package repo_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// rvWorld is a self-contained two-repo fixture: an init'd src
// repo + an init'd dst repo, with helpers to plant matching /
// drifted / missing keys at either end.
type rvWorld struct {
	srcSP, dstSP storage.StoragePlugin
	srcStore     *backup.ManifestStore
	dstStore     *backup.ManifestStore
	srcURL       string
	dstURL       string
	signer       *backup.Signer
	verifier     *backup.Verifier
}

func setupRVWorld(t *testing.T) *rvWorld {
	t.Helper()
	openRepo := func(name string) (*fs.Plugin, string) {
		root := t.TempDir()
		repoURL := "file://" + root
		if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
			t.Fatal(err)
		}
		sp := &fs.Plugin{}
		if err := sp.Open(context.Background(), storage.StorageConfig{
			URL: &url.URL{Scheme: "file", Path: root},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sp.Close() })
		return sp, repoURL
	}
	srcSP, srcURL := openRepo("src")
	dstSP, dstURL := openRepo("dst")
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	return &rvWorld{
		srcSP:    srcSP,
		dstSP:    dstSP,
		srcStore: backup.NewManifestStore(srcSP),
		dstStore: backup.NewManifestStore(dstSP),
		srcURL:   srcURL,
		dstURL:   dstURL,
		signer:   signer,
		verifier: verifier,
	}
}

// commitToBoth plants a backup on src and (optionally) replicates
// it to dst.  Returns the backup ID.
func (w *rvWorld) commitToBoth(t *testing.T, deployment string, idx int, body []byte, replicate bool) string {
	t.Helper()
	cas := casdefault.New(w.srcSP)
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	stoppedAt := time.Date(2026, 5, 1, 12, idx, 0, 0, time.UTC)
	id := deployment + ".full." + stoppedAt.Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        stoppedAt.Add(-30 * time.Second),
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{{
			Path: "data/" + id, Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}},
		}},
	}
	if err := w.srcStore.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if replicate {
		// Use repo.Replicate to copy primary → replica.  This
		// exercises the same code path the operator runs.
		if _, err := repo.Replicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateOptions{}); err != nil {
			t.Fatalf("Replicate: %v", err)
		}
	}
	return id
}

// TestVerifyReplicate_Validation: nil src / dst error.
func TestVerifyReplicate_Validation(t *testing.T) {
	w := setupRVWorld(t)
	if _, err := repo.VerifyReplicate(context.Background(), nil, w.dstSP, repo.ReplicateVerifyOptions{}); err == nil {
		t.Error("nil src must error")
	}
	if _, err := repo.VerifyReplicate(context.Background(), w.srcSP, nil, repo.ReplicateVerifyOptions{}); err == nil {
		t.Error("nil dst must error")
	}
	if _, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{
		SampleRate: 1.5,
	}); err == nil {
		t.Error("SampleRate > 1 must error")
	}
	if _, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{
		SampleRate: -0.1,
	}); err == nil {
		t.Error("SampleRate < 0 must error")
	}
}

// TestVerifyReplicate_BothEmpty: two fresh repos verify clean.
func TestVerifyReplicate_BothEmpty(t *testing.T) {
	w := setupRVWorld(t)
	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyReplicate: %v", err)
	}
	if r.Verdict != repo.VerdictConsistent {
		t.Errorf("Verdict = %q, want consistent", r.Verdict)
	}
	if r.ManifestsConsidered != 0 {
		t.Errorf("ManifestsConsidered = %d", r.ManifestsConsidered)
	}
}

// TestVerifyReplicate_Consistent: src + dst have the same backup
// → consistent verdict, every counter matches.
func TestVerifyReplicate_Consistent(t *testing.T) {
	w := setupRVWorld(t)
	w.commitToBoth(t, "db1", 1, []byte("payload"), true)

	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyReplicate: %v", err)
	}
	if r.Verdict != repo.VerdictConsistent {
		t.Errorf("Verdict = %q, want consistent\n%+v", r.Verdict, r)
	}
	if r.ManifestsConsidered != 1 || r.ManifestsPresent != 1 {
		t.Errorf("manifests off: %+v", r)
	}
	if r.ChunksConsidered != 1 || r.ChunksPresent != 1 {
		t.Errorf("chunks off: %+v", r)
	}
	if r.AnyMissing() || r.AnyDrifted() {
		t.Errorf("anything off: %+v", r)
	}
}

// TestVerifyReplicate_Broken_MissingManifest: src has a backup,
// dst doesn't → broken + missing_manifest failure.
func TestVerifyReplicate_Broken_MissingManifest(t *testing.T) {
	w := setupRVWorld(t)
	w.commitToBoth(t, "db1", 1, []byte("payload"), false) // NOT replicated

	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyReplicate: %v", err)
	}
	if r.Verdict != repo.VerdictBroken {
		t.Errorf("Verdict = %q, want broken", r.Verdict)
	}
	if r.ManifestsMissing != 1 {
		t.Errorf("ManifestsMissing = %d, want 1", r.ManifestsMissing)
	}
	hasMissingFailure := false
	for _, f := range r.Failures {
		if f.Kind == "missing_manifest" {
			hasMissingFailure = true
		}
	}
	if !hasMissingFailure {
		t.Errorf("expected missing_manifest in Failures: %+v", r.Failures)
	}
}

// TestVerifyReplicate_Broken_MissingChunk: replicate the manifest
// but delete the chunk on dst → broken via missing_chunk.
func TestVerifyReplicate_Broken_MissingChunk(t *testing.T) {
	w := setupRVWorld(t)
	w.commitToBoth(t, "db1", 1, []byte("payload"), true)

	// Find + delete every chunk under chunks/ on dst.
	for info, lerr := range w.dstSP.List(context.Background(), "chunks/") {
		if lerr != nil {
			t.Fatal(lerr)
		}
		if err := w.dstSP.Delete(context.Background(), info.Key); err != nil {
			t.Fatalf("delete %s: %v", info.Key, err)
		}
	}

	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyReplicate: %v", err)
	}
	if r.Verdict != repo.VerdictBroken {
		t.Errorf("Verdict = %q, want broken", r.Verdict)
	}
	if r.ChunksMissing == 0 {
		t.Errorf("ChunksMissing = 0, want > 0")
	}
}

// TestVerifyReplicate_Drifted_ContentMismatch: replicate, then
// overwrite the dst chunk with different bytes → drifted via
// content_drift (size mismatch).
func TestVerifyReplicate_Drifted_ContentMismatch(t *testing.T) {
	w := setupRVWorld(t)
	w.commitToBoth(t, "db1", 1, []byte("payload"), true)

	// Mutate the chunk at dst by re-Putting different bytes.
	for info, lerr := range w.dstSP.List(context.Background(), "chunks/") {
		if lerr != nil {
			t.Fatal(lerr)
		}
		// Delete-then-Put with different bytes (Put with
		// IfNotExists=false overwrites a delete).
		_ = w.dstSP.Delete(context.Background(), info.Key)
		body := []byte("DIFFERENT-content-than-source")
		if _, err := w.dstSP.Put(context.Background(), info.Key,
			byteReader(body),
			storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyReplicate: %v", err)
	}
	if r.Verdict != repo.VerdictDrifted {
		t.Errorf("Verdict = %q, want drifted (size mismatch)\n%+v", r.Verdict, r)
	}
	if r.ChunksContentDrift == 0 {
		t.Errorf("ChunksContentDrift = 0, want > 0")
	}
}

// TestVerifyReplicate_DeploymentFilter: only the named deployment
// is considered; other deployments don't show up in counters.
func TestVerifyReplicate_DeploymentFilter(t *testing.T) {
	w := setupRVWorld(t)
	w.commitToBoth(t, "db1", 1, []byte("a"), true)
	w.commitToBoth(t, "db2", 2, []byte("b"), false) // not replicated

	// Without filter: should detect db2's missing manifest.
	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != repo.VerdictBroken {
		t.Errorf("global: Verdict = %q, want broken", r.Verdict)
	}

	// With deployment=db1 filter: db2 is invisible; verdict
	// should be consistent.
	r, err = repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{
		Deployment: "db1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != repo.VerdictConsistent {
		t.Errorf("filtered: Verdict = %q, want consistent (db2 invisible)", r.Verdict)
	}
	if r.ManifestsConsidered != 1 {
		t.Errorf("ManifestsConsidered = %d, want 1 (filtered)", r.ManifestsConsidered)
	}
}

// TestVerifyReplicate_DeepMode: --deep flag fetches both bodies +
// compares.  Without deep, identical-size-different-content keys
// would pass; with deep, we catch the byte mismatch.
func TestVerifyReplicate_DeepMode(t *testing.T) {
	w := setupRVWorld(t)
	w.commitToBoth(t, "db1", 1, []byte("payload"), true)

	// Find a chunk; replace the dst body with same-size-different-bytes.
	var chunkKey string
	for info, lerr := range w.dstSP.List(context.Background(), "chunks/") {
		if lerr != nil {
			t.Fatal(lerr)
		}
		chunkKey = info.Key
		break
	}
	if chunkKey == "" {
		t.Fatal("no chunk in dst")
	}
	srcInfo, _ := w.srcSP.Stat(context.Background(), chunkKey)
	body := make([]byte, srcInfo.Size)
	for i := range body {
		body[i] = 'X'
	}
	_ = w.dstSP.Delete(context.Background(), chunkKey)
	if _, err := w.dstSP.Put(context.Background(), chunkKey,
		byteReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Stat-only mode: same-size, no drift detected (this is the
	// documented limitation of Stat-only).
	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != repo.VerdictConsistent {
		t.Errorf("stat-only: Verdict = %q, want consistent (size matches)", r.Verdict)
	}

	// Deep mode: byte mismatch is detected.
	r, err = repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{
		Deep: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != repo.VerdictDrifted {
		t.Errorf("deep: Verdict = %q, want drifted", r.Verdict)
	}
}

// TestVerifyReplicate_OnProgress: callback fires per checked key.
func TestVerifyReplicate_OnProgress(t *testing.T) {
	w := setupRVWorld(t)
	w.commitToBoth(t, "db1", 1, []byte("payload"), true)

	var progressCalls int
	_, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{
		OnProgress: func(ev repo.ReplicateVerifyProgress) {
			progressCalls++
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 1 manifest + 1 chunk = 2 calls minimum.
	if progressCalls < 2 {
		t.Errorf("OnProgress fired %d times, want >= 2", progressCalls)
	}
}

// TestVerifyReplicate_FailuresCap: very large fleets cap the
// per-key Failures slice while keeping counters accurate.
func TestVerifyReplicate_FailuresCap(t *testing.T) {
	w := setupRVWorld(t)
	// Plant 250 backups on src (none replicated).
	for i := 0; i < 250; i++ {
		w.commitToBoth(t, "db1", i, []byte("payload"), false)
	}

	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.ManifestsMissing != 250 {
		t.Errorf("ManifestsMissing = %d, want 250 (counter)", r.ManifestsMissing)
	}
	// Failures slice capped at 200.
	if len(r.Failures) != 200 {
		t.Errorf("Failures = %d, want 200 (cap)", len(r.Failures))
	}
}

// TestVerifyReplicate_AnyMissingDrifted_Helpers
func TestVerifyReplicate_AnyMissingDrifted_Helpers(t *testing.T) {
	r := &repo.ReplicateVerifyResult{ManifestsMissing: 1}
	if !r.AnyMissing() {
		t.Errorf("AnyMissing = false; want true")
	}
	if r.AnyDrifted() {
		t.Errorf("AnyDrifted = true; want false")
	}
	r2 := &repo.ReplicateVerifyResult{ChunksContentDrift: 1}
	if r2.AnyMissing() {
		t.Errorf("AnyMissing on drift = true; want false")
	}
	if !r2.AnyDrifted() {
		t.Errorf("AnyDrifted = false; want true")
	}
}

// TestVerifyReplicate_ContextCancellation
func TestVerifyReplicate_ContextCancellation(t *testing.T) {
	w := setupRVWorld(t)
	w.commitToBoth(t, "db1", 1, []byte("a"), true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.VerifyReplicate(ctx, w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{}); err == nil {
		t.Error("expected ctx error")
	}
}

// TestVerifyReplicate_VerdictPrecedence: broken takes precedence
// over drifted.
func TestVerifyReplicate_VerdictPrecedence(t *testing.T) {
	w := setupRVWorld(t)
	// One missing + one drifted.
	w.commitToBoth(t, "db1", 1, []byte("p1"), true)
	w.commitToBoth(t, "db2", 2, []byte("p2"), false) // not replicated → missing

	// Drift one of db1's chunks.
	for info, lerr := range w.dstSP.List(context.Background(), "chunks/") {
		if lerr != nil {
			t.Fatal(lerr)
		}
		_ = w.dstSP.Delete(context.Background(), info.Key)
		body := []byte("DIFFERENT")
		if _, err := w.dstSP.Put(context.Background(), info.Key,
			byteReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
			t.Fatalf("put: %v", err)
		}
		break
	}

	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != repo.VerdictBroken {
		t.Errorf("Verdict = %q, want broken (broken > drifted)", r.Verdict)
	}
}

// byteReader uses bytes.Reader so io.EOF flows correctly through
// the fs storage plugin's body-reader.
func byteReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// rvPut plants raw bytes at key on the given plugin.
func rvPut(t *testing.T, sp storage.StoragePlugin, key string, body []byte) {
	t.Helper()
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// TestVerifyReplicate_WAL_DetectsMissing pins two replicate-verify bugs:
//
//  1. isWALSegmentManifestKey matched a `.wal.json` suffix the streamer
//     never produces (segment manifests end in plain `.json`), so the WAL
//     verify considered ZERO segments and always reported "consistent" —
//     even against a replica with NO WAL at all.
//  2. WAL auxiliary files (.history/.backup/.partial) were not verified,
//     so a replica missing its timeline history (needed for cross-failover
//     PITR) passed verify clean.
//
// Both must now surface as missing.
func TestVerifyReplicate_WAL_DetectsMissing(t *testing.T) {
	w := setupRVWorld(t)
	seg := []byte(`{"schema":"pg_hardstorage.wal_segment.v1","chunks":[]}`)
	hist := []byte("1\t0/3000028\tafter failover\n")

	// SRC has a segment manifest + a timeline .history; DST has NEITHER.
	rvPut(t, w.srcSP, "wal/db1/00000001/00000001000000000000000A.json", seg)
	rvPut(t, w.srcSP, "wal/db1/timelines/2.history", hist)

	r, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP,
		repo.ReplicateVerifyOptions{IncludeWAL: true})
	if err != nil {
		t.Fatalf("VerifyReplicate: %v", err)
	}
	if r.WALManifestsConsidered != 1 {
		t.Errorf("WALManifestsConsidered = %d, want 1 (the .json segment manifest must be seen, not skipped)", r.WALManifestsConsidered)
	}
	if r.WALManifestsMissing != 1 {
		t.Errorf("WALManifestsMissing = %d, want 1 (missing at dst)", r.WALManifestsMissing)
	}
	if r.WALAuxConsidered != 1 || r.WALAuxMissing != 1 {
		t.Errorf("WALAux considered=%d missing=%d, want 1/1 (.history missing at dst)", r.WALAuxConsidered, r.WALAuxMissing)
	}
	if !r.AnyMissing() || r.Verdict != repo.VerdictBroken {
		t.Errorf("verdict = %q, AnyMissing=%v; want broken with missing", r.Verdict, r.AnyMissing())
	}

	// Now mirror both to DST → verify clean.
	rvPut(t, w.dstSP, "wal/db1/00000001/00000001000000000000000A.json", seg)
	rvPut(t, w.dstSP, "wal/db1/timelines/2.history", hist)
	r2, err := repo.VerifyReplicate(context.Background(), w.srcSP, w.dstSP,
		repo.ReplicateVerifyOptions{IncludeWAL: true})
	if err != nil {
		t.Fatalf("VerifyReplicate (2): %v", err)
	}
	if r2.WALManifestsPresent != 1 || r2.WALAuxPresent != 1 || r2.AnyMissing() {
		t.Errorf("after mirroring, want both present and nothing missing; got %+v", r2)
	}
}
