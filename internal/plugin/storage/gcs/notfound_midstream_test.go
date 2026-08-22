package gcs

// notfound_midstream_test.go — "missing" must be answerable during the
// body read, not only at open.
//
// The GCS client can hand back a working reader and then report
// ErrObjectNotExist from Read, when the object is deleted between
// NewReader and the pull. Callers key on storage.ErrNotFound; if the
// raw client error escapes, they stop recognising a plain missing
// object.
//
// That is not theoretical. Lease.Acquire has a branch for exactly the
// benign race where a holder releases between its failed create and its
// read, guarded by errors.Is(rerr, storage.ErrNotFound). On GCS the
// guard never matched, so a concurrent release became a hard backup
// failure reading "a lease exists but could not be read: storage:
// object doesn't exist" — wrong on both counts. Caught by
// TestLeaseSoak_NeverTwoHolders/gcs under contention.

import (
	"errors"
	"io"
	"strings"
	"testing"

	gcs "cloud.google.com/go/storage"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// errAfterReader yields some bytes, then the supplied error — the shape
// of an object deleted mid-stream.
type errAfterReader struct {
	body []byte
	err  error
	off  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off < len(r.body) {
		n := copy(p, r.body[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}
func (r *errAfterReader) Close() error { return nil }

func TestNotFoundOnRead_MidStreamObjectNotExistBecomesErrNotFound(t *testing.T) {
	rc := notFoundOnRead{&errAfterReader{body: []byte("part"), err: gcs.ErrObjectNotExist}}

	_, err := io.ReadAll(rc)
	if err == nil {
		t.Fatal("expected an error once the object vanished mid-read")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("mid-stream ErrObjectNotExist did not map to storage.ErrNotFound: %v\n"+
			"callers keyed on ErrNotFound (Lease.Acquire's released-between-put-and-read "+
			"branch) silently stop matching, turning a benign race into a hard failure", err)
	}
}

// EOF must stay EOF, or every successful read looks like a miss.
func TestNotFoundOnRead_EOFPassesThrough(t *testing.T) {
	rc := notFoundOnRead{&errAfterReader{body: []byte("whole body"), err: io.EOF}}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("clean read returned an error: %v", err)
	}
	if string(got) != "whole body" {
		t.Fatalf("got %q, want %q", got, "whole body")
	}
}

// An unrelated failure must not be laundered into "missing" — that
// would turn a transport fault into a silent absent-object answer.
func TestNotFoundOnRead_UnrelatedErrorNotMasked(t *testing.T) {
	boom := errors.New("connection reset by peer")
	rc := notFoundOnRead{&errAfterReader{body: []byte("x"), err: boom}}

	_, err := io.ReadAll(rc)
	if errors.Is(err, storage.ErrNotFound) {
		t.Fatal("an unrelated read error was reported as ErrNotFound")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("original error lost: %v", err)
	}
}
