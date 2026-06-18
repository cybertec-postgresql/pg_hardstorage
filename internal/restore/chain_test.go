package restore_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/combine"
)

// chainFixture builds a 2-link chain (full + 1 incremental) with a
// real CAS so the chain-restore path materialises bytes through the
// production code path. The "incremental" we synthesise carries an
// INCREMENTAL.<file> placeholder file plus a fake backup_manifest
// blob — enough for our pipeline to materialise into staging dirs;
// the actual pg_combinebackup invocation is what real PG would
// reject (we don't run it without the binary on PATH).
type chainFixture struct {
	repoURL  string
	verifier *backup.Verifier
	full     *backup.Manifest
	inc      *backup.Manifest
}

func newChainFixture(t *testing.T) *chainFixture {
	return newChainFixtureWithLeafGaps(t, nil)
}

// newChainFixtureWithLeafGaps is newChainFixture with optional WAL gaps
// embedded on the incremental leaf manifest — used to exercise the
// chain-path WAL-gap pre-flight (data-loss audit #4).
func newChainFixtureWithLeafGaps(t *testing.T, leafGaps []backup.WALGap) *chainFixture {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cas := repo.NewCAS(sp)

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	put := func(body []byte) (backup.ChunkRef, int64) {
		info, err := cas.PutChunk(context.Background(), body)
		if err != nil {
			t.Fatalf("put chunk: %v", err)
		}
		return backup.ChunkRef{Hash: info.Hash, Offset: 0, Len: info.Size}, info.Size
	}

	// Full anchor: 2 small files + a backup_label + PGBackupManifest.
	pgVerBody := []byte("17\n")
	heapBody := []byte("the quick brown fox heap page goes here xx")
	pgVerCh, pgVerSize := put(pgVerBody)
	heapCh, heapSize := put(heapBody)

	now := time.Now().UTC()
	full := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.20260430T120000Z.aa01",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        170,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        now.Add(-2 * time.Hour),
		StoppedAt:        now.Add(-2 * time.Hour).Add(time.Minute),
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: pgVerSize, Mode: 0o600, Chunks: []backup.ChunkRef{pgVerCh}},
			{Path: "base/16384/2619", Size: heapSize, Mode: 0o600, Chunks: []backup.ChunkRef{heapCh}},
		},
		BackupLabel:      "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\nLABEL: full\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		PGBackupManifest: []byte(`{"PostgreSQL-Backup-Manifest-Version":1,"Files":[]}`),
	}

	// Incremental: pretend a single page changed, write that as
	// INCREMENTAL.<path> per PG 17 convention; chunk content is the
	// CAS-deduped delta bytes.
	deltaBody := []byte("INCREMENTAL placeholder body")
	deltaCh, deltaSize := put(deltaBody)
	inc := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.incremental_lsn.20260430T130000Z.bb02",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeIncremental,
		ParentBackupID:   full.BackupID,
		PGVersion:        170,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/30001A0",
		StopLSN:          "0/3000300",
		Timeline:         1,
		StartedAt:        now.Add(-time.Hour),
		StoppedAt:        now.Add(-time.Hour).Add(30 * time.Second),
		Files: []backup.FileEntry{
			{Path: "INCREMENTAL.base/16384/2619", Size: deltaSize, Mode: 0o600, Chunks: []backup.ChunkRef{deltaCh}},
		},
		BackupLabel:      "START WAL LOCATION: 0/30001A0 (file 000000010000000000000003)\nLABEL: incr\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		PGBackupManifest: []byte(`{"PostgreSQL-Backup-Manifest-Version":1,"Files":[],"Incremental":true}`),
		WALGaps:          leafGaps,
	}

	store := backup.NewManifestStore(sp)
	if err := store.Commit(context.Background(), full, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit full: %v", err)
	}
	if err := store.Commit(context.Background(), inc, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit inc: %v", err)
	}
	return &chainFixture{repoURL: repoURL, verifier: verifier, full: full, inc: inc}
}

// TestRestore_ChainDispatchesToIncrementalPath: when the leaf
// manifest is incremental, Restore routes to chain-restore. We
// verify the dispatch by clearing PATH so combine.Run surfaces the
// preflight.pg_combinebackup_missing code BEFORE materialisation
// happens — this proves the chain path was taken and pre-flight ran
// in the right order. (The full-restore path doesn't probe PATH,
// so the same flag wouldn't trip there.)
func TestRestore_ChainDispatchesToIncrementalPath(t *testing.T) {
	fx := newChainFixture(t)
	target := filepath.Join(t.TempDir(), "merged")
	t.Setenv("PATH", "")

	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   fx.inc.BackupID,
		TargetDir:  target,
		Verifier:   fx.verifier,
	})
	if err == nil {
		t.Fatal("expected pg_combinebackup-missing error on chain restore with empty PATH")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "preflight.pg_combinebackup_missing" {
		t.Errorf("expected preflight.pg_combinebackup_missing, got %v", err)
	}
	// TargetDir should not have been created (pre-flight failed
	// before any I/O).
	if _, statErr := os.Stat(target); statErr == nil {
		t.Errorf("target %q should not exist after pre-flight failure", target)
	}
}

// TestRestore_ChainRefusesPITRTargetInWALGap pins data-loss path #4:
// an incremental-chain PITR restore whose target lands inside a known
// WAL gap is refused. Re-verifying #4 confirmed the guard already
// exists — Restore() runs the WAL-gap pre-flight BEFORE dispatching to
// the chain path (using the leaf's WALGaps), so chain restores inherit
// it. This end-to-end test (which #4 lacked) locks that in: the refusal
// (restore.target_in_wal_gap) fires before the pg_combinebackup probe,
// even with no binary on PATH, and the target dir is never touched.
func TestRestore_ChainRefusesPITRTargetInWALGap(t *testing.T) {
	gap := backup.WALGap{
		SlotName: "s1", Timeline: 1,
		GapStartLSN: "0/4000000", GapEndLSN: "0/5000000",
		GapBytes: 1 << 20, DetectedAt: time.Now().UTC(),
	}
	fx := newChainFixtureWithLeafGaps(t, []backup.WALGap{gap})
	target := filepath.Join(t.TempDir(), "merged")
	t.Setenv("PATH", "")

	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   fx.inc.BackupID,
		TargetDir:  target,
		Verifier:   fx.verifier,
		Recovery:   &restore.Recovery{Enable: true, TargetLSN: "0/4800000"}, // inside the gap
	})
	if err == nil {
		t.Fatal("expected refusal: chain PITR target inside a WAL gap")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "restore.target_in_wal_gap" {
		t.Fatalf("expected restore.target_in_wal_gap (before the pg_combinebackup probe); got %v", err)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Errorf("target %q should not exist after a refused pre-flight", target)
	}
}

// TestRestore_ChainSkipGapCheckBypasses: --skip-gap-check honors the
// operator override on the chain path too — the gap-check is skipped
// and the restore proceeds (here it then trips the pg_combinebackup
// probe, proving the gap-check no longer blocked it).
func TestRestore_ChainSkipGapCheckBypasses(t *testing.T) {
	gap := backup.WALGap{
		SlotName: "s1", Timeline: 1,
		GapStartLSN: "0/4000000", GapEndLSN: "0/5000000",
		GapBytes: 1 << 20, DetectedAt: time.Now().UTC(),
	}
	fx := newChainFixtureWithLeafGaps(t, []backup.WALGap{gap})
	target := filepath.Join(t.TempDir(), "merged")
	t.Setenv("PATH", "")

	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   fx.inc.BackupID,
		TargetDir:  target,
		Verifier:   fx.verifier,
		Recovery:   &restore.Recovery{Enable: true, TargetLSN: "0/4800000", SkipGapCheck: true},
	})
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "preflight.pg_combinebackup_missing" {
		t.Fatalf("--skip-gap-check should bypass the gap-check and proceed to pg_combinebackup; got %v", err)
	}
}

// TestRestore_ChainBuildsCompleteChain: verifies that the chain
// walker resolves the leaf back to the full anchor with the
// expected order. This test bypasses the pg_combinebackup invocation
// (it requires the binary) and tests the chain-build directly via
// combine.Build, which is what restore.Restore calls internally.
func TestRestore_ChainBuildsCompleteChain(t *testing.T) {
	fx := newChainFixture(t)
	root := strings.TrimPrefix(fx.repoURL, "file://")
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	chain, err := combine.Build(context.Background(), sp, "db1", fx.inc.BackupID, fx.verifier)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("len(chain)=%d, want 2", len(chain))
	}
	if chain[0].BackupID != fx.full.BackupID {
		t.Errorf("anchor = %q, want %q", chain[0].BackupID, fx.full.BackupID)
	}
	if chain[1].BackupID != fx.inc.BackupID {
		t.Errorf("leaf = %q, want %q", chain[1].BackupID, fx.inc.BackupID)
	}
}

// TestRestore_ChainBlocksOnTombstonedAnchor: a tombstoned full +
// live incremental child must produce chain.broken_tombstoned at
// chain-build time. We exercise this through combine.Build directly
// (rather than restore.Restore) because the latter pre-flights
// pg_combinebackup BEFORE the chain build, and most CI runners
// don't have the binary installed.
//
// SoftDelete now refuses to tombstone a manifest with live
// descendants (the chain-protection in this same release); we
// bypass it via a direct tombstone-marker write to keep the
// defence-in-depth diagnostic covered.
func TestRestore_ChainBlocksOnTombstonedAnchor(t *testing.T) {
	fx := newChainFixture(t)
	root := strings.TrimPrefix(fx.repoURL, "file://")
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	tombstoneKey := backup.TombstonePath("db1", fx.full.BackupID)
	if _, err := sp.Put(context.Background(), tombstoneKey,
		strings.NewReader(`{"schema":"pg_hardstorage.tombstone.v1"}`),
		storage.PutOptions{}); err != nil {
		t.Fatalf("direct tombstone write: %v", err)
	}

	_, err := combine.Build(context.Background(), sp, "db1", fx.inc.BackupID, fx.verifier)
	if err == nil {
		t.Fatal("expected chain.broken_tombstoned")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "chain.broken_tombstoned" {
		t.Errorf("expected chain.broken_tombstoned, got %v", err)
	}
}

// TestRestore_ChainFailedCombineLeavesNoPartialTarget pins data-loss
// path #5: pg_combinebackup writes the merged datadir into a sibling
// staging dir that is renamed into place only on success. A mid-merge
// FAILURE (here: a fake pg_combinebackup that writes a partial datadir
// then exits non-zero) must therefore leave NO partial, plausible-
// looking datadir at the target — nothing an operator could mistakenly
// start PG against — and must clean up its staging dir.
func TestRestore_ChainFailedCombineLeavesNoPartialTarget(t *testing.T) {
	fx := newChainFixture(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "merged")

	// Fake pg_combinebackup: write a partial datadir into its -o dir,
	// then fail — simulating a crash/abort partway through the merge.
	fake := filepath.Join(dir, "pg_combinebackup")
	script := "#!/bin/sh\n" +
		"out=\"\"\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = \"-o\" ]; then out=\"$2\"; fi; shift; done\n" +
		"mkdir -p \"$out\"\n" +
		"echo 17 > \"$out/PG_VERSION\"\n" + // partial content written
		"exit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Discovery uses the leaf's PG version for the env-var name.
	t.Setenv(fmt.Sprintf("PG_COMBINEBACKUP_%d", fx.inc.PGVersion), fake)

	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   fx.inc.BackupID,
		TargetDir:  target,
		Verifier:   fx.verifier,
	})
	if err == nil {
		t.Fatal("expected chain restore to fail (fake pg_combinebackup exits 1)")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Errorf("target %q must NOT exist after a failed merge (no partial datadir)", target)
	}
	if _, statErr := os.Stat(target + ".pgcombine-staging"); statErr == nil {
		t.Errorf("combine staging dir must be cleaned up after a failed merge")
	}
}

// TestRestore_ChainSuccessfulCombineFinalizesTarget: the success path —
// a fake pg_combinebackup that writes a complete-looking datadir and
// exits 0 results in the merged output appearing atomically at the
// target (renamed from staging), and the staging dir is gone.
func TestRestore_ChainSuccessfulCombineFinalizesTarget(t *testing.T) {
	fx := newChainFixture(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "merged")

	fake := filepath.Join(dir, "pg_combinebackup")
	script := "#!/bin/sh\n" +
		"out=\"\"\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = \"-o\" ]; then out=\"$2\"; fi; shift; done\n" +
		"mkdir -p \"$out\"\n" +
		"echo 17 > \"$out/PG_VERSION\"\n" +
		"exit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fmt.Sprintf("PG_COMBINEBACKUP_%d", fx.inc.PGVersion), fake)

	if _, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   fx.inc.BackupID,
		TargetDir:  target,
		Verifier:   fx.verifier,
	}); err != nil {
		t.Fatalf("chain restore should succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "PG_VERSION")); err != nil {
		t.Errorf("merged datadir should be finalized at the target: %v", err)
	}
	if _, statErr := os.Stat(target + ".pgcombine-staging"); statErr == nil {
		t.Errorf("staging dir should be gone after a successful finalize")
	}
}
