package s3_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
)

// overlapSim is an S3 multipart endpoint that holds every part until a
// second one is in flight, so a serial uploader shows up as peak == 1
// (after the hold times out) rather than as a slow test.
type overlapSim struct {
	mu        sync.Mutex
	cond      *sync.Cond
	inflight  int
	peak      int
	partBytes map[int]int
	completed []int // part numbers, in the order CompleteMultipartUpload lists them
}

var partNumberRe = regexp.MustCompile(`<PartNumber>(\d+)</PartNumber>`)

func (m *overlapSim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		m.mu.Lock()
		m.inflight++
		m.peak = max(m.peak, m.inflight)
		m.cond.Broadcast()
		deadline := time.Now().Add(2 * time.Second)
		wake := time.AfterFunc(2*time.Second, m.cond.Broadcast)
		for m.peak < 2 && time.Now().Before(deadline) {
			m.cond.Wait()
		}
		wake.Stop()
		m.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.inflight--
		m.partBytes[n] = len(body)
		m.mu.Unlock()
		w.Header().Set("ETag", fmt.Sprintf(`"etag%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		for _, sm := range partNumberRe.FindAllSubmatch(body, -1) {
			n, _ := strconv.Atoi(string(sm[1]))
			m.completed = append(m.completed, n)
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><ETag>"final"</ETag></CompleteMultipartUploadResult>`)
	default:
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}
}

// Parts upload concurrently (bounded), every byte is sent exactly once,
// and the completion lists the parts in ascending order whatever order
// they finished in.
func TestPutMultipart_PartsUploadInParallel(t *testing.T) {
	defer s3.SetMultipartForTest(6<<20, 5<<20)()
	sim := &overlapSim{partBytes: map[int]int{}}
	sim.cond = sync.NewCond(&sim.mu)
	srv := httptest.NewServer(sim)
	defer srv.Close()
	p := openAgainst(t, srv)

	const size = 32<<20 + 17 // 7 parts of 5 MiB, the last one short
	body := make([]byte, size)
	if _, err := p.Put(context.Background(), "k", streamOnly{bytes.NewReader(body)},
		storage.PutOptions{ContentLength: size}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	sim.mu.Lock()
	defer sim.mu.Unlock()
	if sim.peak < 2 {
		t.Errorf("peak concurrent part uploads = %d: parts are uploaded one at a time", sim.peak)
	}
	if sim.peak > 4 {
		t.Errorf("peak concurrent part uploads = %d, want at most 4 (each holds a part-sized buffer)", sim.peak)
	}
	if len(sim.completed) != 7 {
		t.Fatalf("completed parts = %v, want 7", sim.completed)
	}
	total := 0
	for i, n := range sim.completed {
		if n != i+1 {
			t.Fatalf("completion lists parts %v, want 1..7 ascending", sim.completed)
		}
		total += sim.partBytes[n]
	}
	if total != size {
		t.Errorf("parts carried %d bytes, want %d", total, size)
	}
}
