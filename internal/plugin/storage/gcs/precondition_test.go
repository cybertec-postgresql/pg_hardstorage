package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gcssdk "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// TestIsPreconditionFailed_TypedOnly pins that the 412 classification
// is made from the SDK's typed error, never from the error TEXT. A
// transport error quotes the upload URL, and that URL carries the
// object name: a chunk hash containing "412" (about 1.5% of them) or a
// dated key like 20260412 used to be read as "already exists", and CAS
// then counted a chunk that was never stored as a dedup hit.
func TestIsPreconditionFailed_TypedOnly(t *testing.T) {
	transport := &url.Error{
		Op:  "Post",
		URL: "https://storage.googleapis.com/upload/storage/v1/b/bkt/o?name=chunks%2Fab%2Fab412cd&ifGenerationMatch=0",
		Err: io.ErrUnexpectedEOF,
	}
	for name, err := range map[string]error{
		"transport error quoting a 412 key": fmt.Errorf("gcs: put: %w", transport),
		"dated key":                         errors.New(`Post "https://x/o?name=wal/20260412/seg": connection reset`),
		"Precondition in a message":         errors.New("proxy: Precondition Required by upstream"),
		"conditionNotMet text, other code":  &googleapi.Error{Code: 503, Message: "conditionNotMet mentioned in 503"},
	} {
		if isPreconditionFailed(err) {
			t.Errorf("%s: classified as precondition-failed (ErrAlreadyExists); must not be", name)
		}
	}
	for name, err := range map[string]error{
		"googleapi 412":         &googleapi.Error{Code: 412},
		"wrapped googleapi 412": fmt.Errorf("writer: %w", &googleapi.Error{Code: 412, Message: "conditionNotMet"}),
	} {
		if !isPreconditionFailed(err) {
			t.Errorf("%s: not classified as precondition-failed", name)
		}
	}
}

// newFakeGCSPlugin returns a Plugin wired to srv with retries off, so a
// failure surfaces on the first attempt.
func newFakeGCSPlugin(t *testing.T, srv *httptest.Server) *Plugin {
	t.Helper()
	cli, err := gcssdk.NewClient(context.Background(),
		option.WithEndpoint(srv.URL+"/storage/v1/"),
		option.WithoutAuthentication(),
		option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cli.SetRetry(gcssdk.WithPolicy(gcssdk.RetryNever))
	t.Cleanup(func() { _ = cli.Close() })
	return &Plugin{bucket: "bkt", client: cli}
}

// TestPut_TransportErrorOnKeyContaining412_IsNotAlreadyExists drives
// the real SDK: the upload connection dies, the error text quotes the
// object name (which contains 412), and Put must report a failure —
// not ErrAlreadyExists, which CAS treats as a successful dedup.
func TestPut_TransportErrorOnKeyContaining412_IsNotAlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		c, _, _ := hj.Hijack()
		_ = c.Close()
	}))
	defer srv.Close()
	p := newFakeGCSPlugin(t, srv)

	_, err := p.Put(context.Background(), "chunks/ab/ab412cdef", strings.NewReader("data"),
		storage.PutOptions{IfNotExists: true})
	if err == nil {
		t.Fatal("Put succeeded against a server that drops every connection")
	}
	if errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("transport failure mapped to ErrAlreadyExists (chunk would be counted as stored): %v", err)
	}
}

// TestPut_Real412_IsAlreadyExists keeps the genuine precondition path.
func TestPut_Real412_IsAlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `{"error":{"code":412,"message":"At least one of the pre-conditions you specified did not hold.","errors":[{"reason":"conditionNotMet"}]}}`)
	}))
	defer srv.Close()
	p := newFakeGCSPlugin(t, srv)

	_, err := p.Put(context.Background(), "chunks/ab/abcdef", strings.NewReader("data"),
		storage.PutOptions{IfNotExists: true})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("Put on 412 = %v, want ErrAlreadyExists", err)
	}
}
