package repo_test

import (
	"context"
	"errors"
	"io"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// recordingStorage wraps storage.StoragePlugin and captures every
// PutOptions call. Used to assert RetainUntil + RetentionMode
// propagation through the CAS / ManifestStore path.
//
// wormCapable overrides Capabilities().WORM — set true when the
// test is asserting WORM-propagation semantics on a backend (fs)
// that doesn't actually support WORM.  Without this override,
// NewCAS would mark the CAS retentionUnenforceable and PutChunk
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

// putsForKey returns every recorded Put for keys with the given prefix.
func (r *recordingStorage) putsForKey(prefix string) []recordedPut {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recordedPut
	for _, p := range r.puts {
		if len(p.Key) >= len(prefix) && p.Key[:len(prefix)] == prefix {
			out = append(out, p)
		}
	}
	return out
}

// TestCAS_WithRetention_PropagatesToPut: a CAS constructed via
// casdefault.NewWithRetention propagates RetainUntil +
// RetentionMode on every PutChunk.
func TestCAS_WithRetention_PropagatesToPut(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	rec := &recordingStorage{inner: sp, wormCapable: true}

	policy, _ := repo.MakeWORMPolicy("compliance", "7y")
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	cas := casdefault.NewWithRetention(rec, policy, now)

	if _, err := cas.PutChunk(context.Background(), []byte("payload-bytes")); err != nil {
		t.Fatalf("PutChunk: %v", err)
	}
	puts := rec.putsForKey("chunks/sha256/")
	if len(puts) != 1 {
		t.Fatalf("expected 1 chunk Put; got %d", len(puts))
	}
	want := policy.RetainUntil(now)
	if !puts[0].Opts.RetainUntil.Equal(want) {
		t.Errorf("RetainUntil = %s, want %s", puts[0].Opts.RetainUntil, want)
	}
	if puts[0].Opts.RetentionMode != storage.WORMMode("compliance") {
		t.Errorf("RetentionMode = %q, want compliance", puts[0].Opts.RetentionMode)
	}
}

// TestCAS_NoRetention_NoPropagation: a CAS without WithRetention
// (the default casdefault.New path) does NOT set RetainUntil.
func TestCAS_NoRetention_NoPropagation(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	rec := &recordingStorage{inner: sp}

	cas := casdefault.New(rec)
	if _, err := cas.PutChunk(context.Background(), []byte("plain")); err != nil {
		t.Fatalf("PutChunk: %v", err)
	}
	puts := rec.putsForKey("chunks/sha256/")
	if len(puts) != 1 {
		t.Fatalf("expected 1 Put; got %d", len(puts))
	}
	if !puts[0].Opts.RetainUntil.IsZero() {
		t.Errorf("RetainUntil should be zero; got %s", puts[0].Opts.RetainUntil)
	}
	if puts[0].Opts.RetentionMode != "" {
		t.Errorf("RetentionMode should be empty; got %q", puts[0].Opts.RetentionMode)
	}
}

// TestCAS_NewWithRetention_NilPolicyFallsBackToPlain: when the
// policy is nil/zero, NewWithRetention is equivalent to New.
func TestCAS_NewWithRetention_NilPolicyFallsBackToPlain(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	rec := &recordingStorage{inner: sp}

	now := time.Now()
	cas := casdefault.NewWithRetention(rec, nil, now)
	if _, err := cas.PutChunk(context.Background(), []byte("p")); err != nil {
		t.Fatalf("PutChunk: %v", err)
	}
	puts := rec.putsForKey("chunks/sha256/")
	if len(puts) != 1 || !puts[0].Opts.RetainUntil.IsZero() {
		t.Errorf("nil policy should not propagate retention; got %+v", puts)
	}
}

// TestCAS_RetentionUnenforceable_RefusesPut: when retention is
// configured but the underlying backend doesn't support WORM, the
// CAS refuses PutChunk with ErrRetentionUnenforceable rather than
// silently writing chunks the operator believes are protected.
// Audit v23 corner case #9.
func TestCAS_RetentionUnenforceable_RefusesPut(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	// Note: NO wormCapable=true here — fs is the real, non-WORM
	// backend; the CAS should detect this and mark itself
	// unenforceable.
	rec := &recordingStorage{inner: sp}

	policy, _ := repo.MakeWORMPolicy("compliance", "7y")
	cas := casdefault.NewWithRetention(rec, policy, time.Now())

	_, err := cas.PutChunk(context.Background(), []byte("payload"))
	if err == nil {
		t.Fatal("expected refusal when WORM is unsupported but retention configured")
	}
	if !errors.Is(err, repo.ErrRetentionUnenforceable) {
		t.Fatalf("err = %v, want ErrRetentionUnenforceable", err)
	}
	// No chunk should have been written.
	puts := rec.putsForKey("chunks/sha256/")
	if len(puts) != 0 {
		t.Fatalf("expected 0 puts on refusal; got %d", len(puts))
	}
}

// TestCAS_RetentionUnenforceable_OptOut: an operator who knowingly
// runs a WORM-config'd CAS against a non-WORM backend (test/dev
// repos) opts out via WithRetentionAllowUnenforced.  The CAS then
// behaves as before — propagating the retention fields, which the
// backend silently ignores.
func TestCAS_RetentionUnenforceable_OptOut(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	rec := &recordingStorage{inner: sp}

	policy, _ := repo.MakeWORMPolicy("compliance", "7y")
	now := time.Now()
	cas := repo.NewCAS(rec,
		repo.WithRetention(repo.CASRetention{
			RetainUntil: policy.RetainUntil(now),
			Mode:        storage.WORMCompliance,
		}),
		repo.WithRetentionAllowUnenforced(),
	)
	if _, err := cas.PutChunk(context.Background(), []byte("payload")); err != nil {
		t.Fatalf("opt-out: unexpected refusal: %v", err)
	}
	puts := rec.putsForKey("chunks/sha256/")
	if len(puts) != 1 {
		t.Fatalf("expected 1 chunk Put under opt-out; got %d", len(puts))
	}
}

// TestCAS_GovernanceMode_PropagatesAsGovernance: governance mode
// flows through to PutOptions.RetentionMode unchanged.
func TestCAS_GovernanceMode_PropagatesAsGovernance(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	rec := &recordingStorage{inner: sp, wormCapable: true}

	policy, _ := repo.MakeWORMPolicy("governance", "30d")
	cas := casdefault.NewWithRetention(rec, policy, time.Now())
	if _, err := cas.PutChunk(context.Background(), []byte("g")); err != nil {
		t.Fatal(err)
	}
	puts := rec.putsForKey("chunks/sha256/")
	if len(puts) != 1 {
		t.Fatal("expected 1 Put")
	}
	if puts[0].Opts.RetentionMode != storage.WORMMode("governance") {
		t.Errorf("RetentionMode = %q, want governance", puts[0].Opts.RetentionMode)
	}
}
