package s3_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
)

// scriptedPutServer answers successive PutObject requests with the
// given statuses (the last one repeats) and S3-style XML error codes.
type scriptedPutServer struct {
	mu     sync.Mutex
	script []int
	puts   int
}

func (s *scriptedPutServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.mu.Lock()
	i := s.puts
	s.puts++
	status := s.script[len(s.script)-1]
	if i < len(s.script) {
		status = s.script[i]
	}
	s.mu.Unlock()
	code := ""
	switch status {
	case http.StatusConflict:
		code = "ConditionalRequestConflict"
	case http.StatusPreconditionFailed:
		code = "PreconditionFailed"
	}
	if code != "" {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>m</Message></Error>`, code)
		return
	}
	w.Header().Set("ETag", `"abc"`)
	w.WriteHeader(status)
}

func (s *scriptedPutServer) Puts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

func openAgainst(t *testing.T, srv *httptest.Server) *s3.Plugin {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	u, err := url.Parse(fmt.Sprintf("s3://b/p?endpoint=%s&region=us-east-1&path_style=true&conditional_put=native", srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	p := &s3.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestPut_IfNoneMatch409IsRetried pins M30: AWS answers a conditional
// PUT that races another conditional write on the same key with 409
// ConditionalRequestConflict and documents that the client should
// RETRY. It was neither retried nor mapped, so a benign race surfaced
// as a hard chunk/manifest write failure.
func TestPut_IfNoneMatch409IsRetried(t *testing.T) {
	sim := &scriptedPutServer{script: []int{http.StatusConflict, http.StatusOK}}
	srv := httptest.NewServer(sim)
	defer srv.Close()
	p := openAgainst(t, srv)

	if _, err := p.Put(context.Background(), "k", bytes.NewReader([]byte("hello")),
		storage.PutOptions{IfNotExists: true, ContentLength: 5}); err != nil {
		t.Fatalf("Put after one 409 ConditionalRequestConflict: %v (want a retry that succeeds)", err)
	}
	if sim.Puts() != 2 {
		t.Errorf("server saw %d PUTs, want 2 (one 409, one retry)", sim.Puts())
	}
}

// TestPut_IfNoneMatch409Then412IsAlreadyExists: the retry after a
// conflict finds the racer's object; that is ErrAlreadyExists.
func TestPut_IfNoneMatch409Then412IsAlreadyExists(t *testing.T) {
	sim := &scriptedPutServer{script: []int{http.StatusConflict, http.StatusPreconditionFailed}}
	srv := httptest.NewServer(sim)
	defer srv.Close()
	p := openAgainst(t, srv)

	_, err := p.Put(context.Background(), "k", bytes.NewReader([]byte("hello")),
		storage.PutOptions{IfNotExists: true, ContentLength: 5})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("Put = %v, want ErrAlreadyExists after 409 then 412", err)
	}
}
