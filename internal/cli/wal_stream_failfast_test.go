package cli

// wal_stream_failfast_test.go — a stream that cannot succeed must stop,
// and must say so.
//
// Reported as issue #45. A repository backend that could not commit a
// manifest (an S3-compatible store answering NotImplemented to the
// conditional COPY behind RenameIfNotExists) produced no output at all:
// the slot went active, chunks accumulated, `wal list` stayed empty,
// memory grew, and the process was OOM-killed and restarted from the
// same LSN. Nothing in the logs said why.
//
// Two independent defects made that possible, and both are covered
// here:
//
//   - only pre-stream SETUP errors were classified as permanent, so a
//     mid-stream failure was retried forever no matter what it was;
//   - --output json suppressed every event, including warnings, so the
//     reconnect loop it did emit was invisible under the renderer a
//     Kubernetes deployment would obviously pick.
//
// In-package: both are unexported decision functions, and reaching
// them through a real stream would need a PG container plus a backend
// that fails a specific way — testing the plumbing rather than the
// judgement.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// TestIsPermanentStreamError_StopsOnUnfixableFailures covers the
// classifier directly.
func TestIsPermanentStreamError_StopsOnUnfixableFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "backend does not implement the operation",
			// A backend missing a capability will not have grown it by
			// the next attempt.
			err:  fmt.Errorf("walsink: rename manifest: %w", storage.ErrUnsupported),
			want: true,
		},
		{
			name: "split-brain",
			// Two clusters archiving into one lineage. Retrying cannot
			// resolve it, and each attempt risks interleaving more
			// foreign WAL.
			err: fmt.Errorf("splitbrain.system_identifier_mismatch: segment X already " +
				"archived by cluster 123"),
			want: true,
		},
		{
			name: "operator input",
			err:  output.NewError("usage.bad_pg_dsn", "malformed DSN"),
			want: true,
		},
		{
			name: "connection dropped — the reason the retry loop exists",
			err:  errors.New("unexpected EOF"),
			want: false,
		},
		{
			name: "primary draining during failover",
			err:  errors.New("replication: primary is draining"),
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPermanentStreamError(tc.err); got != tc.want {
				t.Errorf("isPermanentStreamError(%v) = %v, want %v.\nClassifying a "+
					"transient failure as permanent breaks failover recovery; classifying "+
					"a permanent one as transient is what produced the silent loop in #45",
					tc.err, got, tc.want)
			}
		})
	}
}

// TestNoProgressBackstopIsBounded pins the backstop for permanent
// failures nobody enumerated — which is the case that actually bit.
//
// The reported error was a raw "NotImplemented" from the storage layer
// with no code to match on, so no classifier would have caught it.
// Judging by outcome covers that: attempts that end in error having
// synced nothing are making no progress, and a run of them means
// retrying is not working, whatever the cause.
func TestNoProgressBackstopIsBounded(t *testing.T) {
	if maxNoProgressAttempts <= 0 {
		t.Fatal("maxNoProgressAttempts must be positive, or the loop stops on the first " +
			"transient blip and a failover can never recover")
	}
	if maxNoProgressAttempts > 20 {
		t.Errorf("maxNoProgressAttempts = %d: with the escalating backoff that is long "+
			"enough for a permanently-broken repository to buffer its way to an OOM, "+
			"which is the failure being fixed", maxNoProgressAttempts)
	}
}

// TestShouldEmitEvent covers the renderer decision directly.
//
// The first version of this fix wrote the comparison the intuitive way
// round and suppressed ERRORS while keeping progress chatter — the
// exact inverse of the intent — because severity is syslog-ordered.
// The earlier test asserted the ordering CONSTANTS and passed anyway,
// which is why the decision is now a function and the test drives it.
func TestShouldEmitEvent(t *testing.T) {
	severe := []output.Severity{
		output.SeverityEmergency, output.SeverityAlert,
		output.SeverityCritical, output.SeverityError, output.SeverityWarning,
	}
	chatter := []output.Severity{
		output.SeverityNotice, output.SeverityInfo, output.SeverityDebug,
	}

	// Without suppression every severity is emitted.
	for _, sev := range append(append([]output.Severity{}, severe...), chatter...) {
		if !shouldEmitEvent(false, sev) {
			t.Errorf("severity %v dropped with suppression off", sev)
		}
	}
	// With suppression, anything an operator must act on survives.
	for _, sev := range severe {
		if !shouldEmitEvent(true, sev) {
			t.Errorf("severity %v suppressed under --output json; a failure an operator "+
				"must act on cannot depend on the renderer they chose. This is issue #45: "+
				"a streamer failing every attempt emitted warnings that went nowhere", sev)
		}
	}
	// ...and progress chatter, which the suppression exists for, does not.
	for _, sev := range chatter {
		if shouldEmitEvent(true, sev) {
			t.Errorf("severity %v survives json suppression; a JSON consumer parses the "+
				"final Result, not a progress stream", sev)
		}
	}
}

// TestDecideStreamStop covers the loop's actual stop decision.
//
// Testing isPermanentStreamError alone said nothing about whether the
// retry loop consults it — and that gap is the bug: the classifier
// existed for setup errors while mid-stream failures were retried
// unconditionally.
func TestDecideStreamStop(t *testing.T) {
	transient := errors.New("unexpected EOF")
	permanent := fmt.Errorf("walsink: rename manifest: %w", storage.ErrUnsupported)

	t.Run("transient failure keeps reconnecting", func(t *testing.T) {
		if _, _, stop := decideStreamStop(transient, 1); stop {
			t.Error("stopped on a transient failure after one attempt; the reconnect loop " +
				"exists to survive a failover")
		}
	})

	t.Run("permanent failure stops immediately", func(t *testing.T) {
		code, msg, stop := decideStreamStop(permanent, 1)
		if !stop {
			t.Fatal("a backend that does not implement the operation was retried; it will " +
				"fail identically on every attempt (issue #45)")
		}
		if code != "wal.stream_permanent" {
			t.Errorf("code = %q, want wal.stream_permanent", code)
		}
		if !strings.Contains(msg, "not supported") {
			t.Errorf("message does not carry the underlying cause: %q", msg)
		}
	})

	t.Run("unrecognised failure stops via the no-progress backstop", func(t *testing.T) {
		// The reported error was a raw "NotImplemented" with no code to
		// match on, so only the backstop catches it.
		raw := errors.New("s3: copy \"a\" -> \"b\": NotImplemented")
		if _, _, stop := decideStreamStop(raw, maxNoProgressAttempts-1); stop {
			t.Error("stopped before the backstop threshold")
		}
		code, msg, stop := decideStreamStop(raw, maxNoProgressAttempts)
		if !stop {
			t.Fatalf("still retrying after %d attempts that synced nothing — this is the "+
				"loop that buffered its way to an OOM", maxNoProgressAttempts)
		}
		if code != "wal.stream_no_progress" {
			t.Errorf("code = %q, want wal.stream_no_progress", code)
		}
		if !strings.Contains(msg, "NotImplemented") {
			t.Errorf("message must carry the last error, or the operator learns only that "+
				"it gave up: %q", msg)
		}
	})

	t.Run("the two stop reasons are distinguishable", func(t *testing.T) {
		p, _, _ := decideStreamStop(permanent, 1)
		n, _, _ := decideStreamStop(errors.New("x"), maxNoProgressAttempts)
		if p == n {
			t.Error("a recognised permanent condition and the no-progress backstop report " +
				"the same code; the first names a cause, the second says only that " +
				"retrying is not working")
		}
	})
}

// repoRootForWalTest locates the checkout from this file's path.
func repoRootForWalTest(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
}

func readWalSource(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "internal", "cli", "wal.go"))
	if err != nil {
		t.Fatalf("read wal.go: %v", err)
	}
	return string(b)
}

// TestStreamLoopUsesTheDecisions checks the wiring.
//
// The decision functions above are only worth testing if the loop
// consults them. A call site that bypasses either — an unconditional
// `if suppressEvents`, or a loop that never asks whether to stop —
// leaves every test above green while restoring the exact behaviour of
// issue #45: a permanent failure retried forever, emitting nothing.
//
// Source-level rather than behavioural: driving it through
// runWalStream needs a PG container plus a backend rigged to fail a
// specific way, which tests the plumbing rather than the wiring.
func TestStreamLoopUsesTheDecisions(t *testing.T) {
	src := readWalSource(t, repoRootForWalTest(t))

	if !strings.Contains(src, "shouldEmitEvent(suppressEvents, e.Severity)") {
		t.Error("the emit closure no longer routes through shouldEmitEvent, so warnings " +
			"and errors are suppressed under --output json again (issue #45)")
	}
	if !strings.Contains(src, "decideStreamStop(streamErr, noProgress)") {
		t.Error("the reconnect loop no longer consults decideStreamStop, so a permanent " +
			"mid-stream failure is retried forever again (issue #45)")
	}
	for _, code := range []string{"wal.stream_permanent", "wal.stream_no_progress"} {
		if !strings.Contains(src, code) {
			t.Errorf("wal.go no longer emits %q — a stop reason lost its name", code)
		}
	}
}
