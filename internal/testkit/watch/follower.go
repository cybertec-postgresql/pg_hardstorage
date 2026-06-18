// follower.go — FollowOptions + file follower: tail-and-poll NDJSON-style soak event logs.
package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// FollowOptions tunes the file follower.
type FollowOptions struct {
	// PollInterval is the gap between EOF-then-retry attempts.
	// 250 ms is a good balance between TUI responsiveness and
	// not pegging the CPU on a quiet soak.  0 → 250 ms default.
	PollInterval time.Duration

	// FromBeginning seeks to byte 0 first; otherwise starts at
	// EOF.  "From beginning" is right for `watch <run-dir>` (the
	// operator wants to see history); "from EOF" is right for
	// log-tailing modes we may add later.
	FromBeginning bool
}

// ResolveEventsPath turns a user-supplied path into the events
// file we should follow.  Accepts:
//
//   - a directory containing events.ndjson (the soak case)
//   - a directory containing result.ndjson (scenario-run
//     artefact dir; `watch` works on those too)
//   - a direct path to either file
//
// Returns the chosen path, or an error if no candidate
// exists.  Searched in priority order so a run-dir that
// happens to contain both prefers events.ndjson (soak primary).
func ResolveEventsPath(p string) (string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("watch: %w", err)
	}
	if !st.IsDir() {
		return p, nil
	}
	for _, name := range []string{"events.ndjson", "result.ndjson"} {
		candidate := filepath.Join(p, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("watch: no events.ndjson or result.ndjson under %s", p)
}

// Follow streams Events out of path, calling onEvent for each
// successfully-decoded line.  Returns when ctx is cancelled or
// a non-recoverable error occurs.
//
// Rotation / truncation is intentionally NOT handled — the
// soak writes one events.ndjson per run and never rotates, so
// supporting rotation would be carrying weight for nobody.
// A run that finishes simply stops appending; Follow keeps
// polling until the operator quits the TUI.
//
// Bad lines (malformed JSON) are silently skipped so a single
// half-written line during the operator's `Ctrl-C → tail file`
// race can't kill the watcher.
func Follow(ctx context.Context, path string, opts FollowOptions, onEvent func(Event)) error {
	if opts.PollInterval == 0 {
		opts.PollInterval = 250 * time.Millisecond
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("watch: open %s: %w", path, err)
	}
	defer f.Close()

	if !opts.FromBeginning {
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return fmt.Errorf("watch: seek end: %w", err)
		}
	}

	r := bufio.NewReader(f)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		line, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) {
			// Wait, then retry the same reader (preserves any
			// half-line we already buffered).
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(opts.PollInterval):
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("watch: read %s: %w", path, err)
		}
		if len(line) == 0 || line[0] == '\n' {
			continue
		}
		var ev Event
		if jerr := json.Unmarshal([]byte(line), &ev); jerr != nil {
			// Skip the bad line; keep going.  This is the
			// right call for "operator opened the file mid-
			// write" and for scenario-runner lines whose shape
			// doesn't match validate.Event (those drop into
			// the tail with empty Cell, which is fine).
			continue
		}
		onEvent(ev)
	}
}
