package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdfs "io/fs"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// countStagingDeferred walks the plugin's backing tree directly and
// counts ".deferred-" staging temps. List intentionally hides
// fs-internal staging files, so durability internals are inspected via
// the filesystem.
func countStagingDeferred(t *testing.T, p *Plugin) int {
	t.Helper()
	n := 0
	if err := filepath.WalkDir(p.root, func(_ string, d stdfs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepathHasDeferred(d.Name()) {
			n++
		}
		return nil
	}); err != nil {
		t.Fatalf("walk staging: %v", err)
	}
	return n
}

// openTestPlugin returns an fs.Plugin rooted at a fresh temp dir.
func openTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := &Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	return p
}

func getBytes(t *testing.T, p *Plugin, key string) []byte {
	t.Helper()
	rc, err := p.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return b
}

// Capabilities must advertise the durability contract: fs defers via
// Barrier (DurabilityBarrier) and is NOT inline-durable.
func TestDurability_Capabilities(t *testing.T) {
	c := (&Plugin{}).Capabilities()
	if !c.DurabilityBarrier {
		t.Error("fs must advertise DurabilityBarrier=true")
	}
	if c.InlineDurable {
		t.Error("fs must advertise InlineDurable=false (page cache until fsync)")
	}
}

// THE crash-safety property: a deferred Put must NOT publish a file
// at its final content key until Barrier. Before the Barrier the key
// is absent — only a ".deferred-*" staging temp exists — so a crash
// can never leave a truncated file at a real key that a later run's
// O_EXCL dedup would mistake for valid. After Barrier the key is live.
func TestDurability_DeferredInvisibleUntilBarrier(t *testing.T) {
	p := openTestPlugin(t)
	ctx := context.Background()
	body := []byte("crash-safe-payload")

	if _, err := p.PutBytes(ctx, "chunks/x", body, storage.PutOptions{
		IfNotExists: true,
		Durability:  storage.DurabilityDeferred,
	}); err != nil {
		t.Fatalf("deferred put: %v", err)
	}

	// Before Barrier: the final key must not exist.
	if _, err := p.Get(ctx, "chunks/x"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("before barrier: Get err = %v, want ErrNotFound (no file at the final key)", err)
	}
	// Exactly one staging temp must exist on disk (List hides
	// fs-internal staging files, so inspect the tree directly).
	if staged := countStagingDeferred(t, p); staged != 1 {
		t.Errorf("before barrier: %d staging temps, want 1", staged)
	}
	// And List must NOT surface the staging temp as a key.
	for obj, err := range p.List(ctx, "") {
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if filepathHasDeferred(obj.Key) {
			t.Errorf("List leaked a staging temp as a key: %s", obj.Key)
		}
	}

	// After Barrier: the chunk is published at its key.
	if err := p.Barrier(ctx); err != nil {
		t.Fatalf("barrier: %v", err)
	}
	if got := getBytes(t, p, "chunks/x"); string(got) != string(body) {
		t.Errorf("after barrier: got %q, want %q", got, body)
	}
}

func filepathHasDeferred(key string) bool {
	for i := 0; i+10 <= len(key); i++ {
		if key[i:i+10] == ".deferred-" {
			return true
		}
	}
	return false
}

// Many deferred writes followed by one Barrier: every object is
// durable and reads back correctly — the batch-then-barrier shape
// the base backup and WAL streamer rely on.
func TestDurability_ManyDeferredThenBarrier(t *testing.T) {
	p := openTestPlugin(t)
	ctx := context.Background()
	const n = 64
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("chunks/aa/bb/c%03d", i)
		if _, err := p.PutBytes(ctx, key, []byte(key), storage.PutOptions{
			IfNotExists: true,
			Durability:  storage.DurabilityDeferred,
		}); err != nil {
			t.Fatalf("deferred put %d: %v", i, err)
		}
	}
	if err := p.Barrier(ctx); err != nil {
		t.Fatalf("barrier: %v", err)
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("chunks/aa/bb/c%03d", i)
		if got := getBytes(t, p, key); string(got) != key {
			t.Errorf("object %d: got %q, want %q", i, got, key)
		}
	}
}

// Barrier with nothing outstanding is a safe, cheap no-op.
func TestDurability_BarrierEmptyIsNoop(t *testing.T) {
	p := openTestPlugin(t)
	if err := p.Barrier(context.Background()); err != nil {
		t.Errorf("empty barrier: %v", err)
	}
}

// An inline Put (the zero-value default) is durable and visible on
// its own; a following Barrier is a harmless no-op.
func TestDurability_InlineUnaffected(t *testing.T) {
	p := openTestPlugin(t)
	ctx := context.Background()
	body := []byte("inline-body")
	if _, err := p.PutBytes(ctx, "a/inline", body, storage.PutOptions{}); err != nil {
		t.Fatalf("inline put: %v", err)
	}
	// Inline is visible immediately — no Barrier needed.
	if got := getBytes(t, p, "a/inline"); string(got) != string(body) {
		t.Errorf("got %q, want %q", got, body)
	}
	if err := p.Barrier(ctx); err != nil {
		t.Errorf("barrier after inline put: %v", err)
	}
}

// The safety invariant: a Barrier that returns early (cancelled ctx)
// must not leave deferred writes permanently unpublished — a retried
// Barrier on a live context still publishes + durably commits them.
func TestDurability_BarrierCancelThenRetry(t *testing.T) {
	p := openTestPlugin(t)
	body := []byte("survives-cancel")
	if _, err := p.PutBytes(context.Background(), "chunks/y", body, storage.PutOptions{
		IfNotExists: true,
		Durability:  storage.DurabilityDeferred,
	}); err != nil {
		t.Fatalf("deferred put: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Barrier(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled barrier: err = %v, want context.Canceled", err)
	}
	if err := p.Barrier(context.Background()); err != nil {
		t.Fatalf("retry barrier: %v", err)
	}
	if got := getBytes(t, p, "chunks/y"); string(got) != string(body) {
		t.Errorf("after retry barrier: got %q, want %q", got, body)
	}
}

// A deferred Put of a key already published by a prior Barrier is a
// dedup hit — ErrAlreadyExists — without staging a redundant copy.
func TestDurability_DeferredDedupAgainstPublished(t *testing.T) {
	p := openTestPlugin(t)
	ctx := context.Background()
	if _, err := p.PutBytes(ctx, "chunks/z", []byte("first"), storage.PutOptions{
		IfNotExists: true,
		Durability:  storage.DurabilityDeferred,
	}); err != nil {
		t.Fatalf("first deferred put: %v", err)
	}
	if err := p.Barrier(ctx); err != nil {
		t.Fatalf("barrier: %v", err)
	}
	// chunks/z is now published; a second deferred Put must dedup.
	_, err := p.PutBytes(ctx, "chunks/z", []byte("first"), storage.PutOptions{
		IfNotExists: true,
		Durability:  storage.DurabilityDeferred,
	})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("deferred put of published key: err = %v, want ErrAlreadyExists", err)
	}
}
