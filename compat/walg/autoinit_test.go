package walg

import (
	"bytes"
	"reflect"
	"testing"
)

// fakeDispatch lets tests substitute the dispatchNativeCapture
// var to script multi-call sequences and assert what the shim
// invoked.
type fakeDispatch struct {
	// responses is consumed in order, one per dispatch call.
	responses []dispatchResult
	// calls records the args of every dispatch in order.
	calls [][]string
}

func (f *fakeDispatch) handle(args []string) dispatchResult {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(f.responses) == 0 {
		// Default: success, no output.
		return dispatchResult{ExitCode: 0}
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	return r
}

// withFakeDispatch swaps dispatchNativeCapture for the test's
// duration.  Restores on test cleanup.
func withFakeDispatch(t *testing.T, f *fakeDispatch) {
	t.Helper()
	orig := dispatchNativeCapture
	dispatchNativeCapture = f.handle
	t.Cleanup(func() { dispatchNativeCapture = orig })
}

func TestDispatchWithAutoInit_HappyPath(t *testing.T) {
	// Single push, succeeds straight off; no init runs.
	f := &fakeDispatch{
		responses: []dispatchResult{
			{ExitCode: 0, Stdout: []byte(`{"result":"ok"}`)},
		},
	}
	withFakeDispatch(t, f)

	var audit bytes.Buffer
	rc := dispatchWithAutoInit(&audit, "s3://example/repo",
		[]string{"wal", "push", "deployment", "/wal/000.."})
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
	if len(f.calls) != 1 {
		t.Errorf("expected 1 dispatch; got %d (%v)", len(f.calls), f.calls)
	}
	if audit.Len() != 0 {
		t.Errorf("happy path should not write to audit; got %q", audit.String())
	}
}

func TestDispatchWithAutoInit_AutoInitOnNotfoundRepo(t *testing.T) {
	// Push fails with notfound.repo → init runs → push retries
	// and succeeds.
	pushArgs := []string{"wal", "push", "deployment", "/wal/000.."}
	f := &fakeDispatch{
		responses: []dispatchResult{
			// 1st push: notfound.repo
			{
				ExitCode: 6, // ExitNotFound
				Stdout: []byte(`{
                    "schema": "pg_hardstorage.v1",
                    "command": "pg_hardstorage wal push",
                    "error": {"code": "notfound.repo", "message": "no repo"}
                }`),
			},
			// repo init: success
			{ExitCode: 0, Stdout: []byte(`{"result":{"id":"abc"}}`)},
			// 2nd push: success
			{ExitCode: 0, Stdout: []byte(`{"result":"ok"}`)},
		},
	}
	withFakeDispatch(t, f)

	var audit bytes.Buffer
	rc := dispatchWithAutoInit(&audit, "s3://example/repo", pushArgs)
	if rc != 0 {
		t.Errorf("rc = %d, want 0 (auto-init should have rescued)", rc)
	}
	if len(f.calls) != 3 {
		t.Fatalf("expected 3 dispatches (push, init, push); got %d (%v)", len(f.calls), f.calls)
	}
	if !reflect.DeepEqual(f.calls[0], pushArgs) {
		t.Errorf("call 0 = %v, want push args", f.calls[0])
	}
	wantInit := []string{"repo", "init", "s3://example/repo"}
	if !reflect.DeepEqual(f.calls[1], wantInit) {
		t.Errorf("call 1 = %v, want %v", f.calls[1], wantInit)
	}
	if !reflect.DeepEqual(f.calls[2], pushArgs) {
		t.Errorf("call 2 = %v, want push args (retry)", f.calls[2])
	}
	if audit.Len() == 0 {
		t.Errorf("expected audit breadcrumb on auto-init")
	}
}

func TestDispatchWithAutoInit_RaceWithAlreadyExists(t *testing.T) {
	// Push fails notfound.repo → init returns conflict.repo_exists
	// (another invocation init'd concurrently) → push retries and
	// succeeds.  Should NOT surface init's "already exists" as a
	// failure.
	f := &fakeDispatch{
		responses: []dispatchResult{
			{ExitCode: 6, Stdout: []byte(`{"error":{"code":"notfound.repo"}}`)},
			{ExitCode: 7, Stdout: []byte(`{"error":{"code":"conflict.repo_exists"}}`)},
			{ExitCode: 0, Stdout: []byte(`{"result":"ok"}`)},
		},
	}
	withFakeDispatch(t, f)

	var audit bytes.Buffer
	rc := dispatchWithAutoInit(&audit, "s3://example/repo",
		[]string{"wal", "push", "deployment", "/wal/000.."})
	if rc != 0 {
		t.Errorf("rc = %d, want 0 (race-with-init should be benign)", rc)
	}
	if len(f.calls) != 3 {
		t.Errorf("expected 3 dispatches; got %d", len(f.calls))
	}
}

func TestDispatchWithAutoInit_NonRepoFailureIsForwarded(t *testing.T) {
	// Push fails with auth error (NOT notfound.repo) → init must
	// NOT run; the auth error is forwarded as-is.
	f := &fakeDispatch{
		responses: []dispatchResult{
			{
				ExitCode: 3, // ExitAuth
				Stdout:   []byte(`{"error":{"code":"auth.bad_credentials"}}`),
			},
		},
	}
	withFakeDispatch(t, f)

	var audit bytes.Buffer
	rc := dispatchWithAutoInit(&audit, "s3://example/repo",
		[]string{"wal", "push", "deployment", "/wal/000.."})
	if rc != 3 {
		t.Errorf("rc = %d, want 3 (auth failure forwarded)", rc)
	}
	if len(f.calls) != 1 {
		t.Errorf("expected 1 dispatch; got %d (init should NOT run for non-repo errors)", len(f.calls))
	}
}

func TestDispatchWithAutoInit_InitFailureSurfaces(t *testing.T) {
	// Push fails notfound.repo → init fails (e.g. KMS unreachable)
	// → init's failure is the surfaced error, NOT the original
	// notfound.repo.  Operator wants the actionable cause.
	f := &fakeDispatch{
		responses: []dispatchResult{
			{ExitCode: 6, Stdout: []byte(`{"error":{"code":"notfound.repo"}}`)},
			{ExitCode: 8, Stdout: []byte(`{"error":{"code":"unreachable.kms"}}`)},
		},
	}
	withFakeDispatch(t, f)

	var audit bytes.Buffer
	rc := dispatchWithAutoInit(&audit, "s3://example/repo",
		[]string{"wal", "push", "deployment", "/wal/000.."})
	if rc != 8 {
		t.Errorf("rc = %d, want 8 (init's failure should surface)", rc)
	}
	if len(f.calls) != 2 {
		t.Errorf("expected 2 dispatches (push, failed init); got %d", len(f.calls))
	}
}

func TestDispatchWithAutoInit_RetryPushFailsBoundedToOnce(t *testing.T) {
	// Push fails notfound.repo → init succeeds → retry push ALSO
	// fails (e.g. with a real auth error this time).  We must
	// NOT loop indefinitely; the second failure surfaces.
	f := &fakeDispatch{
		responses: []dispatchResult{
			{ExitCode: 6, Stdout: []byte(`{"error":{"code":"notfound.repo"}}`)},
			{ExitCode: 0, Stdout: []byte(`{"result":{"id":"abc"}}`)},
			{ExitCode: 3, Stdout: []byte(`{"error":{"code":"auth.bad"}}`)},
		},
	}
	withFakeDispatch(t, f)

	var audit bytes.Buffer
	rc := dispatchWithAutoInit(&audit, "s3://example/repo",
		[]string{"wal", "push", "deployment", "/wal/000.."})
	if rc != 3 {
		t.Errorf("rc = %d, want 3 (retry's failure surfaces)", rc)
	}
	if len(f.calls) != 3 {
		t.Errorf("expected 3 dispatches (push, init, retry); got %d", len(f.calls))
	}
}

func TestLooksLikeMissingRepo(t *testing.T) {
	cases := []struct {
		name string
		res  dispatchResult
		want bool
	}{
		{"compact json on stdout", dispatchResult{
			ExitCode: 6,
			Stdout:   []byte(`{"error":{"code":"notfound.repo"}}`),
		}, true},
		{"pretty json on stdout", dispatchResult{
			ExitCode: 6,
			Stdout:   []byte(`{"error": {"code": "notfound.repo"}}`),
		}, true},
		{"compact json on stderr", dispatchResult{
			// Native CLI's structured error renderer writes
			// to stderr; production hit this branch and the
			// stdout-only matcher silently no-op'd.  Regression
			// test for the K8s S2 repo-init discovery.
			ExitCode: 6,
			Stderr:   []byte(`{"error":{"code":"notfound.repo"}}`),
		}, true},
		{"pretty json on stderr (production shape)", dispatchResult{
			ExitCode: 6,
			Stderr: []byte(`{
  "schema": "pg_hardstorage.v1",
  "command": "pg_hardstorage wal push",
  "error": {
    "code": "notfound.repo",
    "message": "..."
  }
}`),
		}, true},
		{"different code, exit 6", dispatchResult{
			ExitCode: 6,
			Stderr:   []byte(`{"error":{"code":"notfound.deployment"}}`),
		}, false},
		{"matches code but exit 0", dispatchResult{
			ExitCode: 0,
			Stderr:   []byte(`{"error":{"code":"notfound.repo"}}`),
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeMissingRepo(c.res); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
