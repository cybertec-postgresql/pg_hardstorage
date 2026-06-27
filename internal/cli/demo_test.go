package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// fakeRunner records every command the demo issues and lets a handler
// decide each one's output/error, so the whole orchestration can be
// driven without a Docker daemon or a second pg_hardstorage process.
type fakeRunner struct {
	calls   [][]string
	handler func(name string, args []string) (string, error)
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.handler != nil {
		return f.handler(name, args)
	}
	return "", nil
}

func (f *fakeRunner) called(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

// okHandler succeeds at every step and returns a plausible container id
// and published port.
func okHandler(name string, args []string) (string, error) {
	if name == "docker" && len(args) > 0 {
		switch args[0] {
		case "run":
			return "container0abc\n", nil
		case "port":
			return "0.0.0.0:49157\n", nil
		}
	}
	return "", nil
}

func errCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	e, ok := output.AsOutputError(err)
	if !ok {
		t.Fatalf("error is not a structured output error: %v", err)
	}
	return e.Code
}

func TestRunDemo_HappyPath_RunsFullFlowAndCleansUp(t *testing.T) {
	f := &fakeRunner{handler: okHandler}
	var w strings.Builder
	if err := runDemo(context.Background(), &w, f, "/usr/bin/pg_hardstorage"); err != nil {
		t.Fatalf("runDemo: %v", err)
	}
	// The demo drove the real flow through the actual verbs, with the
	// host port resolved from `docker port` woven into the DSN.
	wants := []string{
		"docker info",
		"docker run -d --rm",
		"POSTGRES_HOST_AUTH_METHOD=trust",
		"docker port container0abc 5432/tcp",
		"docker exec container0abc pg_isready",
		"/usr/bin/pg_hardstorage repo init file://",
		"/usr/bin/pg_hardstorage backup demo --pg-connection postgres://postgres@127.0.0.1:49157/postgres",
		"/usr/bin/pg_hardstorage restore demo latest",
		"/usr/bin/pg_hardstorage verify demo latest",
	}
	for _, want := range wants {
		if !f.called(want) {
			t.Errorf("demo did not run %q;\ncalls: %v", want, f.calls)
		}
	}
	// The container must always be torn down.
	if !f.called("docker rm -f container0abc") {
		t.Errorf("demo must tear down the container; calls: %v", f.calls)
	}
}

func TestRunDemo_DockerUnavailable_FriendlyError_NoContainer(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) (string, error) {
		if name == "docker" && len(args) > 0 && args[0] == "info" {
			return "Cannot connect to the Docker daemon at unix:///var/run/docker.sock", errors.New("exit status 1")
		}
		return "", nil
	}}
	err := runDemo(context.Background(), io.Discard, f, "pg_hardstorage")
	if code := errCode(t, err); code != "demo.docker_unavailable" {
		t.Errorf("error code = %q, want demo.docker_unavailable", code)
	}
	if f.called("docker run") {
		t.Error("demo must not start a container when Docker is unreachable")
	}
}

func TestRunDemo_StepFailure_StillCleansUp(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) (string, error) {
		switch {
		case name == "docker" && len(args) > 0 && args[0] == "run":
			return "cidX\n", nil
		case name == "docker" && len(args) > 0 && args[0] == "port":
			return "0.0.0.0:5400\n", nil
		case name != "docker" && len(args) > 0 && args[0] == "backup":
			return "connection refused", errors.New("exit status 1")
		}
		return "", nil
	}}
	err := runDemo(context.Background(), io.Discard, f, "self")
	if code := errCode(t, err); code != "demo.step_failed" {
		t.Errorf("error code = %q, want demo.step_failed", code)
	}
	// Cleanup must run even though a mid-flow step failed.
	if !f.called("docker rm -f cidX") {
		t.Errorf("container must be removed even when a step fails; calls: %v", f.calls)
	}
}

func TestParseDockerPort(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{"ipv4", "0.0.0.0:49153\n", "49153", false},
		{"ipv6 first then ipv4", "[::]:49153\n0.0.0.0:49153\n", "49153", false},
		{"empty", "", "", true},
		{"no colon", "garbage", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDockerPort(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("port = %q, want %q", got, tc.want)
			}
		})
	}
}
