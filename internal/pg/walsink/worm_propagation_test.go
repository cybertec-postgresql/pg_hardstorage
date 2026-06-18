package walsink_test

import (
	"context"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/jackc/pglogrepl"
)

// recordingStorage wraps storage.StoragePlugin and captures every
// PutOptions. Used to assert that the v0.6+ WORM threading lands
// in the storage backend on every committed key.
//
// wormCapable overrides Capabilities().WORM so the test can mock
// a WORM-capable backend on top of fs (which doesn't support
// WORM).  Without this override, NewCAS would mark the CAS
// retentionUnenforceable per the v23 audit fix and PutChunk
// would refuse, defeating the test's intent.
type recordingStorage struct {
	storage.NopBarrier // test fake: Inline-only, no deferred writes
	mu                 sync.Mutex
	inner              storage.StoragePlugin
	puts               []recordedPut
	wormCapable        bool
}

type recordedPut struct {
	Key  string
	Opts storage.PutOptions
}

func (r *recordingStorage) Name() string { return r.inner.Name() }
func (r *recordingStorage) Open(ctx context.Context, cfg storage.StorageConfig) error {
	return r.inner.Open(ctx, cfg)
}
func (r *recordingStorage) Put(ctx context.Context, key string, src io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	r.mu.Lock()
	r.puts = append(r.puts, recordedPut{Key: key, Opts: opts})
	r.mu.Unlock()
	return r.inner.Put(ctx, key, src, opts)
}
func (r *recordingStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return r.inner.Get(ctx, key)
}
func (r *recordingStorage) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	return r.inner.Stat(ctx, key)
}
func (r *recordingStorage) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return r.inner.List(ctx, prefix)
}
func (r *recordingStorage) Delete(ctx context.Context, key string) error {
	return r.inner.Delete(ctx, key)
}
func (r *recordingStorage) RenameIfNotExists(ctx context.Context, src, dst string) error {
	return r.inner.RenameIfNotExists(ctx, src, dst)
}
func (r *recordingStorage) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	return r.inner.SetRetention(ctx, key, until, mode)
}
func (r *recordingStorage) Capabilities() storage.Capabilities {
	caps := r.inner.Capabilities()
	if r.wormCapable {
		caps.WORM = true
	}
	return caps
}
func (r *recordingStorage) Close() error { return r.inner.Close() }

// TestPushSegmentFile_PropagatesWORMRetention: with an explicit
// WORM policy, every Put issued by PushSegmentFile (chunk Puts via
// the CAS + the segment-manifest tmp Put) carries a non-zero
// RetainUntil + the configured mode. This exercises the agent-
// archive path's WORM commitment for v0.6+.
func TestPushSegmentFile_PropagatesWORMRetention(t *testing.T) {
	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	sp, _ := openFSRepo(t, repoURL)
	defer sp.Close()

	rec := &recordingStorage{inner: sp, wormCapable: true}
	policy, _ := repo.MakeWORMPolicy("compliance", "30d")
	wormNow := time.Now().UTC()
	cas := casdefault.NewWithRetention(rec, policy, wormNow)

	// Build a synthetic 16 MiB segment file.
	segmentName := "000000010000000000000005"
	segPath := filepath.Join(t.TempDir(), segmentName)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte(i % 256)
	}
	if err := os.WriteFile(segPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := walsink.PushSegmentFile(context.Background(), cas, rec, segPath, walsink.PushOptions{
		Deployment:       "db1",
		SystemIdentifier: "7000000000000000001",
		WORM:             policy,
	}); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Every recorded Put must carry RetainUntil + Mode.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.puts) == 0 {
		t.Fatal("expected at least one Put")
	}
	for _, p := range rec.puts {
		if p.Opts.RetainUntil.IsZero() {
			t.Errorf("Put %q has zero RetainUntil despite WORM policy", p.Key)
		}
		if p.Opts.RetentionMode != storage.WORMMode("compliance") {
			t.Errorf("Put %q RetentionMode = %q, want compliance", p.Key, p.Opts.RetentionMode)
		}
	}

	// Sanity: a manifest tmp must be among the recorded Puts so we
	// know the manifest path (not just chunks) carries WORM too.
	var sawManifestTmp bool
	for _, p := range rec.puts {
		if strings.Contains(p.Key, "wal/db1/") && strings.Contains(p.Key, ".json.tmp.") {
			sawManifestTmp = true
			break
		}
	}
	if !sawManifestTmp {
		t.Error("expected a wal manifest tmp Put among the recorded Puts; got keys:")
		for _, p := range rec.puts {
			t.Errorf("  %s", p.Key)
		}
	}
}

// TestPushSegmentFile_NoWORM_ZeroRetention: without a WORM policy
// the manifest Put MUST NOT carry retention. Defensive regression
// guard so a future refactor doesn't accidentally lock fs-backed
// dev repos under WORM.
func TestPushSegmentFile_NoWORM_ZeroRetention(t *testing.T) {
	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	sp, _ := openFSRepo(t, repoURL)
	defer sp.Close()

	rec := &recordingStorage{inner: sp}
	cas := casdefault.New(rec)

	segmentName := "000000010000000000000006"
	segPath := filepath.Join(t.TempDir(), segmentName)
	body := make([]byte, walsink.SegmentSize)
	if err := os.WriteFile(segPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := walsink.PushSegmentFile(context.Background(), cas, rec, segPath, walsink.PushOptions{
		Deployment:       "db1",
		SystemIdentifier: "7000000000000000001",
		// WORM intentionally nil.
	}); err != nil {
		t.Fatalf("push: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, p := range rec.puts {
		if !p.Opts.RetainUntil.IsZero() || p.Opts.RetentionMode != "" {
			t.Errorf("non-WORM Put %q picked up retention: %+v", p.Key, p.Opts)
		}
	}
}

// TestSink_PropagatesWORMRetentionToManifest is the streaming-path regression
// for the WORM bypass: the wal-stream Sink, given a WORM policy, must lock the
// segment MANIFEST as well as the chunks. The CAS already locks chunks; if the
// manifest Put omits RetainUntil (the bug: runWalStream built Options without
// WORM), the manifest can be deleted before its deadline, stranding the
// WORM-locked chunks and making the streamed segment unrecoverable — WORM
// bypassed for `wal stream`.
func TestSink_PropagatesWORMRetentionToManifest(t *testing.T) {
	repoDir := t.TempDir()
	sp, _ := openFSRepo(t, "file://"+repoDir)
	defer sp.Close()

	rec := &recordingStorage{inner: sp, wormCapable: true}
	policy, _ := repo.MakeWORMPolicy("compliance", "30d")
	cas := casdefault.NewWithRetention(rec, policy, time.Now().UTC())

	sink, err := walsink.New(cas, rec, walsink.Options{
		Deployment:       "db1",
		Timeline:         1,
		SystemIdentifier: "7000000000000000001",
		WORM:             policy,
	})
	if err != nil {
		t.Fatalf("walsink.New: %v", err)
	}

	// Feed exactly one full 16 MiB segment, then close to flush + commit it.
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte(i % 256)
	}
	if err := sink.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(0),
		Data:     body,
	}); err != nil {
		t.Fatalf("OnRecord: %v", err)
	}
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.puts) == 0 {
		t.Fatal("expected Puts from the streamed segment")
	}
	var sawManifest bool
	for _, p := range rec.puts {
		if p.Opts.RetainUntil.IsZero() {
			t.Errorf("streamed Put %q has zero RetainUntil despite WORM policy", p.Key)
		}
		if p.Opts.RetentionMode != storage.WORMMode("compliance") {
			t.Errorf("streamed Put %q RetentionMode = %q, want compliance", p.Key, p.Opts.RetentionMode)
		}
		if strings.Contains(p.Key, "wal/db1/") && strings.Contains(p.Key, ".json.tmp.") {
			sawManifest = true
		}
	}
	if !sawManifest {
		t.Error("expected a streamed wal-manifest tmp Put carrying WORM")
	}
}
