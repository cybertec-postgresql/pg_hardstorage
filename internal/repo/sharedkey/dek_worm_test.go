package sharedkey_test

// dek_worm_test.go — the shared DEK must be Object-Locked on a retention
// repo, and its lock must be EXTENDED on every resolve.
//
// The shared DEK decrypts every encrypted chunk in the repo. On a WORM
// repo the chunks, manifests, WAL, and timeline history are all
// immutable — but if this one tiny object is deletable, deleting it
// makes ALL encrypted data permanently unrecoverable. And because the
// DEK is minted once while backups written later are locked longer, a
// fixed mint-time lock would expire before those backups; so each
// resolve extends it to now+term.

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/sharedkey"
)

type recSP struct {
	storage.StoragePlugin
	mu      sync.Mutex
	puts    map[string]storage.PutOptions
	extends map[string]time.Time
}

func (r *recSP) Put(ctx context.Context, key string, src io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	r.mu.Lock()
	if r.puts == nil {
		r.puts = map[string]storage.PutOptions{}
	}
	r.puts[key] = opts
	r.mu.Unlock()
	return r.StoragePlugin.Put(ctx, key, src, opts)
}

func (r *recSP) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	r.mu.Lock()
	if r.extends == nil {
		r.extends = map[string]time.Time{}
	}
	r.extends[key] = until
	r.mu.Unlock()
	return r.StoragePlugin.SetRetention(ctx, key, until, mode)
}

func (r *recSP) Capabilities() storage.Capabilities {
	c := r.StoragePlugin.Capabilities()
	c.WORM = true
	return c
}

func TestResolveOrMint_WORMLocksAndExtendsSharedDEK(t *testing.T) {
	rec := &recSP{StoragePlugin: newSP(t)}
	kek := testKEK(9)
	unwrap := unwrapperFor(kek)
	wrap := wrapperFor(kek)
	const kekRef = "local:default"

	t1 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	// Mint: the shared-DEK object must be written with a retention deadline.
	if _, err := sharedkey.ResolveOrMint(context.Background(), rec, kekRef, unwrap, wrap, t1, "compliance"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	var dekKey string
	rec.mu.Lock()
	for k := range rec.puts {
		if strings.HasPrefix(k, "keys/shared-dek/") {
			dekKey = k
		}
	}
	opts := rec.puts[dekKey]
	rec.mu.Unlock()
	if dekKey == "" {
		t.Fatal("no shared-DEK object was written")
	}
	if opts.RetainUntil.IsZero() {
		t.Fatal("shared DEK minted WITHOUT a retention deadline on a WORM repo — deleting this one " +
			"object would make ALL encrypted data unrecoverable")
	}
	if !opts.RetainUntil.Equal(t1) || opts.RetentionMode != storage.WORMMode("compliance") {
		t.Errorf("mint retention = %s/%s, want %s/compliance", opts.RetainUntil, opts.RetentionMode, t1)
	}

	// Resolve later (a backup written months on): the DEK's lock must be
	// EXTENDED to the newer deadline so it keeps outliving the newest backup.
	t2 := t1.Add(365 * 24 * time.Hour)
	if _, err := sharedkey.ResolveOrMint(context.Background(), rec, kekRef, unwrap, wrap, t2, "compliance"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rec.mu.Lock()
	extended, ok := rec.extends[dekKey]
	rec.mu.Unlock()
	if !ok || !extended.Equal(t2) {
		t.Fatalf("shared DEK lock was NOT extended on resolve (got %v, want %s) — a long-lived repo's "+
			"DEK would expire before the backups it decrypts", extended, t2)
	}
}
