package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// flakyObject is a one-object backend whose reads break off: every
// Get (or GetFrom) serves at most `burst` bytes and then fails, for the
// first `breaks` opens. With ranged=false it does not implement
// OffsetReadAware, so a resume has to skip the prefix.
type flakyObject struct {
	StoragePlugin
	body    []byte
	burst   int
	breaks  int
	growing bool // each open serves burst more bytes than the last

	mu     sync.Mutex
	opens  int
	ranged []int64 // offsets GetFrom was asked for
	gone   bool
}

var errReset = errors.New("read tcp 10.0.0.4:51234->20.60.1.1:443: read: connection reset by peer")

func (f *flakyObject) open(off int64) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone {
		return nil, ErrNotFound
	}
	f.opens++
	r := io.Reader(bytes.NewReader(f.body[off:]))
	if f.opens <= f.breaks {
		// An unranged open starts at byte 0 again, so it has to get
		// further than the last one for a resume to be possible at all.
		limit := int64(f.burst)
		if f.growing {
			limit *= int64(f.opens)
		}
		r = io.MultiReader(io.LimitReader(r, limit), errReader{errReset})
	}
	return io.NopCloser(r), nil
}

func (f *flakyObject) Get(context.Context, string) (io.ReadCloser, error) { return f.open(0) }

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type rangedFlaky struct{ *flakyObject }

func (r rangedFlaky) GetFrom(_ context.Context, _ string, off int64) (io.ReadCloser, error) {
	r.mu.Lock()
	r.ranged = append(r.ranged, off)
	r.mu.Unlock()
	return r.open(off)
}

func noBackoff(t *testing.T) {
	old := resumeBackoff
	resumeBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { resumeBackoff = old })
}

func testBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

// Issue #97: a restore streams the manifest for hours; one connection
// reset ended it. The stream must carry on from the byte it reached —
// with a ranged request where the backend has one, by skipping the
// prefix where it has not — and deliver exactly the object's bytes.
func TestGetResuming_SurvivesBrokenReads(t *testing.T) {
	noBackoff(t)
	body := testBody(100_000)
	for _, ranged := range []bool{true, false} {
		f := &flakyObject{body: body, burst: 9_000, breaks: 7, growing: !ranged}
		var sp StoragePlugin = f
		if ranged {
			sp = rangedFlaky{f}
		}
		rc, err := GetResuming(context.Background(), sp, "manifests/db1/backups/b1/manifest.json")
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("ranged=%v: %v", ranged, err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("ranged=%v: resumed stream differs from the object (%d bytes, want %d)", ranged, len(got), len(body))
		}
		if f.opens != 8 {
			t.Errorf("ranged=%v: %d opens, want 8 (7 broken, 1 clean)", ranged, f.opens)
		}
		if ranged {
			// Each resume asks for the byte after the last one delivered.
			for i, off := range f.ranged {
				if want := int64(9_000 * (i + 1)); off != want {
					t.Errorf("resume %d asked for byte %d, want %d", i, off, want)
				}
			}
		}
	}
}

// A stream that makes no progress at all gives up, and names the read
// error that started it.
func TestGetResuming_GivesUpWithoutProgress(t *testing.T) {
	noBackoff(t)
	f := &flakyObject{body: testBody(1000), burst: 0, breaks: 1 << 30}
	rc, err := GetResuming(context.Background(), rangedFlaky{f}, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, err = io.ReadAll(rc)
	if err == nil || !errors.Is(err, errReset) {
		t.Fatalf("err = %v, want one wrapping the connection reset", err)
	}
	if f.opens != 1+ResumeAttempts {
		t.Errorf("%d opens, want %d", f.opens, 1+ResumeAttempts)
	}
}

// Progress resets the count: far more resets than ResumeAttempts are
// survived as long as each attempt delivers something.
func TestGetResuming_ProgressResetsTheCount(t *testing.T) {
	noBackoff(t)
	body := testBody(4000)
	f := &flakyObject{body: body, burst: 100, breaks: 30}
	rc, err := GetResuming(context.Background(), rangedFlaky{f}, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("err=%v, %d bytes; 30 resets with progress each time must be survived", err, len(got))
	}
}

// Neither a vanished object nor a cancelled context is retried.
func TestGetResuming_DoesNotRetryNotFoundOrCancel(t *testing.T) {
	noBackoff(t)
	f := &flakyObject{body: testBody(1000), burst: 10, breaks: 1 << 30}
	rc, err := GetResuming(context.Background(), rangedFlaky{f}, "k")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := rc.Read(buf); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.gone = true
	f.mu.Unlock()
	if _, err := io.ReadAll(rc); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	_ = rc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	f2 := &flakyObject{body: testBody(1000), burst: 10, breaks: 1 << 30}
	rc, err = GetResuming(ctx, rangedFlaky{f2}, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := rc.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("a cancelled stream reported success")
	}
	if f2.opens > 2 {
		t.Errorf("%d opens after cancel", f2.opens)
	}

	if _, err := GetResuming(context.Background(), &flakyObject{gone: true}, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("opening a missing object: %v", err)
	}
}

// An object that became shorter than the resume point is reported, not
// read as an empty tail.
func TestGetFrom_SkipPastTheEnd(t *testing.T) {
	f := &flakyObject{body: testBody(100)}
	if _, err := GetFrom(context.Background(), f, "k", 500); err == nil {
		t.Fatal("resuming past the end of the object succeeded")
	}
	rc, err := GetFrom(context.Background(), f, "k", 40)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, f.body[40:]) {
		t.Fatal("skip-based GetFrom returned the wrong tail")
	}
}
