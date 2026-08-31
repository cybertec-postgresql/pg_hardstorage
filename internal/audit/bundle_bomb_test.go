package audit_test

// `audit verify-bundle <path>` is pointed at a file the operator
// RECEIVED — an auditor's copy, a partner's export, evidence attached
// to a ticket. It decompresses that file and holds every entry in
// memory to reconstruct the canonical signing input.
//
// It had a path-traversal check, so hostile input was clearly
// considered, but no size or count bound at all: gzip.NewReader
// straight into tar.NewReader, io.ReadAll per entry, everything into a
// map. A few-MB tarball expanding to hundreds of GB, or one with
// millions of tiny entries, exhausts the machine.
//
// The repo-bundle importer has carried entry-count, per-entry and
// whole-bundle caps since input-validation audit #4. This is the same
// posture for the sibling path — the one that takes files from
// outside.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
)

// gzipTar builds a .tar.gz from the given entries.
func gzipTar(t *testing.T, entries []struct {
	name string
	body []byte
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: 0o600, Size: int64(len(e.body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A highly compressible payload past the whole-bundle ceiling: the
// classic bomb shape, tiny on disk and enormous decompressed.
func TestVerifyBundle_RefusesADecompressionBomb(t *testing.T) {
	const oversize = audit.MaxBundleBytes + (1 << 20)
	zeros := make([]byte, 1<<20)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name: "events.ndjson", Mode: 0o600, Size: oversize,
	}); err != nil {
		t.Fatal(err)
	}
	for written := int64(0); written < oversize; written += int64(len(zeros)) {
		n := int64(len(zeros))
		if oversize-written < n {
			n = oversize - written
		}
		if _, err := tw.Write(zeros[:n]); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = zw.Close()

	t.Logf("bomb is %d bytes on disk, %d decompressed (%.0fx)",
		buf.Len(), oversize, float64(oversize)/float64(buf.Len()))

	_, err := audit.VerifyBundle(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("VerifyBundle accepted a bundle decompressing past its ceiling — a few-MB " +
			"file the operator received can exhaust the machine running the verification")
	}
	if !strings.Contains(err.Error(), "limit") && !strings.Contains(err.Error(), "decompresses to more") {
		t.Errorf("refused, but not for the size: %v", err)
	}
}

// Millions of tiny entries: bounded per-entry, unbounded in count.
func TestVerifyBundle_RefusesTooManyEntries(t *testing.T) {
	var entries []struct {
		name string
		body []byte
	}
	for i := 0; i <= audit.MaxBundleEntries; i++ {
		entries = append(entries, struct {
			name string
			body []byte
		}{name: "f" + itoa(i), body: []byte("x")})
	}
	_, err := audit.VerifyBundle(bytes.NewReader(gzipTar(t, entries)))
	if err == nil {
		t.Fatal("VerifyBundle accepted a bundle with more entries than the cap")
	}
	if !strings.Contains(err.Error(), "entries") {
		t.Errorf("refused, but not for the entry count: %v", err)
	}
}

// The pre-existing path-traversal guard must survive the change.
func TestVerifyBundle_StillRefusesTraversalNames(t *testing.T) {
	for _, name := range []string{"../escape", "/etc/passwd", "a/../../b"} {
		body := gzipTar(t, []struct {
			name string
			body []byte
		}{{name: name, body: []byte("x")}})
		if _, err := audit.VerifyBundle(bytes.NewReader(body)); err == nil {
			t.Errorf("accepted traversal name %q", name)
		}
	}
}

// A well-formed but incomplete bundle must fail on its own merits
// (missing bundle.json), not on a size bound — otherwise the new caps
// would be masking real diagnostics.
func TestVerifyBundle_SmallBundleStillReachesItsRealError(t *testing.T) {
	body := gzipTar(t, []struct {
		name string
		body []byte
	}{{name: "README.md", body: []byte("hello")}})

	_, err := audit.VerifyBundle(bytes.NewReader(body))
	if err == nil {
		t.Fatal("expected the missing-bundle.json error")
	}
	if !strings.Contains(err.Error(), "bundle.json") {
		t.Errorf("a small valid tarball hit a size guard instead of its real error: %v", err)
	}
}

// Truncated gzip must be reported as such rather than silently
// yielding a short file set (which would surface later as a confusing
// signature failure).
func TestVerifyBundle_TruncatedArchiveErrors(t *testing.T) {
	full := gzipTar(t, []struct {
		name string
		body []byte
	}{{name: "bundle.json", body: []byte(`{"schema":"x"}`)}})
	if _, err := audit.VerifyBundle(bytes.NewReader(full[:len(full)/2])); err == nil {
		t.Error("a truncated archive verified successfully")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

var _ = io.Discard
