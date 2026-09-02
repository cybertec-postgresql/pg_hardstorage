package throttle_test

// Forwarding must actually carry the answer, not merely exist.
//
// capacity.Preflight asks storage.FreeSpaceOf, which type-asserts
// FreeSpaceAware. A Throttle that did not implement it failed the
// assertion, so the pre-flight returned PreflightUnsupported with the
// note "backend does not expose free-space probe" -- a true sentence
// about an object store and a false one about the fs plugin underneath,
// whose disk-space gate had just been switched off because a bandwidth
// limit was configured.

import (
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/throttle"
)

func TestThrottle_FreeSpaceSeesThroughToTheBackend(t *testing.T) {
	root := t.TempDir()
	inner := &fs.Plugin{}
	if err := inner.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	// Precondition: the backend itself reports free space.
	bare, err := storage.FreeSpaceOf(context.Background(), inner)
	if err != nil {
		t.Fatalf("FreeSpaceOf(fs): %v", err)
	}
	if bare.Unsupported {
		t.Skip("this filesystem does not support the free-space probe; nothing to see through")
	}

	wrapped := throttle.New(inner, 1<<20)
	got, err := storage.FreeSpaceOf(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("FreeSpaceOf(throttle{fs}): %v", err)
	}
	if got.Unsupported {
		t.Fatal("a throttled fs plugin reports free space as unsupported.\n\n" +
			"The wrapper does not fall through here -- it answers. capacity.Preflight " +
			"would record PreflightUnsupported and skip the disk-space gate entirely, " +
			"purely because a bandwidth limit was configured.")
	}
	if got.TotalBytes != bare.TotalBytes {
		t.Errorf("TotalBytes through the wrapper = %d, bare = %d; the wrapper is not "+
			"reporting the same volume", got.TotalBytes, bare.TotalBytes)
	}
}
