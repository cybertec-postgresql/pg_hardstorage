package repo

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// flakyGetSP fails Get for one specific key with a NON-NotFound error
// (throttle/partition/IAM-denial shape).
type flakyGetSP struct {
	storage.StoragePlugin
	failKey string
}

func (f *flakyGetSP) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == f.failKey {
		return nil, errors.New("simulated transient storage error (503)")
	}
	return f.StoragePlugin.Get(ctx, key)
}

// The forward-format gate used to run ONLY when the _repo_version.json
// Get succeeded: every other failure mode (throttling, partition, IAM
// deny on that one key) was treated like "marker absent = v1.0" and
// Open proceeded — letting an old binary mutate a future-format repo
// whose v2 manifests its lenient readers skip as "malformed", so a
// routine `repo gc --apply` would reap chunks those manifests still
// reference. The gate must be fail-closed: only a definitive
// ErrNotFound may take the legacy path.
func TestCheckRepoFormat_FailsClosedOnTransientReadError(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(context.Background(), InitOptions{URL: "file://" + root}); err != nil {
		t.Fatal(err)
	}
	inner := &fs.Plugin{}
	if err := inner.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	err := checkRepoFormat(context.Background(),
		&flakyGetSP{StoragePlugin: inner, failKey: RepoVersionFilename})
	if err == nil {
		t.Fatal("format gate passed despite an undeterminable repo format — fail-open")
	}
	if !strings.Contains(err.Error(), RepoVersionFilename) {
		t.Errorf("error should name the version marker; got: %v", err)
	}

	// Definitive absence (legacy pre-v0.10 repo) must still pass.
	if err := inner.Delete(context.Background(), RepoVersionFilename); err != nil {
		t.Fatal(err)
	}
	if err := checkRepoFormat(context.Background(), inner); err != nil {
		t.Errorf("legacy repo without a version marker must pass the gate, got: %v", err)
	}

	// A future format must still refuse (the gate's original job).
	if _, err := inner.Put(context.Background(), RepoVersionFilename,
		strings.NewReader(`{"schema":"pg_hardstorage.repo_version.v1","format":"v9.9"}`), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	var unsup *ErrRepoFormatUnsupported
	if err := checkRepoFormat(context.Background(), inner); !errors.As(err, &unsup) {
		t.Errorf("future format v9.9: err = %v, want ErrRepoFormatUnsupported", err)
	}
}
