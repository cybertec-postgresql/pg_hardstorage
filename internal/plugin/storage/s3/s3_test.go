// Unit tests for the s3 storage plugin.
//
// We exercise the plugin against a tiny in-process fake S3 server
// built on net/http. The fake supports just enough of the S3 wire
// format (PutObject, GetObject, HeadObject, ListObjectsV2,
// DeleteObject, CopyObject) for the plugin's behaviour to be
// observable. We don't test against a real AWS endpoint here —
// that's an integration concern and lands behind the `integration`
// build tag.
//
// The fake is intentionally permissive about auth: any bearer token
// or signature is accepted. The point is to drive the plugin's
// HTTP-shape; SigV4 correctness is the AWS SDK's responsibility.
package s3_test

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
)

// fakeS3 is a minimum-viable S3 server. Concurrency-safe for the test
// scenarios we drive (sequential within one test).
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte // key path → body
	// failDeleteOnce, when true, makes the next DELETE return 503
	// once and then auto-clears. Used to exercise the rename
	// "copy succeeded but src-delete failed" path without affecting
	// other tests' concurrency.
	failDeleteOnce bool
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Bucket-style URL: /<bucket>/<key>
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	var bucket, key string
	switch len(parts) {
	case 1:
		bucket = parts[0]
	case 2:
		bucket, key = parts[0], parts[1]
	}
	_ = bucket // accepted, never validated

	switch r.Method {
	case http.MethodPut:
		// Distinguish a PutObject from a CopyObject by the presence of
		// the x-amz-copy-source header.
		if src := r.Header.Get("X-Amz-Copy-Source"); src != "" {
			// Strip leading "/" from CopySource and URL-decode.
			src = strings.TrimPrefix(src, "/")
			decoded, err := url.PathUnescape(src)
			if err != nil {
				http.Error(w, "bad copy source", http.StatusBadRequest)
				return
			}
			// CopySource is "bucket/key"; strip the bucket part.
			if idx := strings.IndexByte(decoded, '/'); idx >= 0 {
				decoded = decoded[idx+1:]
			}
			body, ok := f.objects[decoded]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
				return
			}
			if r.Header.Get("If-None-Match") == "*" {
				if _, exists := f.objects[key]; exists {
					w.WriteHeader(http.StatusPreconditionFailed)
					_, _ = w.Write([]byte(`<Error><Code>PreconditionFailed</Code></Error>`))
					return
				}
			}
			f.objects[key] = body
			w.Header().Set("ETag", `"copy-etag"`)
			_, _ = w.Write([]byte(`<CopyObjectResult/>`))
			return
		}

		// Plain PutObject.
		if r.Header.Get("If-None-Match") == "*" {
			if _, exists := f.objects[key]; exists {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte(`<Error><Code>PreconditionFailed</Code></Error>`))
				return
			}
		}
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
		w.Header().Set("ETag", `"put-etag"`)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		// ListObjectsV2 looks like GET /bucket?list-type=2&prefix=...
		if r.URL.Query().Get("list-type") == "2" {
			f.handleList(w, r)
			return
		}
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", `"get-etag"`)
		_, _ = w.Write(body)

	case http.MethodHead:
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"head-etag"`)
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		if f.failDeleteOnce {
			f.failDeleteOnce = false
			// 403 Forbidden — non-retryable per AWS SDK conventions
			// (in contrast with 5xx, which the SDK auto-retries).
			// Mirrors the reviewer's "IAM revocation" example.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code></Error>`))
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not implemented", http.StatusMethodNotAllowed)
	}
}

// handleList implements just enough of ListObjectsV2 for our tests.
func (f *fakeS3) handleList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	type contents struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
	}
	type result struct {
		XMLName  xml.Name   `xml:"ListBucketResult"`
		Name     string     `xml:"Name"`
		Prefix   string     `xml:"Prefix"`
		Contents []contents `xml:"Contents"`
	}
	out := result{Prefix: prefix}
	for k, body := range f.objects {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		out.Contents = append(out.Contents, contents{
			Key:          k,
			Size:         int64(len(body)),
			LastModified: time.Now().UTC().Format(time.RFC3339),
			ETag:         `"list-etag"`,
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	xmlBytes, _ := xml.Marshal(out)
	_, _ = w.Write(xmlBytes)
}

// mustOpen returns an opened s3.Plugin (under the StoragePlugin
// interface) wired to a fake-S3 httptest server.
func mustOpen(t *testing.T, prefix string) (storage.StoragePlugin, *httptest.Server) {
	t.Helper()
	p, srv, _ := mustOpenWithFake(t, prefix)
	return p, srv
}

// mustOpenWithFake is mustOpen plus access to the fakeS3 so a test
// can flip per-call failure switches (e.g. failDeleteOnce).
func mustOpenWithFake(t *testing.T, prefix string) (storage.StoragePlugin, *httptest.Server, *fakeS3) {
	t.Helper()
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	p := &s3.Plugin{}
	urlStr := fmt.Sprintf("s3://test-bucket/%s?endpoint=%s&region=us-east-1&path_style=true", prefix, srv.URL)
	u, err := url.Parse(urlStr)
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		srv.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.Close()
		srv.Close()
	})
	return p, srv, fake
}

func TestS3_PutGetRoundTrip(t *testing.T) {
	p, _ := mustOpen(t, "")
	body := []byte("hello s3")
	if _, err := p.Put(context.Background(), "object1", strings.NewReader(string(body)), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	rc, err := p.Get(context.Background(), "object1")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != string(body) {
		t.Errorf("round-trip differs; got %q", got)
	}
}

func TestS3_PutIfNotExists_RejectsOverwrite(t *testing.T) {
	p, _ := mustOpen(t, "")
	_, err := p.Put(context.Background(), "k", strings.NewReader("first"), storage.PutOptions{IfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Put(context.Background(), "k", strings.NewReader("second"), storage.PutOptions{IfNotExists: true})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("expected ErrAlreadyExists; got %v", err)
	}
}

func TestS3_GetMissing_NotFound(t *testing.T) {
	p, _ := mustOpen(t, "")
	_, err := p.Get(context.Background(), "ghost")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected ErrNotFound; got %v", err)
	}
}

func TestS3_StatMissing_NotFound(t *testing.T) {
	p, _ := mustOpen(t, "")
	_, err := p.Stat(context.Background(), "ghost")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected ErrNotFound; got %v", err)
	}
}

func TestS3_StatPresent(t *testing.T) {
	p, _ := mustOpen(t, "")
	_, _ = p.Put(context.Background(), "alive", strings.NewReader("x"), storage.PutOptions{})
	info, err := p.Stat(context.Background(), "alive")
	if err != nil {
		t.Fatal(err)
	}
	if info.Key != "alive" {
		t.Errorf("Key = %q", info.Key)
	}
	if info.Size == 0 {
		t.Errorf("Size = 0; expected > 0 from Content-Length header")
	}
}

func TestS3_ListPrefixed(t *testing.T) {
	p, _ := mustOpen(t, "")
	for _, k := range []string{"a/1", "a/2", "b/1"} {
		_, _ = p.Put(context.Background(), k, strings.NewReader("x"), storage.PutOptions{})
	}
	got := []string{}
	for info, err := range p.List(context.Background(), "a/") {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, info.Key)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want 2 a/ entries", got)
	}
	for _, k := range got {
		if !strings.HasPrefix(k, "a/") {
			t.Errorf("key %q escaped prefix", k)
		}
	}
}

func TestS3_ListStripsConfiguredPrefix(t *testing.T) {
	// The plugin's URL prefix should be stripped from List results.
	p, _ := mustOpen(t, "myprefix")
	_, _ = p.Put(context.Background(), "child", strings.NewReader("x"), storage.PutOptions{})
	got := []string{}
	for info, err := range p.List(context.Background(), "") {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, info.Key)
	}
	if len(got) != 1 || got[0] != "child" {
		t.Errorf("List should strip myprefix/; got %v", got)
	}
}

func TestS3_DeleteIdempotent(t *testing.T) {
	p, _ := mustOpen(t, "")
	if err := p.Delete(context.Background(), "ghost"); err != nil {
		t.Errorf("delete on missing should be a no-op; got %v", err)
	}
	_, _ = p.Put(context.Background(), "alive", strings.NewReader("x"), storage.PutOptions{})
	if err := p.Delete(context.Background(), "alive"); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(context.Background(), "alive"); err != nil {
		t.Errorf("re-delete should be a no-op; got %v", err)
	}
}

func TestS3_RenameIfNotExists_HappyPath(t *testing.T) {
	p, _ := mustOpen(t, "")
	_, _ = p.Put(context.Background(), "src", strings.NewReader("body"), storage.PutOptions{})
	if err := p.RenameIfNotExists(context.Background(), "src", "dst"); err != nil {
		t.Fatal(err)
	}
	rc, err := p.Get(context.Background(), "dst")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "body" {
		t.Errorf("dst body = %q", got)
	}
	if _, err := p.Stat(context.Background(), "src"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("src should be gone after successful rename; got %v", err)
	}
}

func TestS3_RenameIfNotExists_DstAlreadyExists(t *testing.T) {
	p, _ := mustOpen(t, "")
	_, _ = p.Put(context.Background(), "src", strings.NewReader("a"), storage.PutOptions{})
	_, _ = p.Put(context.Background(), "dst", strings.NewReader("b"), storage.PutOptions{})
	err := p.RenameIfNotExists(context.Background(), "src", "dst")
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("expected ErrAlreadyExists; got %v", err)
	}
}

// Pre-fix this test would have passed with a silent orphan: the
// `_, _ = p.client.DeleteObject(...)` swallowed the 503, so the
// caller got nil while the src object lingered. The fix surfaces
// the error so the operator gets a signal.
//
// Important behaviours this test pins:
//  1. The dst was successfully copied (the operator-visible commit
//     didn't roll back).
//  2. The error is non-nil (the operator KNOWS).
//  3. The error message mentions "unlink src failed" so the
//     operator can grep for it in their incident log.
func TestS3_RenameIfNotExists_SourceDeleteError_Surfaced(t *testing.T) {
	p, _, fake := mustOpenWithFake(t, "")
	_, _ = p.Put(context.Background(), "src", strings.NewReader("body"), storage.PutOptions{})

	// Arm the fake to fail the next DELETE.
	fake.mu.Lock()
	fake.failDeleteOnce = true
	fake.mu.Unlock()

	err := p.RenameIfNotExists(context.Background(), "src", "dst")
	if err == nil {
		t.Fatal("expected error from rename when source delete fails; got nil (silent orphan)")
	}
	if !strings.Contains(err.Error(), "unlink src failed") {
		t.Errorf("error should mention unlink-src-failed; got: %v", err)
	}
	// dst MUST be present (copy succeeded; the rollback logic isn't
	// in scope — the orphan is the cost of S3's two-call rename).
	if _, err := p.Stat(context.Background(), "dst"); err != nil {
		t.Errorf("dst should be committed despite src-delete failure; got %v", err)
	}
	// src is still present (the delete that we asked the fake to
	// fail is what would have removed it). This documents the
	// orphan-leak shape so future GC can sweep it.
	if _, err := p.Stat(context.Background(), "src"); err != nil {
		t.Errorf("src should still exist after src-delete failure; got %v", err)
	}
}

func TestS3_OpenRequiresBucket(t *testing.T) {
	p := &s3.Plugin{}
	u, _ := url.Parse("s3:///no-bucket-here")
	err := p.Open(context.Background(), storage.StorageConfig{URL: u})
	if err == nil || !strings.Contains(err.Error(), "bucket") {
		t.Errorf("expected bucket-required error; got %v", err)
	}
}

func TestS3_PluginName(t *testing.T) {
	p := &s3.Plugin{}
	if p.Name() != "s3" {
		t.Errorf("Name = %q, want s3", p.Name())
	}
}

func TestS3_Capabilities(t *testing.T) {
	p := &s3.Plugin{}
	c := p.Capabilities()
	if !c.WORM || !c.ConditionalPut || !c.Multipart {
		t.Errorf("expected capabilities to advertise WORM/ConditionalPut/Multipart; got %+v", c)
	}
}

func TestS3_RegistersForS3Scheme(t *testing.T) {
	found := false
	for _, s := range storage.Schemes() {
		if s == "s3" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("s3 scheme not registered; got schemes %v", storage.Schemes())
	}
}
