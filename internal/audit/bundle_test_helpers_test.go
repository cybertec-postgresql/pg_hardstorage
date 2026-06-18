package audit_test

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"time"
)

// Tiny stdlib-import wrappers used only by craftMaliciousBundle
// in bundle_test.go.  Kept in a sibling file so the main test
// file's imports list stays focused on the bundle assertions.

func newGzipWriter(w io.Writer) *gzip.Writer { return gzip.NewWriter(w) }
func newTarWriter(w io.Writer) *tar.Writer   { return tar.NewWriter(w) }

func newTarHeader(name string, size int64) *tar.Header {
	return &tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    size,
		ModTime: time.Now().UTC(),
		Format:  tar.FormatPAX,
	}
}
