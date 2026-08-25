package s3_test

// addressing_test.go — issue #52: path-style vs virtual-hosted.
//
// The style was resolved as `path_style || endpoint != ""`, which made
// the parameter one-way: with a custom ?endpoint= set, path-style was
// forced and `path_style=false` could not turn it off. Alibaba Cloud
// OSS REQUIRES virtual-hosted addressing and answers path-style with
//
//	403 SecondLevelDomainForbidden: Please use virtual hosted style
//	to access.
//
// so an OSS repository could not be created at all — `repo init` failed
// on the very first PutObject of HSREPO.
//
// The two styles are distinguishable on the wire, which is what these
// tests assert rather than reading a config field: virtual-hosted puts
// the bucket in the HOST (bucket.endpoint/key), path-style puts it in
// the PATH (endpoint/bucket/key).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
)

// addrCaptureServer records the Host and path of the first request.
type addrCaptureServer struct {
	mu   sync.Mutex
	host string
	path string
	seen bool
}

func (c *addrCaptureServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	if !c.seen {
		c.host, c.path, c.seen = r.Host, r.URL.Path, true
	}
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (c *addrCaptureServer) observed() (string, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.host, c.path, c.seen
}

// hostEndpoint rewrites httptest's http://127.0.0.1:PORT into
// http://localhost:PORT.
//
// This is load-bearing, not cosmetic: virtual-hosted addressing puts
// the bucket in the hostname, and there is no such thing as
// "bucketxx.127.0.0.1". Against an IP-literal endpoint the SDK falls
// back to path-style whatever UsePathStyle says, so a test that used
// httptest's URL directly could never observe the style it asserts.
// *.localhost resolves to 127.0.0.1, so the request still lands here.
func hostEndpoint(t *testing.T, srvURL string) string {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatal(err)
	}
	return "http://localhost:" + u.Port()
}

// openAndPut opens a plugin at the given URL and issues one Put so the
// SDK actually builds a request we can inspect.
func openAndPut(t *testing.T, rawURL string) (host, path string) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	p := &s3.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("Open(%s): %v", rawURL, err)
	}
	defer p.Close()
	// The response body is irrelevant; we only need the request built.
	_, _ = p.Put(context.Background(), "HSREPO", strings.NewReader("x"), storage.PutOptions{})
	return "", ""
}

// The reporter's case: a custom endpoint with path_style=false must
// produce virtual-hosted addressing. Before the fix this was
// unreachable — the bucket stayed in the path and OSS returned 403.
func TestS3_CustomEndpoint_PathStyleFalse_UsesVirtualHosted(t *testing.T) {
	cap := &addrCaptureServer{}
	srv := httptest.NewServer(cap)
	defer srv.Close()

	openAndPut(t, fmt.Sprintf("s3://bucketxx/phsrepo?region=us-east-1&endpoint=%s&path_style=false", hostEndpoint(t, srv.URL)))

	host, path, seen := cap.observed()
	if !seen {
		t.Fatal("no request reached the server")
	}
	if !strings.HasPrefix(host, "bucketxx.") {
		t.Errorf("host = %q, want the bucket in the HOST (virtual-hosted). "+
			"path_style=false was ignored, which is what made Alibaba Cloud OSS "+
			"unusable: it answers path-style with SecondLevelDomainForbidden", host)
	}
	if strings.HasPrefix(path, "/bucketxx/") {
		t.Errorf("path = %q still carries the bucket — this is path-style addressing", path)
	}
}

// The MinIO default must not regress: a custom endpoint with no
// path_style keeps path-style, because MinIO/localstack are commonly
// used with bucket names that are not DNS-valid.
func TestS3_CustomEndpoint_Default_StaysPathStyle(t *testing.T) {
	cap := &addrCaptureServer{}
	srv := httptest.NewServer(cap)
	defer srv.Close()

	openAndPut(t, fmt.Sprintf("s3://bucketxx/phsrepo?region=us-east-1&endpoint=%s", hostEndpoint(t, srv.URL)))

	host, path, seen := cap.observed()
	if !seen {
		t.Fatal("no request reached the server")
	}
	if strings.HasPrefix(host, "bucketxx.") {
		t.Errorf("host = %q — a custom endpoint without path_style must stay path-style "+
			"so MinIO/localstack with non-DNS bucket names keep working", host)
	}
	if !strings.HasPrefix(path, "/bucketxx/") {
		t.Errorf("path = %q, want the bucket in the PATH", path)
	}
}

// path_style=true must still force path-style.
func TestS3_CustomEndpoint_PathStyleTrue_StaysPathStyle(t *testing.T) {
	cap := &addrCaptureServer{}
	srv := httptest.NewServer(cap)
	defer srv.Close()

	openAndPut(t, fmt.Sprintf("s3://bucketxx/phsrepo?region=us-east-1&endpoint=%s&path_style=true", hostEndpoint(t, srv.URL)))

	_, path, seen := cap.observed()
	if !seen {
		t.Fatal("no request reached the server")
	}
	if !strings.HasPrefix(path, "/bucketxx/") {
		t.Errorf("path = %q, want the bucket in the PATH", path)
	}
}

// A non-boolean is refused rather than silently read as false.
// strconv.ParseBool does accept 1/0/t/f/TRUE/False, so this uses a
// value that is genuinely not a boolean: "yes" quietly meaning "no" is
// how an operator ends up debugging a 403 their URL says cannot
// happen.
func TestS3_PathStyle_NonBooleanRefused(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	u, err := url.Parse("s3://bucketxx/phsrepo?region=us-east-1&endpoint=https://example.invalid&path_style=yes")
	if err != nil {
		t.Fatal(err)
	}
	p := &s3.Plugin{}
	err = p.Open(context.Background(), storage.StorageConfig{URL: u})
	if err == nil {
		p.Close()
		t.Fatal("path_style=yes accepted; a non-boolean must be refused rather than " +
			"silently read as false")
	}
	if !strings.Contains(err.Error(), "path_style") {
		t.Errorf("error does not name the parameter:\n%v", err)
	}
}
