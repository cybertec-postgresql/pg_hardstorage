package throttle_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/throttle"
)

// bareBackend implements nothing optional: the capability assertions in
// the wrapper must fail against it.
type bareBackend struct{ storage.StoragePlugin }

func TestThrottle_ReapStagingForwardsToAwareBackend(t *testing.T) {
	inner := &fs.Plugin{}
	if err := inner.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	got, err := throttle.New(inner, 1<<20).ReapStaging(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.Unsupported {
		t.Fatal("throttle hid the fs plugin's staging reaper: repo gc would report it unsupported")
	}
}

func TestThrottle_ReapStagingReportsUnsupportedWhenBackendCannot(t *testing.T) {
	got, err := throttle.New(bareBackend{}, 1<<20).ReapStaging(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Unsupported {
		t.Fatal("a backend without a reaper must be reported Unsupported")
	}
}

func TestThrottle_FreeSpaceUnsupportedBackend(t *testing.T) {
	got, err := throttle.New(bareBackend{}, 1<<20).FreeSpace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Unsupported {
		t.Fatal("a backend without a free-space probe must be reported Unsupported")
	}
}
