package storage_test

// ReadAllLimited is the shared bound for reads of repository objects.
// The properties that matter:
//
//   - exceeding the limit must ERROR, never truncate. A truncated read
//     is worse than a refusal: the caller would parse a prefix of the
//     object and act on whatever it happened to contain — a manifest
//     missing its tail chunk refs, for instance, which is precisely how
//     gc would come to believe live chunks are orphans.
//   - a body exactly at the limit must be accepted, or the ceiling is
//     silently one byte lower than documented.

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func TestReadAllLimited_UnderLimitReadsEverything(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 1000)
	got, err := storage.ReadAllLimited(bytes.NewReader(body), 4096)
	if err != nil {
		t.Fatalf("ReadAllLimited: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("got %d bytes, want %d", len(got), len(body))
	}
}

// Off-by-one on the ceiling itself.
func TestReadAllLimited_ExactlyAtLimitIsAccepted(t *testing.T) {
	const max = 512
	body := bytes.Repeat([]byte("y"), max)
	got, err := storage.ReadAllLimited(bytes.NewReader(body), max)
	if err != nil {
		t.Fatalf("a body of exactly max bytes must be accepted: %v", err)
	}
	if len(got) != max {
		t.Errorf("got %d bytes, want %d", len(got), max)
	}
}

// The property the whole helper exists for.
func TestReadAllLimited_OverLimitErrorsRatherThanTruncating(t *testing.T) {
	const max = 512
	body := bytes.Repeat([]byte("z"), max+1)
	got, err := storage.ReadAllLimited(bytes.NewReader(body), max)
	if err == nil {
		t.Fatalf("a %d-byte body was accepted under a %d-byte limit; it returned %d bytes — "+
			"a silent truncation here means the caller parses a PREFIX of the object and acts "+
			"on it", len(body), max, len(got))
	}
	if got != nil {
		t.Errorf("a rejected read must not also hand back a partial body (%d bytes)", len(got))
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should say the object exceeded the limit: %v", err)
	}
}

// A read error must propagate rather than surface as a short body.
func TestReadAllLimited_PropagatesReadErrors(t *testing.T) {
	boom := errors.New("backend went away")
	r := io.MultiReader(bytes.NewReader([]byte("partial")), errReader{boom})
	if _, err := storage.ReadAllLimited(r, 1<<20); !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v — a swallowed read error becomes a short object", err, boom)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestReadAllLimited_EmptyBody(t *testing.T) {
	got, err := storage.ReadAllLimited(bytes.NewReader(nil), 1024)
	if err != nil {
		t.Fatalf("empty body: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d bytes, want 0", len(got))
	}
}

// The documented ceiling must match backup.MaxManifestBytes, which is
// the project's established limit for the largest metadata object.
func TestMaxMetadataBytes_MatchesTheEstablishedManifestCeiling(t *testing.T) {
	if storage.MaxMetadataBytes != 1<<30 {
		t.Errorf("MaxMetadataBytes = %d, want %d (1 GiB, matching backup.MaxManifestBytes) — "+
			"a lower value would refuse a legitimate manifest for a very large cluster",
			storage.MaxMetadataBytes, 1<<30)
	}
}
