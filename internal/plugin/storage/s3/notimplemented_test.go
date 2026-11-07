package s3_test

// notimplemented_test.go — issue #98: an S3-compatible store that
// REJECTS `If-None-Match: *` instead of honouring or ignoring it.
//
// Alibaba Cloud OSS answers a conditional PutObject with
//
//	400 NotImplemented: A header you provided implies functionality
//	that is not implemented.
//
// so `repo init` could not write HSREPO and nothing after it could
// run. The plugin already reported ConditionalPut=false for such an
// endpoint (no ?conditional_put=native vouch), so the callers were on
// their read-back paths; only the header itself was in the way.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
)

// noCondStore is an in-memory bucket that refuses the If-None-Match
// header on PutObject and CopyObject the way OSS does, and otherwise
// behaves: HEAD answers 200/404, PUT stores, COPY copies, DELETE deletes.
type noCondStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	parts   map[string]map[int][]byte // multipart uploads by key, part number
	// request log, one entry per call: "PUT key cond" / "HEAD key" ...
	log []string
}

func newNoCondStore() *noCondStore {
	return &noCondStore{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}}
}

func notImplemented(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NotImplemented</Code>`+
		`<Message>A header you provided implies functionality that is not implemented.</Message></Error>`)
}

func (s *noCondStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	// Path style: /<bucket>/<key>.
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"), "b/")
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := r.Method + " " + key
	if r.Header.Get("If-None-Match") != "" {
		entry += " cond"
	}
	s.log = append(s.log, entry)
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.parts[key] = map[int][]byte{}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><UploadId>u-%s</UploadId></InitiateMultipartUploadResult>`, key, key)
		return
	case r.Method == http.MethodPut && q.Has("partNumber"):
		var n int
		fmt.Sscan(q.Get("partNumber"), &n)
		s.parts[key][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"part%d"`, n))
		w.WriteHeader(http.StatusOK)
		return
	case r.Method == http.MethodPost && q.Has("uploadId"):
		if r.Header.Get("If-None-Match") != "" {
			notImplemented(w)
			return
		}
		var whole []byte
		for n := 1; ; n++ {
			part, ok := s.parts[key][n]
			if !ok {
				break
			}
			whole = append(whole, part...)
		}
		s.objects[key] = whole
		delete(s.parts, key)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>"mp"</ETag></CompleteMultipartUploadResult>`)
		return
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(s.parts, key)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodHead:
		if _, ok := s.objects[key]; ok {
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	case http.MethodPut:
		if r.Header.Get("If-None-Match") != "" {
			notImplemented(w)
			return
		}
		if src := r.Header.Get("X-Amz-Copy-Source"); src != "" {
			src, _ = url.PathUnescape(src)
			src = strings.TrimPrefix(strings.TrimPrefix(src, "/"), "b/")
			data, ok := s.objects[src]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
				return
			}
			s.objects[key] = append([]byte(nil), data...)
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<CopyObjectResult><ETag>"c"</ETag></CopyObjectResult>`)
			return
		}
		s.objects[key] = body
		w.Header().Set("ETag", `"e"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		data, ok := s.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		_, _ = w.Write(data)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (s *noCondStore) object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	return b, ok
}

func (s *noCondStore) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

func openNoCond(t *testing.T, srv *httptest.Server, vouch bool) *s3.Plugin {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	raw := fmt.Sprintf("s3://b/p?endpoint=%s&region=us-east-1&path_style=true", srv.URL)
	if vouch {
		raw += "&conditional_put=native"
	}
	u, err := url.Parse(raw)
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

// TestPut_NotImplementedFallsBackToReadBeforeWrite is the reporter's
// `repo init`: the first conditional PUT is refused, the plugin reads
// the key and writes unconditionally, and from then on never sends
// the header again. The body reaches the store whole although the
// first attempt consumed it.
func TestPut_NotImplementedFallsBackToReadBeforeWrite(t *testing.T) {
	store := newNoCondStore()
	srv := httptest.NewServer(store)
	defer srv.Close()
	p := openNoCond(t, srv, false)
	ctx := context.Background()

	if p.Capabilities().ConditionalPut {
		t.Fatal("an unvouched custom endpoint must report ConditionalPut=false; the fallback relies on it")
	}

	body := []byte(`{"schema":"pg_hardstorage.repo.v1"}`)
	if _, err := p.Put(ctx, "HSREPO", bytes.NewReader(body), storage.PutOptions{IfNotExists: true, ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("Put(IfNotExists) on a store that rejects If-None-Match: %v\n\nThis is issue #98: repo init failed with NotImplemented", err)
	}
	if got, ok := store.object("p/HSREPO"); !ok || !bytes.Equal(got, body) {
		t.Fatalf("stored body = %q (present=%v), want the full body after the rewind", got, ok)
	}
	want := []string{"PUT p/HSREPO cond", "HEAD p/HSREPO", "PUT p/HSREPO"}
	if got := store.requests(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("first put: requests %v, want %v", got, want)
	}

	// Exclusion still holds, through the read.
	_, err := p.Put(ctx, "HSREPO", bytes.NewReader([]byte("other")), storage.PutOptions{IfNotExists: true, ContentLength: 5})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("second Put(IfNotExists) = %v, want ErrAlreadyExists", err)
	}
	if got, _ := store.object("p/HSREPO"); !bytes.Equal(got, body) {
		t.Fatal("the refused second put overwrote the object")
	}

	// A fresh key: no probe any more, the plugin remembers.
	if _, err := p.Put(ctx, "chunks/x", bytes.NewReader([]byte("c")), storage.PutOptions{IfNotExists: true, ContentLength: 1}); err != nil {
		t.Fatal(err)
	}
	for _, r := range store.requests()[3:] {
		if strings.HasSuffix(r, " cond") {
			t.Errorf("header sent again after the store refused it: %q", r)
		}
	}

	// A plain put was never conditional and is unaffected.
	if _, err := p.Put(ctx, "chunks/x", bytes.NewReader([]byte("d")), storage.PutOptions{ContentLength: 1}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.object("p/chunks/x"); string(got) != "d" {
		t.Errorf("unconditional put stored %q, want d", got)
	}
}

// TestPut_NotImplementedWithVouchIsAnError: the operator claimed the
// endpoint enforces the header; it does not. Downgrading silently would
// turn the vouch into a lie, so the put fails and the message says
// what to change.
func TestPut_NotImplementedWithVouchIsAnError(t *testing.T) {
	store := newNoCondStore()
	srv := httptest.NewServer(store)
	defer srv.Close()
	p := openNoCond(t, srv, true)

	_, err := p.Put(context.Background(), "HSREPO", bytes.NewReader([]byte("x")), storage.PutOptions{IfNotExists: true, ContentLength: 1})
	if err == nil {
		t.Fatal("Put succeeded on a vouched endpoint that rejects If-None-Match; the capability claim must not be downgraded silently")
	}
	if !strings.Contains(err.Error(), "conditional_put=native") || !strings.Contains(err.Error(), "NotImplemented") {
		t.Errorf("error must name the parameter to drop and keep the store's code; got %v", err)
	}
	if _, ok := store.object("p/HSREPO"); ok {
		t.Error("the object was written despite the error")
	}
}

// TestRenameIfNotExists_NotImplementedCopyFallsBack covers the staging
// commit path such a store takes (ConditionalPut=false → stage +
// rename): a conditional COPY refused with NotImplemented is retried
// without the directive; the HEAD before it is the guard, as on stores
// that ignore the directive.
func TestRenameIfNotExists_NotImplementedCopyFallsBack(t *testing.T) {
	store := newNoCondStore()
	srv := httptest.NewServer(store)
	defer srv.Close()
	p := openNoCond(t, srv, false)
	ctx := context.Background()

	if _, err := p.Put(ctx, "m.tmp", bytes.NewReader([]byte("manifest")), storage.PutOptions{ContentLength: 8}); err != nil {
		t.Fatal(err)
	}
	if err := p.RenameIfNotExists(ctx, "m.tmp", "m"); err != nil {
		t.Fatalf("RenameIfNotExists on a store that rejects a conditional COPY: %v", err)
	}
	if got, ok := store.object("p/m"); !ok || string(got) != "manifest" {
		t.Fatalf("dst = %q present=%v", got, ok)
	}
	if _, ok := store.object("p/m.tmp"); ok {
		t.Error("src survived the rename")
	}

	if _, err := p.Put(ctx, "m2.tmp", bytes.NewReader([]byte("other")), storage.PutOptions{ContentLength: 5}); err != nil {
		t.Fatal(err)
	}
	if err := p.RenameIfNotExists(ctx, "m2.tmp", "m"); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("rename onto an existing dst = %v, want ErrAlreadyExists", err)
	}
	if got, _ := store.object("p/m"); string(got) != "manifest" {
		t.Error("the refused rename overwrote dst")
	}
	var conds int
	for _, r := range store.requests() {
		if strings.HasSuffix(r, " cond") {
			conds++
		}
	}
	if conds != 1 {
		t.Errorf("%d conditional requests, want exactly the one probe", conds)
	}
}

// TestCommitExclusive_OnStoreWithoutAnyConditionalWrite: the manifest
// commit end to end on the reporter's store. ConditionalPut is false,
// so CommitExclusive stages and renames; both legs now work.
func TestCommitExclusive_OnStoreWithoutAnyConditionalWrite(t *testing.T) {
	store := newNoCondStore()
	srv := httptest.NewServer(store)
	defer srv.Close()
	p := openNoCond(t, srv, false)
	ctx := context.Background()

	if err := storage.CommitExclusive(ctx, p, "manifests/d/b.json", []byte(`{}`), storage.PutOptions{}); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	err := storage.CommitExclusive(ctx, p, "manifests/d/b.json", []byte(`{"x":1}`), storage.PutOptions{})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("second commit = %v, want ErrAlreadyExists", err)
	}
	if got, _ := store.object("p/manifests/d/b.json"); string(got) != `{}` {
		t.Errorf("committed object = %q, the second writer must lose", got)
	}
	for k := range store.objects {
		if strings.Contains(k, ".tmp.") {
			t.Errorf("staging object %s left behind", k)
		}
	}
}

// TestPutMultipart_NotImplementedFallsBack: the same refusal on
// CompleteMultipartUpload. The parts are already uploaded when it
// arrives; the completion is retried after the read, without the
// directive, and nothing is re-sent.
func TestPutMultipart_NotImplementedFallsBack(t *testing.T) {
	store := newNoCondStore()
	srv := httptest.NewServer(store)
	defer srv.Close()
	p := openNoCond(t, srv, false)
	defer s3.SetMultipartForTest(1<<20, 1<<20)()
	ctx := context.Background()

	body := append(bytes.Repeat([]byte("0123456789abcdef"), (3<<20)/16), 'z') // 3 MiB + 1 → 4 parts
	put := func(data []byte) error {
		_, err := p.Put(ctx, "big", bytes.NewReader(data), storage.PutOptions{IfNotExists: true, ContentLength: int64(len(data))})
		return err
	}
	if err := put(body); err != nil {
		t.Fatalf("multipart Put(IfNotExists) on a store that rejects If-None-Match: %v", err)
	}
	if got, ok := store.object("p/big"); !ok || !bytes.Equal(got, body) {
		t.Fatalf("stored %d bytes (present=%v), want %d", len(got), ok, len(body))
	}
	if err := put([]byte(strings.Repeat("x", 2<<20))); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("second multipart Put(IfNotExists) = %v, want ErrAlreadyExists", err)
	}
	if got, _ := store.object("p/big"); !bytes.Equal(got, body) {
		t.Error("the refused second upload overwrote the object")
	}
	var conds, posts, puts int
	for _, r := range store.requests() {
		if strings.HasSuffix(r, " cond") {
			conds++
		}
		if strings.HasPrefix(r, "POST p/big") {
			posts++
		}
		if strings.HasPrefix(r, "PUT p/big") {
			puts++
		}
	}
	if conds != 1 {
		t.Errorf("%d conditional requests, want exactly the one probe", conds)
	}
	// First upload: initiate, refused completion, completion = 3 POSTs
	// and 4 parts. The second must be refused by the read BEFORE any
	// part goes up: no further POST or PUT.
	if posts != 3 || puts != 4 {
		t.Errorf("POSTs=%d PUTs=%d on the key, want 3 and 4 (the refused upload must not send parts)", posts, puts)
	}
	store.mu.Lock()
	open := len(store.parts)
	store.mu.Unlock()
	if open != 0 {
		t.Errorf("%d multipart uploads left open", open)
	}
}
