package s3_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
)

// multipartSim is a minimal S3 multipart endpoint that can fail one part
// number or the completion, and counts aborts.
type multipartSim struct {
	failPart    int  // part number answered with 403 (0 = none)
	failMissing bool // CompleteMultipartUpload answers 412
	aborts      atomic.Int32
	completes   atomic.Int32
}

func (m *multipartSim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	q := r.URL.Query()
	xmlErr := func(status int, code string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>m</Message></Error>`, code)
	}
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		if n == m.failPart {
			xmlErr(http.StatusForbidden, "AccessDenied")
			return
		}
		w.Header().Set("ETag", fmt.Sprintf(`"etag%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		m.completes.Add(1)
		if m.failMissing {
			xmlErr(http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><ETag>"final"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		m.aborts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func putBig(t *testing.T, sim *multipartSim, opts storage.PutOptions) error {
	t.Helper()
	defer s3.SetMultipartForTest(6<<20, 5<<20)()
	srv := httptest.NewServer(sim)
	defer srv.Close()
	p := openAgainst(t, srv)
	body := make([]byte, 12<<20)
	opts.ContentLength = int64(len(body))
	_, err := p.Put(context.Background(), "k", streamOnly{bytes.NewReader(body)}, opts)
	return err
}

// A part upload that fails must abort the multipart upload, or its parts
// keep accruing storage until a lifecycle rule notices.
func TestPutMultipart_FailedPartAbortsUpload(t *testing.T) {
	sim := &multipartSim{failPart: 2}
	if err := putBig(t, sim, storage.PutOptions{}); err == nil {
		t.Fatal("Put succeeded although part 2 was refused")
	}
	if sim.aborts.Load() != 1 {
		t.Fatalf("AbortMultipartUpload calls = %d, want 1", sim.aborts.Load())
	}
	if sim.completes.Load() != 0 {
		t.Fatal("a failed upload was completed")
	}
}

// A lost IfNotExists race at completion reports ErrAlreadyExists and
// aborts the upload.
func TestPutMultipart_CompletePreconditionFailedAbortsUpload(t *testing.T) {
	sim := &multipartSim{failMissing: true}
	err := putBig(t, sim, storage.PutOptions{IfNotExists: true})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
	if sim.aborts.Load() != 1 {
		t.Fatalf("AbortMultipartUpload calls = %d, want 1", sim.aborts.Load())
	}
}
