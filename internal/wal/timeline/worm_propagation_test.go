package timeline_test

import (
	"context"
	"io"
	"iter"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

// recordingStorage wraps storage.StoragePlugin and captures every
// PutOptions. Used here to assert RetainUntil + RetentionMode
// propagation through timeline.Store.Put.
type recordingStorage struct {
	storage.NopBarrier // test fake: Inline-only, no deferred writes
	mu                 sync.Mutex
	inner              storage.StoragePlugin
	puts               []recordedPut
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
func (r *recordingStorage) Capabilities() storage.Capabilities { return r.inner.Capabilities() }
func (r *recordingStorage) Close() error                       { return r.inner.Close() }

func newRecording(t *testing.T) *recordingStorage {
	t.Helper()
	root := t.TempDir()
	inner := &fs.Plugin{}
	if err := inner.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatalf("fs open: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	return &recordingStorage{inner: inner}
}

// TestPut_PropagatesWORMRetention: timeline.NewWithRetention()
// applies the policy to the tmp Put. Verifies the v0.6+ WORM
// threading commitment for the timeline-history layer.
func TestPut_PropagatesWORMRetention(t *testing.T) {
	rec := newRecording(t)
	policy, _ := repo.MakeWORMPolicy("compliance", "7y")
	store := timeline.NewWithRetention(rec, policy)

	body := []byte("1\t0/15A2B388\tno recovery target\n")
	before := time.Now().UTC()
	if err := store.Put(context.Background(), "db1", 2, body); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Find the tmp Put that wrote our content. The tmp suffix is random
	// (per-writer, to avoid concurrent agents colliding on one tmp), so
	// match the prefix rather than an exact key.
	var found *recordedPut
	for i := range rec.puts {
		if strings.HasPrefix(rec.puts[i].Key, "wal/db1/timelines/2.history.tmp.") {
			p := rec.puts[i]
			found = &p
			break
		}
	}
	if found == nil {
		t.Fatalf("expected a Put at the tmp key; got: %+v", rec.puts)
	}
	if found.Opts.RetentionMode != storage.WORMMode("compliance") {
		t.Errorf("RetentionMode = %q, want compliance", found.Opts.RetentionMode)
	}
	// RetainUntil ~= before + 7y. Loose comparison (1 minute slack).
	wantMin := before.Add(7*365*24*time.Hour - time.Minute)
	wantMax := time.Now().UTC().Add(7*365*24*time.Hour + time.Minute)
	if found.Opts.RetainUntil.Before(wantMin) || found.Opts.RetainUntil.After(wantMax) {
		t.Errorf("RetainUntil = %s, want roughly now+7y", found.Opts.RetainUntil)
	}
}

// TestPut_NoWORM_ZeroRetention: timeline.New() (no WORM) keeps
// PutOptions free of retention. Regression guard so a future
// constructor refactor doesn't accidentally lock fs-backed dev
// repos under WORM-by-default.
func TestPut_NoWORM_ZeroRetention(t *testing.T) {
	rec := newRecording(t)
	store := timeline.New(rec)

	if err := store.Put(context.Background(), "db1", 2, []byte("body")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, p := range rec.puts {
		if !p.Opts.RetainUntil.IsZero() || p.Opts.RetentionMode != "" {
			t.Errorf("non-WORM Put picked up retention: %+v", p)
		}
	}
}

// TestPut_NilWORMPolicy_NoRetention: NewWithRetention(sp, nil) is
// a documented degenerate case (caller threads a nil policy
// through when the operator hasn't configured WORM). Must NOT
// apply retention.
func TestPut_NilWORMPolicy_NoRetention(t *testing.T) {
	rec := newRecording(t)
	store := timeline.NewWithRetention(rec, nil)

	if err := store.Put(context.Background(), "db1", 3, []byte("body")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, p := range rec.puts {
		if !p.Opts.RetainUntil.IsZero() {
			t.Errorf("nil-policy Put picked up retention: %+v", p)
		}
	}
}
