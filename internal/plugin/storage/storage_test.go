package storage_test

import (
	"context"
	"fmt"
	stdio "io"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// fake is a minimal in-memory plugin used to test the registry and
// the URL-scheme dispatcher without depending on a concrete backend.
type fake struct {
	storage.NopBarrier
	name   string
	opened bool
}

func (f *fake) Name() string                                          { return f.name }
func (f *fake) Open(_ context.Context, _ storage.StorageConfig) error { f.opened = true; return nil }
func (f *fake) Close() error                                          { return nil }
func (f *fake) Capabilities() storage.Capabilities                    { return storage.Capabilities{} }
func (f *fake) Put(context.Context, string, stdio.Reader, storage.PutOptions) (storage.PutResult, error) {
	return storage.PutResult{}, nil
}
func (f *fake) Get(context.Context, string) (stdio.ReadCloser, error) {
	return nil, storage.ErrNotFound
}
func (f *fake) Stat(context.Context, string) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotFound
}
func (f *fake) Delete(context.Context, string) error                    { return nil }
func (f *fake) RenameIfNotExists(context.Context, string, string) error { return nil }
func (f *fake) SetRetention(context.Context, string, time.Time, storage.WORMMode) error {
	return storage.ErrUnsupported
}
func (f *fake) List(context.Context, string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {}
}

var dupRegCounter atomic.Int64

func TestRegister_DuplicatePanics(t *testing.T) {
	// Unique name per invocation: the scheme registry is process-global
	// and persists across `-count=N` iterations, so a fixed name would
	// make the FIRST Register panic (already registered) on the second
	// run, before the recover() below is in scope.
	name := fmt.Sprintf("fake-dup-%d", dupRegCounter.Add(1))
	storage.Register(name, func() storage.StoragePlugin { return &fake{name: "fake"} })
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	storage.Register(name, func() storage.StoragePlugin { return &fake{name: "fake"} })
}

func TestOpen_UnknownScheme(t *testing.T) {
	_, err := storage.Open(context.Background(), "weird://nope")
	if err == nil {
		t.Fatal("expected error for unknown scheme")
	}
	if !strings.Contains(err.Error(), "no plugin registered") {
		t.Errorf("error should describe the scheme registry: %v", err)
	}
}

func TestOpen_KnownScheme(t *testing.T) {
	// Unique scheme per run (the registry is process-global; see
	// TestRegister_DuplicatePanics).
	scheme := fmt.Sprintf("fake-known-%d", dupRegCounter.Add(1))
	storage.Register(scheme, func() storage.StoragePlugin { return &fake{name: "fake-known"} })
	p, err := storage.Open(context.Background(), scheme+"://anywhere")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Name() != "fake-known" {
		t.Errorf("name = %q", p.Name())
	}
	if !p.(*fake).opened {
		t.Error("Open should have been called")
	}
}

func TestOpen_BareAbsolutePathBecomesFileScheme(t *testing.T) {
	storage.Register(fmt.Sprintf("file-test-%d", dupRegCounter.Add(1)),
		func() storage.StoragePlugin { return &fake{name: "fs"} })
	// We can't intercept the exact URL the plugin sees without exposing it;
	// just verify a bare path doesn't error out as "no scheme" when the
	// "file" scheme handler exists. Use a unique name to avoid colliding
	// with the real fs plugin's "file" registration in other test files.
	_, err := storage.Open(context.Background(), "/tmp/somewhere")
	if err == nil {
		t.Skip("file scheme is registered globally; bare path test inconclusive in this binary")
		return
	}
	// A real "file" registration will land later; for now we expect "no
	// plugin registered for scheme \"file\"" or success if fs is loaded.
	if !strings.Contains(err.Error(), "file") {
		t.Errorf("bare path should be canonicalized to file://; err=%v", err)
	}
}

func TestOpen_EmptyURL(t *testing.T) {
	if _, err := storage.Open(context.Background(), ""); err == nil {
		t.Error("empty URL should error")
	}
}

func TestSchemes_Sorted(t *testing.T) {
	all := storage.Schemes()
	for i := 1; i < len(all); i++ {
		if all[i-1] > all[i] {
			t.Errorf("Schemes() not sorted: %v", all)
			break
		}
	}
}
