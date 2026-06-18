package faultinject_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/faultinject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// fsBackend constructs an fs:// backend against a temp dir.
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

// putBytes writes body at key via sp; helper for setting up Get tests.
func putBytes(t *testing.T, sp storage.StoragePlugin, key string, body []byte) {
	t.Helper()
	if _, err := sp.Put(context.Background(), key,
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// TestFaultInject_PassthroughWhenInactive: with no rules installed,
// every method delegates cleanly. No fault is injected.
func TestFaultInject_PassthroughWhenInactive(t *testing.T) {
	sp := fsBackend(t)
	mw := faultinject.New(sp)

	body := []byte("passthrough")
	if _, err := mw.Put(context.Background(), "k", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Errorf("Put should pass through: %v", err)
	}
	rc, err := mw.Get(context.Background(), "k")
	if err != nil {
		t.Errorf("Get should pass through: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Errorf("Get returned wrong bytes: %q", got)
	}
	if mw.IsActive() {
		t.Error("IsActive should be false with no rules installed")
	}
}

// TestFaultInject_PutRuleFires: a Put rule with no key-prefix and
// no MaxFires returns the configured error on every Put while
// active.
func TestFaultInject_PutRuleFires(t *testing.T) {
	sp := fsBackend(t)
	mw := faultinject.New(sp)
	mw.Activate([]faultinject.Rule{{
		Name: "always-fail-put",
		Ops:  faultinject.OpPut,
		Err:  faultinject.ErrInjected,
	}}, faultinject.ActivateOptions{})

	if !mw.IsActive() {
		t.Error("IsActive should be true after Activate with rules")
	}

	for i := 0; i < 3; i++ {
		_, err := mw.Put(context.Background(), "k",
			bytes.NewReader([]byte("x")),
			storage.PutOptions{ContentLength: 1})
		if !errors.Is(err, faultinject.ErrInjected) {
			t.Errorf("iter %d: expected ErrInjected, got %v", i, err)
		}
	}

	stats := mw.Stats()
	if len(stats) != 1 || stats[0].Hits != 3 {
		t.Errorf("stats=%+v, want hits=3", stats)
	}
}

// TestFaultInject_OpsFilter: a rule that only targets OpGet does
// not affect Put. A separate Put against the same wrapped plugin
// succeeds.
func TestFaultInject_OpsFilter(t *testing.T) {
	sp := fsBackend(t)
	mw := faultinject.New(sp)
	putBytes(t, sp, "k", []byte("ok"))

	mw.Activate([]faultinject.Rule{{
		Name: "only-get-fails",
		Ops:  faultinject.OpGet,
		Err:  faultinject.ErrInjected,
	}}, faultinject.ActivateOptions{})

	// Put succeeds (rule is Get-only).
	if _, err := mw.Put(context.Background(), "k2",
		bytes.NewReader([]byte("y")),
		storage.PutOptions{ContentLength: 1}); err != nil {
		t.Errorf("Put should not be affected by Get-only rule: %v", err)
	}
	// Get fails.
	if _, err := mw.Get(context.Background(), "k"); !errors.Is(err, faultinject.ErrInjected) {
		t.Errorf("Get should fire Get-only rule: %v", err)
	}
}

// TestFaultInject_KeyPrefix: a rule with a KeyPrefix only fires
// for matching keys. Non-matching keys pass through.
func TestFaultInject_KeyPrefix(t *testing.T) {
	sp := fsBackend(t)
	mw := faultinject.New(sp)
	mw.Activate([]faultinject.Rule{{
		Name:      "chunks-fail-only",
		Ops:       faultinject.OpPut,
		KeyPrefix: "chunks/",
		Err:       faultinject.ErrInjected,
	}}, faultinject.ActivateOptions{})

	// Matching key fires the rule.
	_, err := mw.Put(context.Background(), "chunks/aa/bb/cc.chk",
		bytes.NewReader([]byte("x")),
		storage.PutOptions{ContentLength: 1})
	if !errors.Is(err, faultinject.ErrInjected) {
		t.Errorf("chunks/ key should fire: %v", err)
	}
	// Non-matching key passes through.
	_, err = mw.Put(context.Background(), "manifests/db1/x.json",
		bytes.NewReader([]byte("x")),
		storage.PutOptions{ContentLength: 1})
	if err != nil {
		t.Errorf("manifests/ key should not fire: %v", err)
	}
}

// TestFaultInject_MaxFires: a rule with MaxFires=N fires N times,
// then is exhausted and subsequent calls pass through.
func TestFaultInject_MaxFires(t *testing.T) {
	sp := fsBackend(t)
	mw := faultinject.New(sp)
	mw.Activate([]faultinject.Rule{{
		Name:     "first-two-fail",
		Ops:      faultinject.OpPut,
		Err:      faultinject.ErrInjected,
		MaxFires: 2,
	}}, faultinject.ActivateOptions{})

	for i := 0; i < 4; i++ {
		_, err := mw.Put(context.Background(),
			"k", bytes.NewReader([]byte("x")),
			storage.PutOptions{ContentLength: 1})
		if i < 2 {
			if !errors.Is(err, faultinject.ErrInjected) {
				t.Errorf("iter %d: expected fault, got %v", i, err)
			}
		} else {
			// fs's IfNotExists default would return AlreadyExists
			// after the first successful write — we bypass that by
			// using a different key per iter.
			// Use a unique key for each iteration above to avoid
			// noise; here the i>=2 cases need to NOT fire the
			// fault but DO need to succeed at the backend. We
			// craft the test with unique keys to keep it simple.
		}
	}
	// Re-run with unique keys for the post-exhaustion verifications.
	for i := 0; i < 2; i++ {
		key := "k-post-" + string(rune('0'+i))
		_, err := mw.Put(context.Background(), key,
			bytes.NewReader([]byte("x")),
			storage.PutOptions{ContentLength: 1})
		if err != nil {
			t.Errorf("post-exhaustion iter %d: expected pass-through, got %v", i, err)
		}
	}
	stats := mw.Stats()
	if len(stats) != 1 || stats[0].Hits != 2 {
		t.Errorf("stats=%+v, want hits=2 (cap)", stats)
	}
}

// TestFaultInject_FirstMatchingRuleWins: with multiple rules, the
// first one that matches fires. Lower-priority rules do not see
// the request.
func TestFaultInject_FirstMatchingRuleWins(t *testing.T) {
	errA := errors.New("A")
	errB := errors.New("B")
	sp := fsBackend(t)
	mw := faultinject.New(sp)
	mw.Activate([]faultinject.Rule{
		{Name: "A-first", Ops: faultinject.OpPut, KeyPrefix: "chunks/", Err: errA},
		{Name: "B-second", Ops: faultinject.OpPut, KeyPrefix: "chunks/", Err: errB},
	}, faultinject.ActivateOptions{})

	_, err := mw.Put(context.Background(), "chunks/k",
		bytes.NewReader([]byte("x")),
		storage.PutOptions{ContentLength: 1})
	if !errors.Is(err, errA) {
		t.Errorf("expected first-match (A) to fire, got %v", err)
	}
	stats := mw.Stats()
	if len(stats) != 2 {
		t.Fatalf("wrong stats len: %d", len(stats))
	}
	if stats[0].Hits != 1 || stats[1].Hits != 0 {
		t.Errorf("stats=%+v, want A=1 B=0", stats)
	}
}

// TestFaultInject_TimeBoundedExpires: a Middleware activated with
// an ActiveDuration auto-expires past the window. We use the
// fake-clock helper to advance past it deterministically.
func TestFaultInject_TimeBoundedExpires(t *testing.T) {
	sp := fsBackend(t)
	now := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	mw := faultinject.New(sp).WithClock(func() time.Time { return now })
	mw.Activate([]faultinject.Rule{{
		Name: "fail-for-5s",
		Ops:  faultinject.OpPut,
		Err:  faultinject.ErrInjected,
	}}, faultinject.ActivateOptions{ActiveDuration: 5 * time.Second})

	if !mw.IsActive() {
		t.Error("should be active immediately after Activate")
	}
	// Within window: rule fires.
	_, err := mw.Put(context.Background(), "k",
		bytes.NewReader([]byte("x")),
		storage.PutOptions{ContentLength: 1})
	if !errors.Is(err, faultinject.ErrInjected) {
		t.Errorf("within window should fault: %v", err)
	}

	// Advance past the window.
	now = now.Add(10 * time.Second)
	if mw.IsActive() {
		t.Error("should be inactive past ActiveDuration")
	}
	_, err = mw.Put(context.Background(), "k2",
		bytes.NewReader([]byte("x")),
		storage.PutOptions{ContentLength: 1})
	if err != nil {
		t.Errorf("past window should pass through: %v", err)
	}
}

// TestFaultInject_Deactivate: explicit Deactivate clears all rules
// and resets state. Stats() reports nothing.
func TestFaultInject_Deactivate(t *testing.T) {
	sp := fsBackend(t)
	mw := faultinject.New(sp)
	mw.Activate([]faultinject.Rule{{
		Name: "x", Ops: faultinject.OpPut, Err: faultinject.ErrInjected,
	}}, faultinject.ActivateOptions{})
	if !mw.IsActive() {
		t.Fatal("should be active after Activate")
	}
	mw.Deactivate()
	if mw.IsActive() {
		t.Error("should be inactive after Deactivate")
	}
	if got := mw.Stats(); len(got) != 0 {
		t.Errorf("stats should be empty after Deactivate; got %+v", got)
	}
	// Subsequent Put should pass through.
	_, err := mw.Put(context.Background(), "k",
		bytes.NewReader([]byte("x")),
		storage.PutOptions{ContentLength: 1})
	if err != nil {
		t.Errorf("after Deactivate, Put should pass through: %v", err)
	}
}

// TestFaultInject_ListRule: a rule on OpList returns the configured
// error via the iter.Seq2 single-yield (zero,err) shape that List
// uses for fatal errors.
func TestFaultInject_ListRule(t *testing.T) {
	sp := fsBackend(t)
	mw := faultinject.New(sp)
	mw.Activate([]faultinject.Rule{{
		Name: "list-fails",
		Ops:  faultinject.OpList,
		Err:  faultinject.ErrInjected,
	}}, faultinject.ActivateOptions{})

	for _, lerr := range mw.List(context.Background(), "any/") {
		_ = lerr
	}
	// Better: collect the err.
	var sawErr error
	for _, e := range mw.List(context.Background(), "any/") {
		if e != nil {
			sawErr = e
			break
		}
	}
	if !errors.Is(sawErr, faultinject.ErrInjected) {
		t.Errorf("List should yield ErrInjected; got %v", sawErr)
	}
}

// TestFaultInject_OpStringForms: the Op String helper renders the
// bitmask in a stable form for Stats / log lines.
func TestFaultInject_OpStringForms(t *testing.T) {
	cases := []struct {
		op   faultinject.Op
		want string
	}{
		{0, "none"},
		{faultinject.AllOps, "all"},
		{faultinject.OpPut, "Put"},
		{faultinject.OpPut | faultinject.OpGet, "Put,Get"},
	}
	for _, c := range cases {
		if got := c.op.String(); got != c.want {
			t.Errorf("Op(%d).String() = %q, want %q", c.op, got, c.want)
		}
	}
}

// TestFaultInject_NameAndRegionDelegate: the wrapper is transparent
// for backend identity (Name) and region.
func TestFaultInject_NameAndRegionDelegate(t *testing.T) {
	mw := faultinject.New(fsBackend(t))
	if mw.Name() != "fs" {
		t.Errorf("Name() = %q, want fs", mw.Name())
	}
	if got := mw.Region(); got != storage.RegionUnknown {
		t.Errorf("Region() = %q, want RegionUnknown", got)
	}
}

// TestFaultInject_ImplementsStoragePlugin: compile-time check the
// wrapper satisfies the full interface.
func TestFaultInject_ImplementsStoragePlugin(t *testing.T) {
	var _ storage.StoragePlugin = faultinject.New(fsBackend(t))
}

// TestFaultInject_ConcurrentSafe: concurrent Puts hammer the rule
// list + counters; race detector should be silent and the total
// hit count should match the per-rule cap.
func TestFaultInject_ConcurrentSafe(t *testing.T) {
	sp := fsBackend(t)
	mw := faultinject.New(sp)
	mw.Activate([]faultinject.Rule{{
		Name:     "first-50-fail",
		Ops:      faultinject.OpPut,
		Err:      faultinject.ErrInjected,
		MaxFires: 50,
	}}, faultinject.ActivateOptions{})

	const N = 200
	var failures int
	failuresCh := make(chan int, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			_, err := mw.Put(context.Background(),
				"k-"+string(rune('A'+i%26))+"-"+timeStamp(i),
				bytes.NewReader([]byte("x")),
				storage.PutOptions{ContentLength: 1})
			if errors.Is(err, faultinject.ErrInjected) {
				failuresCh <- 1
			} else {
				failuresCh <- 0
			}
		}(i)
	}
	for i := 0; i < N; i++ {
		failures += <-failuresCh
	}
	if failures != 50 {
		t.Errorf("expected exactly 50 injected failures (MaxFires=50); got %d", failures)
	}
}

// timeStamp produces a simple unique suffix per goroutine so the
// fs backend's IfNotExists semantics don't collide.
func timeStamp(i int) string {
	const hex = "0123456789abcdef"
	var buf [8]byte
	for j := 0; j < 8; j++ {
		buf[j] = hex[(i>>(j*4))&0xf]
	}
	return strings.ReplaceAll(string(buf[:]), "0", "z") // make hex visually distinct
}
