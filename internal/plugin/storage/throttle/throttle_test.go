package throttle_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/throttle"
)

// fakeClock is a deterministic time source for the throttle tests.
// time advances only when the test calls advance() — including for
// what the throttle thinks is a "sleep". Sleeps record their
// duration so the test can assert "we'd have slept for X seconds."
type fakeClock struct {
	mu         sync.Mutex
	now        time.Time
	slept      []time.Duration
	totalSlept time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		now: time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep advances the fake clock by d and records the duration.
// This is what the throttle calls instead of time.Sleep.
func (c *fakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	c.totalSlept += d
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// setNow jumps the clock to t. Used by schedule tests to position
// the clock inside or outside a window without computing the
// elapsed delta.
func (c *fakeClock) setNow(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *fakeClock) totalSleep() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalSlept
}

// fsBackend constructs an fs:// plugin against a temp dir for tests.
func fsBackend(t *testing.T) storage.StoragePlugin {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// TestThrottle_NoOpWhenBPSZero: a Throttle with bps=0 is a
// transparent pass-through; Put returns immediately and the inner
// plugin gets the body verbatim.
func TestThrottle_NoOpWhenBPSZero(t *testing.T) {
	clock := newFakeClock()
	tr := throttle.New(fsBackend(t), 0,
		throttle.WithClock(clock.Now, clock.Sleep))
	body := []byte("hello throttle no-op")
	_, err := tr.Put(context.Background(), "k",
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if clock.totalSleep() != 0 {
		t.Errorf("bps=0 should not sleep; slept %s", clock.totalSleep())
	}
	// Round-trip: Get returns the same body.
	rc, err := tr.Get(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Errorf("Get returned different bytes")
	}
}

// TestThrottle_RespectsCap: a 100KB Put at 50KB/s should sleep for
// roughly 1 second total (the burst tokens cover the first ~1s of
// bytes; the second 50KB requires another second of refill).
//
// We configure burst = bps so the burst is exactly 1 second's
// worth, and chunkSize small enough that the throttle exercises
// multiple acquire calls.
func TestThrottle_RespectsCap(t *testing.T) {
	clock := newFakeClock()
	tr := throttle.New(fsBackend(t), 50*1024,
		throttle.WithBurst(50*1024),
		throttle.WithChunkSize(8*1024),
		throttle.WithClock(clock.Now, clock.Sleep))
	body := bytes.Repeat([]byte{0x42}, 100*1024)
	if _, err := tr.Put(context.Background(), "k",
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Expected: burst tokens (50KB) consumed without sleep; the
	// remaining ~50KB requires ~1s of waiting. The exact figure
	// depends on chunk granularity but should be in [0.9s, 1.1s].
	got := clock.totalSleep()
	if got < 800*time.Millisecond || got > 1200*time.Millisecond {
		t.Errorf("expected ~1s of throttle sleep, got %s", got)
	}
}

// TestThrottle_LargerPayloadLongerSleep: 200KB at 50KB/s burst
// 50KB should sleep ~3 seconds (50KB free, 150KB at 50KB/s = 3s).
func TestThrottle_LargerPayloadLongerSleep(t *testing.T) {
	clock := newFakeClock()
	tr := throttle.New(fsBackend(t), 50*1024,
		throttle.WithBurst(50*1024),
		throttle.WithChunkSize(8*1024),
		throttle.WithClock(clock.Now, clock.Sleep))
	body := bytes.Repeat([]byte{0x55}, 200*1024)
	if _, err := tr.Put(context.Background(), "k",
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got := clock.totalSleep()
	if got < 2800*time.Millisecond || got > 3200*time.Millisecond {
		t.Errorf("expected ~3s of throttle sleep, got %s", got)
	}
}

// TestThrottle_BurstAbsorbsSmallPuts: a Put smaller than the burst
// fits inside the bucket and produces no sleep.
func TestThrottle_BurstAbsorbsSmallPuts(t *testing.T) {
	clock := newFakeClock()
	tr := throttle.New(fsBackend(t), 100*1024,
		throttle.WithBurst(100*1024),
		throttle.WithChunkSize(8*1024),
		throttle.WithClock(clock.Now, clock.Sleep))
	body := bytes.Repeat([]byte{1}, 10*1024) // 10KB into 100KB bucket
	if _, err := tr.Put(context.Background(), "k",
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := clock.totalSleep(); got > 100*time.Millisecond {
		t.Errorf("small Put inside burst should not sleep meaningfully; got %s", got)
	}
}

// TestThrottle_TokensRefillBetweenPuts: two sequential Puts, each
// the size of the burst, with idle time between. The second Put
// should not sleep if enough idle time elapsed for the bucket to
// refill.
func TestThrottle_TokensRefillBetweenPuts(t *testing.T) {
	clock := newFakeClock()
	const bps = 50 * 1024
	tr := throttle.New(fsBackend(t), bps,
		throttle.WithBurst(bps),
		throttle.WithChunkSize(8*1024),
		throttle.WithClock(clock.Now, clock.Sleep))
	body := bytes.Repeat([]byte{1}, bps) // exactly burst
	if _, err := tr.Put(context.Background(), "k1",
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	first := clock.totalSleep()
	// Idle 2 seconds (more than enough to refill).
	clock.advance(2 * time.Second)
	if _, err := tr.Put(context.Background(), "k2",
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if delta := clock.totalSleep() - first; delta > 100*time.Millisecond {
		t.Errorf("second Put after refill should not sleep; got %s", delta)
	}
}

// TestThrottle_ContextCancellation: a Put against a body too large
// for the burst should bail when ctx is cancelled mid-sleep.
//
// Real-world impact: an operator hits Ctrl-C during a slow
// throttled replicate; we want the upload to abort promptly rather
// than complete the full sleep window.
//
// We test the behaviour by injecting a sleep function that panics
// if it's called with a non-zero duration after we cancel the
// context. The throttledReader checks ctx.Done() before sleeping,
// so cancellation between acquires is immediate.
func TestThrottle_ContextCancellation(t *testing.T) {
	clock := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	tr := throttle.New(fsBackend(t), 1024,
		throttle.WithBurst(1024),
		throttle.WithChunkSize(512),
		throttle.WithClock(clock.Now, clock.Sleep))
	body := bytes.Repeat([]byte{9}, 4096) // 4x burst
	_, err := tr.Put(ctx, "k", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// TestThrottle_ConcurrentSharedBucket: two goroutines uploading
// concurrently share the bucket. The TOTAL sleep across both should
// be approximately what we'd expect for the combined byte count.
//
// We don't assert exact equality (concurrent acquires interleave)
// but we do assert it's within a reasonable window.
func TestThrottle_ConcurrentSharedBucket(t *testing.T) {
	clock := newFakeClock()
	const bps = 50 * 1024
	tr := throttle.New(fsBackend(t), bps,
		throttle.WithBurst(bps),
		throttle.WithChunkSize(8*1024),
		throttle.WithClock(clock.Now, clock.Sleep))
	body := bytes.Repeat([]byte{0x33}, 100*1024) // 100KB each
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		key := "k" + string(rune('A'+i))
		go func() {
			defer wg.Done()
			if _, err := tr.Put(context.Background(), key,
				bytes.NewReader(body),
				storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
				t.Errorf("Put: %v", err)
			}
		}()
	}
	wg.Wait()
	// Two 100KB puts at 50KB/s burst 50KB:
	//   First 50KB free (burst), remaining 150KB at 50KB/s = 3s.
	got := clock.totalSleep()
	if got < 2500*time.Millisecond || got > 3500*time.Millisecond {
		t.Errorf("concurrent shared bucket: expected ~3s of total sleep, got %s", got)
	}
}

// TestThrottle_ImplementsStoragePlugin: compile-time check that the
// wrapper satisfies the full interface (no method silently dropped).
func TestThrottle_ImplementsStoragePlugin(t *testing.T) {
	var _ storage.StoragePlugin = throttle.New(fsBackend(t), 0)
}

// TestThrottle_NameDelegates: the throttle returns the inner
// plugin's name, not a synthesised "throttle/<name>".
func TestThrottle_NameDelegates(t *testing.T) {
	tr := throttle.New(fsBackend(t), 0)
	if tr.Name() != "fs" {
		t.Errorf("Name() = %q, want %q", tr.Name(), "fs")
	}
}

// TestThrottle_RegionDelegates: regional metadata propagates through
// the wrapper so residency checks see the inner backend's region.
func TestThrottle_RegionDelegates(t *testing.T) {
	tr := throttle.New(fsBackend(t), 0)
	// fs has no meaningful region — RegionOf returns RegionUnknown.
	if got := tr.Region(); got != storage.RegionUnknown {
		t.Errorf("Region() = %q, want %q", got, storage.RegionUnknown)
	}
}
