package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
)

// streamOnly hides every optional interface of a reader (Seek, Size, ...).
type streamOnly struct{ r io.Reader }

func (s streamOnly) Read(p []byte) (int, error) { return s.r.Read(p) }

func TestS3_PutMultipart_StreamedBodyAndExclusiveCommit(t *testing.T) {
	p := openS3OnFreshMinIO(t)
	defer s3.SetMultipartForTest(6<<20, 5<<20)()
	ctx := context.Background()

	body := make([]byte, 12<<20+123)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	// An unseekable stream: the single-PUT path cannot even sign this.
	if _, err := p.Put(ctx, "big/obj", streamOnly{bytes.NewReader(body)}, storage.PutOptions{
		IfNotExists: true, ContentLength: int64(len(body)),
	}); err != nil {
		t.Fatalf("multipart put: %v", err)
	}
	rc, err := p.Get(ctx, "big/obj")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("round trip differs: %d bytes vs %d", len(got), len(body))
	}
	// The exclusive-commit contract holds on the multipart path.
	_, err = p.Put(ctx, "big/obj", streamOnly{bytes.NewReader(body)}, storage.PutOptions{
		IfNotExists: true, ContentLength: int64(len(body)),
	})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("second IfNotExists put: err = %v, want ErrAlreadyExists", err)
	}
	// A body shorter than declared fails cleanly (and aborts the upload).
	if _, err := p.Put(ctx, "big/short", streamOnly{bytes.NewReader(body[:7<<20])}, storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err == nil {
		t.Fatal("short body accepted")
	}
	if _, err := p.Stat(ctx, "big/short"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("short upload left an object: %v", err)
	}
}
