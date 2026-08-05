package storage_test

// commit_notimplemented_test.go — the reporter's backend, without
// needing their bucket.
//
// Issue #45 came from an S3-compatible store that implements a
// conditional PUT and answers the conditional COPY with:
//
//	NotImplemented … Copy object not implemented with If-None-Match
//
// That combination is the whole bug: the staging commit path needs the
// COPY, so no manifest could ever commit, even though the single
// conditional PUT the code now prefers would have worked on the first
// try. Reproducing it with a fake is worth more than a note in an
// issue — it turns "we believe this fixes their case" into something
// the suite asserts.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// notImplementedCopySP behaves like the reported store: conditional PUT
// works, conditional COPY does not exist.
type notImplementedCopySP struct {
	storage.StoragePlugin
	advertiseCondPut bool
}

func (n *notImplementedCopySP) Capabilities() storage.Capabilities {
	c := n.StoragePlugin.Capabilities()
	c.ConditionalPut = n.advertiseCondPut
	return c
}

func (n *notImplementedCopySP) RenameIfNotExists(ctx context.Context, src, dst string) error {
	return fmt.Errorf("s3: copy %q -> %q: NotImplemented: Copy object not implemented "+
		"with If-None-Match", src, dst)
}

func newNotImplementedCopySP(t *testing.T, advertiseCondPut bool) *notImplementedCopySP {
	t.Helper()
	p := &fs.Plugin{}
	u, err := url.Parse("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("open fs: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return &notImplementedCopySP{StoragePlugin: p, advertiseCondPut: advertiseCondPut}
}

// TestCommitExclusive_WorksOnStoreWithoutConditionalCopy is the
// reported configuration, fixed.
//
// The store vouches for its conditional PUT (?conditional_put=native),
// so the commit never reaches the COPY that would fail.
func TestCommitExclusive_WorksOnStoreWithoutConditionalCopy(t *testing.T) {
	sp := newNotImplementedCopySP(t, true)

	if err := storage.CommitExclusive(context.Background(), sp, "wal/db1/seg.json",
		[]byte(`{"schema":"segment"}`), storage.PutOptions{}); err != nil {
		t.Fatalf("commit failed on a store that implements conditional PUT but not "+
			"conditional COPY: %v\n\nThis is the configuration in issue #45: base backups "+
			"failed outright and wal stream buffered until it was OOM-killed", err)
	}

	// And the exclusion still holds without the COPY.
	err := storage.CommitExclusive(context.Background(), sp, "wal/db1/seg.json",
		[]byte("other"), storage.PutOptions{})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("second commit err = %v, want ErrAlreadyExists", err)
	}
}

// TestCommitExclusive_UnvouchedStoreStillFailsHonestly pins the other
// half, which an operator needs to understand.
//
// Without ?conditional_put=native the plugin will not assume the
// endpoint enforces the precondition — assuming wrongly would make
// every single-winner guarantee silently false. So the commit takes the
// staging path and fails exactly as reported. That is the correct
// outcome for an unvouched store, and it is why the parameter is now
// documented: the fix is only reachable once the operator vouches.
func TestCommitExclusive_UnvouchedStoreStillFailsHonestly(t *testing.T) {
	sp := newNotImplementedCopySP(t, false)

	err := storage.CommitExclusive(context.Background(), sp, "wal/db1/seg.json",
		[]byte("body"), storage.PutOptions{})
	if err == nil {
		t.Fatal("commit succeeded on a store whose conditional COPY is unimplemented and " +
			"whose conditional PUT is not vouched for; it has no way to publish exclusively")
	}
	if !strings.Contains(err.Error(), "NotImplemented") {
		t.Errorf("the store's own error must survive so an operator can act on it; got %v", err)
	}
}
