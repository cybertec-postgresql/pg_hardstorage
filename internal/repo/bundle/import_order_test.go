package bundle_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/bundle"
)

// truncateAfterManifests re-emits the exported tar but stops right after
// the first chunk entry's header — a transfer that died mid-bundle.
// Export writes manifests before chunks, so the truncated stream carries
// every manifest and not every chunk.
func truncateAfterManifests(t *testing.T, full []byte) []byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(full))
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	sawManifest := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("bundle had no chunk entry after its manifests")
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(hdr.Name, "manifests/") {
			sawManifest = true
		}
		if strings.HasPrefix(hdr.Name, "chunks/") && sawManifest {
			// Header for a chunk, then the stream ends mid-body.
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			return out.Bytes() // no body, no trailer: truncated
		}
		body, _ := io.ReadAll(tr)
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
}

// An import that aborts part-way must not leave manifests visible over
// chunks that never arrived: listed, signature-valid, unrestorable
// backups. Manifests are written only after every chunk is in.
func TestImport_AbortedImportLeavesNoManifests(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	commitManifest(t, src, sampleManifest(t, src, "db1.full.20260428T120000Z"))
	var buf bytes.Buffer
	if _, err := bundle.Export(ctx, src, &buf, bundle.ExportOptions{Deployment: "db1"}); err != nil {
		t.Fatal(err)
	}
	dst := newRepo(t)
	if _, err := bundle.Import(ctx, bytes.NewReader(truncateAfterManifests(t, buf.Bytes())), dst, bundle.ImportOptions{}); err == nil {
		t.Fatal("a truncated bundle imported successfully")
	}
	for info, err := range dst.List(ctx, "manifests/") {
		if err != nil {
			t.Fatal(err)
		}
		t.Errorf("aborted import left %s in the repository", info.Key)
	}
}
