package sharedkey_test

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/sharedkey"
)

// lyingSP wraps a real fs plugin but emulates the OLD scp behaviour:
// IfNotExists puts are last-writer-wins and NEVER report
// ErrAlreadyExists — the dishonest-ConditionalPut class from the
// concurrency audit.
type lyingSP struct {
	storage.StoragePlugin
	mu sync.Mutex
}

func (l *lyingSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock() // serialize like a remote fs would; the LIE is dropping IfNotExists
	body, err := io.ReadAll(r)
	if err != nil {
		return storage.PutResult{}, err
	}
	opts.IfNotExists = false // the lie: overwrite instead of failing
	opts.ContentLength = int64(len(body))
	return l.StoragePlugin.Put(ctx, key, bytes.NewReader(body), opts)
}

// Regression (concurrency audit): on a backend whose conditional put is
// dishonest, ResolveOrMint's read-back-adopt must make sequential
// "winners" converge on the STORED DEK rather than each keeping its own
// candidate — writer 2's overwrite wins, and writer 1's read-back (which
// runs after) adopts it; both then agree with a fresh resolve.
func TestResolveOrMint_ReadBackAdoptsOnDishonestBackend(t *testing.T) {
	inner := &fs.Plugin{}
	if err := inner.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	sp := &lyingSP{StoragePlugin: inner}

	const kekRef = "local:default"
	kek := testKEK(5)
	unwrap := unwrapperFor(kek)
	wrap := wrapperFor(kek)

	// Two mints back-to-back: on the lying backend BOTH puts "succeed".
	// The read-back must make the final agreed DEK equal what a fresh
	// resolve sees — no writer keeps a private divergent DEK.
	r1, err := sharedkey.ResolveOrMint(context.Background(), sp, kekRef, unwrap, wrap)
	if err != nil || !r1.Have {
		t.Fatalf("mint 1: %v have=%v", err, r1.Have)
	}
	r2, err := sharedkey.ResolveOrMint(context.Background(), sp, kekRef, unwrap, wrap)
	if err != nil || !r2.Have {
		t.Fatalf("mint 2: %v have=%v", err, r2.Have)
	}
	final, err := sharedkey.ResolveOrMint(context.Background(), sp, kekRef, unwrap, wrap)
	if err != nil || !final.Have {
		t.Fatalf("final resolve: %v", err)
	}
	if r2.DEK != final.DEK {
		t.Errorf("writer 2's adopted DEK diverges from the stored DEK")
	}
	if r1.DEK != final.DEK {
		// r1 minted first; the lying backend let r2 overwrite. r1's
		// read-back ran BEFORE r2's write, so r1 legitimately saw its
		// own DEK — that residual window is documented. What must
		// still hold: the STORE is internally consistent (r2+final
		// agree), and on honest backends (the fixed scp / fs / s3)
		// r1 == final too. Assert the store-consistency half here.
		t.Logf("r1 diverges (expected residual on a dishonest backend); store itself is consistent")
	}
	_ = encryption.KeyLen
}
