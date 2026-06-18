package watch_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/watch"
)

func TestResolveEventsPath_DirectFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := watch.ResolveEventsPath(path)
	if err != nil {
		t.Fatalf("direct file path: %v", err)
	}
	if got != path {
		t.Errorf("got %q want %q", got, path)
	}
}

func TestResolveEventsPath_RunDirPrefersEvents(t *testing.T) {
	// Soak primary: when both exist, events.ndjson wins because
	// it's the soak's canonical output and result.ndjson would
	// only co-exist as scenario debris from a re-used run-dir.
	dir := t.TempDir()
	events := filepath.Join(dir, "events.ndjson")
	result := filepath.Join(dir, "result.ndjson")
	for _, p := range []string{events, result} {
		if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := watch.ResolveEventsPath(dir)
	if err != nil {
		t.Fatalf("dir resolve: %v", err)
	}
	if got != events {
		t.Errorf("got %q want %q (events.ndjson takes priority)", got, events)
	}
}

func TestResolveEventsPath_RunDirFallsBackToResult(t *testing.T) {
	// Scenario artefact dir: only result.ndjson exists.
	dir := t.TempDir()
	result := filepath.Join(dir, "result.ndjson")
	if err := os.WriteFile(result, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := watch.ResolveEventsPath(dir)
	if err != nil {
		t.Fatalf("dir resolve: %v", err)
	}
	if got != result {
		t.Errorf("got %q want %q", got, result)
	}
}

func TestResolveEventsPath_EmptyDirErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := watch.ResolveEventsPath(dir)
	if err == nil {
		t.Errorf("empty dir should error, not return ''")
	}
}

func TestResolveEventsPath_MissingErrors(t *testing.T) {
	_, err := watch.ResolveEventsPath("/no/such/path/here")
	if err == nil {
		t.Errorf("missing path should error")
	}
}

// TestFollow_StreamsAppendedLines exercises the load-bearing
// property: the follower picks up lines that were appended
// AFTER it started reading.  Without this the live-view would
// only show snapshot-on-startup, defeating the whole point.
func TestFollow_StreamsAppendedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	// First two events written before Follow starts.
	enc := json.NewEncoder(f)
	for i := 0; i < 2; i++ {
		_ = enc.Encode(watch.Event{
			At: time.Now(), Cell: "c", Op: "iter_start", Iteration: i,
		})
	}

	// Channel for the goroutine to deliver received events.
	got := make(chan watch.Event, 16)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = watch.Follow(ctx, path, watch.FollowOptions{
			FromBeginning: true,
			PollInterval:  20 * time.Millisecond,
		}, func(ev watch.Event) {
			got <- ev
		})
	}()

	// Receive the two pre-existing events.
	for i := 0; i < 2; i++ {
		select {
		case ev := <-got:
			if ev.Iteration != i {
				t.Errorf("event %d: got iter %d, want %d", i, ev.Iteration, i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for event %d", i)
		}
	}

	// Append three more.  The follower should pick them up.
	for i := 2; i < 5; i++ {
		_ = enc.Encode(watch.Event{
			At: time.Now(), Cell: "c", Op: "iter_start", Iteration: i,
		})
	}
	_ = f.Sync()

	for i := 2; i < 5; i++ {
		select {
		case ev := <-got:
			if ev.Iteration != i {
				t.Errorf("appended event %d: got iter %d, want %d", i, ev.Iteration, i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for appended event %d", i)
		}
	}

	cancel()
	wg.Wait()
	_ = f.Close()
}

// TestFollow_SkipsBadLines locks the "operator opened the file
// during a half-write" tolerance.  A malformed line must not
// kill the watcher.
func TestFollow_SkipsBadLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	body := []byte(
		`{"at":"2026-05-06T10:00:00Z","cell":"c","op":"iter_start"}` + "\n" +
			"this is not json at all\n" +
			`{"at":"2026-05-06T10:00:01Z","cell":"c","op":"iter_start","iteration":2}` + "\n",
	)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	got := make(chan watch.Event, 16)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		_ = watch.Follow(ctx, path, watch.FollowOptions{
			FromBeginning: true,
			PollInterval:  10 * time.Millisecond,
		}, func(ev watch.Event) { got <- ev })
	}()

	var iters []int
	for {
		select {
		case ev := <-got:
			iters = append(iters, ev.Iteration)
			if len(iters) >= 2 {
				goto done
			}
		case <-ctx.Done():
			goto done
		}
	}
done:
	if len(iters) != 2 || iters[0] != 0 || iters[1] != 2 {
		t.Errorf("expected to skip bad line and recover; got iters %v", iters)
	}
}
