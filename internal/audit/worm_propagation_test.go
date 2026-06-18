package audit_test

import (
	"context"
	"io"
	"iter"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// recordingStorage wraps storage.StoragePlugin and captures every
// PutOptions for assertion. Matches the shape used in
// internal/repo/worm_propagation_test.go and
// internal/wal/timeline/worm_propagation_test.go.
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

// putsFor returns every recorded Put whose key starts with prefix.
func (r *recordingStorage) putsFor(prefix string) []recordedPut {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recordedPut
	for _, p := range r.puts {
		if strings.HasPrefix(p.Key, prefix) {
			out = append(out, p)
		}
	}
	return out
}

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

// TestStore_Append_PropagatesWORMRetention: with a WORM policy
// configured on the Store, every committed event Put carries
// RetainUntil + the configured mode. Locks the v0.6+ commitment
// that audit events become non-rewritable when the repo's
// retention posture demands it.
func TestStore_Append_PropagatesWORMRetention(t *testing.T) {
	rec := newRecording(t)
	policy, _ := repo.MakeWORMPolicy("compliance", "7y")
	store := audit.NewStoreWithRetention(rec, policy)

	ev := &audit.Event{Action: "backup.create"}
	if err := store.Append(context.Background(), ev); err != nil {
		t.Fatalf("Append: %v", err)
	}

	eventPuts := rec.putsFor("audit/")
	// Filter out HeadKey writes (which deliberately omit retention).
	var bodyPuts []recordedPut
	for _, p := range eventPuts {
		if p.Key == audit.HeadKey {
			continue
		}
		bodyPuts = append(bodyPuts, p)
	}
	if len(bodyPuts) == 0 {
		t.Fatal("expected at least one event-body Put")
	}
	for _, p := range bodyPuts {
		if p.Opts.RetainUntil.IsZero() {
			t.Errorf("event Put %q has zero RetainUntil despite WORM policy", p.Key)
		}
		if p.Opts.RetentionMode != storage.WORMMode("compliance") {
			t.Errorf("event Put %q RetentionMode = %q, want compliance", p.Key, p.Opts.RetentionMode)
		}
	}
}

// TestStore_Append_HeadPointerNotLocked: the HeadKey pointer Put
// is rewritten on every Append. WORM would refuse the rewrite —
// so by design we DON'T set retention on the head pointer. The
// head is regenerable from the event listing; locking it would
// block every Append.
//
// This test pins that invariant so a future refactor doesn't
// break audit chain progress in a WORM-locked repo.
func TestStore_Append_HeadPointerNotLocked(t *testing.T) {
	rec := newRecording(t)
	policy, _ := repo.MakeWORMPolicy("compliance", "7y")
	store := audit.NewStoreWithRetention(rec, policy)

	for i := 0; i < 3; i++ {
		ev := &audit.Event{Action: "backup.create"}
		if err := store.Append(context.Background(), ev); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	headPuts := rec.putsFor(audit.HeadKey)
	if len(headPuts) == 0 {
		t.Fatal("expected head-pointer Puts")
	}
	for _, p := range headPuts {
		if !p.Opts.RetainUntil.IsZero() {
			t.Errorf("HeadKey Put picked up retention (would block rewrites): %+v", p.Opts)
		}
		if p.Opts.RetentionMode != "" {
			t.Errorf("HeadKey Put has non-empty RetentionMode: %q", p.Opts.RetentionMode)
		}
	}
}

// TestStore_Append_NoWORM_ZeroRetention: NewStore (no WORM)
// keeps PutOptions free of retention. Regression guard.
func TestStore_Append_NoWORM_ZeroRetention(t *testing.T) {
	rec := newRecording(t)
	store := audit.NewStore(rec)

	ev := &audit.Event{Action: "backup.create"}
	if err := store.Append(context.Background(), ev); err != nil {
		t.Fatalf("Append: %v", err)
	}

	for _, p := range rec.putsFor("audit/") {
		if !p.Opts.RetainUntil.IsZero() || p.Opts.RetentionMode != "" {
			t.Errorf("non-WORM Put %q picked up retention: %+v", p.Key, p.Opts)
		}
	}
}

// TestTransparencyLog_PutAnchor_PropagatesWORMRetention: anchor
// publication is the long-lived attestation surface; under WORM
// the published anchor must be non-rewritable.
func TestTransparencyLog_PutAnchor_PropagatesWORMRetention(t *testing.T) {
	rec := newRecording(t)
	policy, _ := repo.MakeWORMPolicy("compliance", "7y")
	log := audit.NewStorageBackedLogWithRetention(rec, policy)

	a := audit.Anchor{
		ChainHeadHash: strings.Repeat("ab", 32),
		HeadSequence:  1,
		AnchoredAt:    time.Now().UTC(),
		PublisherID:   "test",
	}
	if _, err := log.PutAnchor(context.Background(), a); err != nil {
		t.Fatalf("PutAnchor: %v", err)
	}

	puts := rec.putsFor(audit.AnchorPrefix)
	if len(puts) != 1 {
		t.Fatalf("expected 1 anchor Put; got %d", len(puts))
	}
	if puts[0].Opts.RetainUntil.IsZero() {
		t.Errorf("anchor Put has zero RetainUntil despite WORM policy")
	}
	if puts[0].Opts.RetentionMode != storage.WORMMode("compliance") {
		t.Errorf("anchor Put RetentionMode = %q, want compliance", puts[0].Opts.RetentionMode)
	}
}
