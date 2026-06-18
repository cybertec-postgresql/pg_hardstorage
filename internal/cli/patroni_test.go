package cli_test

import (
	stdjson "encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// patroniFixture spins up an httptest server that responds to the
// Patroni REST endpoints. Mirrors the shape used by the patroni
// package's own tests.
func patroniFixture(t *testing.T, cluster, history any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = stdjson.NewEncoder(w).Encode(cluster)
	})
	mux.HandleFunc("/history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = stdjson.NewEncoder(w).Encode(history)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestPatroniStatus_RequiresURL: cobra-level missing-flag case.
func TestPatroniStatus_RequiresURL(t *testing.T) {
	_, stderr, exit := runCLI(t, "patroni", "status", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --url should exit Misuse; got %d\n%s", exit, stderr)
	}
}

// TestPatroniStatus_HappyPath: a fake server returning a typical
// 2-member cluster surfaces the leader + members in the body.
func TestPatroniStatus_HappyPath(t *testing.T) {
	srv := patroniFixture(t, map[string]any{
		"scope": "acme-prod",
		"members": []any{
			map[string]any{"name": "n1", "role": "leader", "state": "running",
				"host": "n1.example.com", "port": 5432, "timeline": 7},
			map[string]any{"name": "n2", "role": "replica", "state": "running",
				"host": "n2.example.com", "port": 5432, "timeline": 7, "lag": 0},
		},
	}, nil)

	stdout, _, exit := runCLI(t,
		"patroni", "status",
		"--url", srv.URL,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("status: exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"scope": "acme-prod"`,
		`"leader_name": "n1"`,
		`"leader_timeline": 7`,
		`"name": "n1"`,
		`"name": "n2"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("body missing %q:\n%s", want, stdout)
		}
	}
}

// TestPatroniStatus_NoLeader_BodyShowsAbsence: the body still
// renders cleanly but leader_name is omitted (omitempty).
func TestPatroniStatus_NoLeader_BodyShowsAbsence(t *testing.T) {
	srv := patroniFixture(t, map[string]any{
		"members": []any{
			map[string]any{"name": "n1", "role": "replica", "state": "running"},
			map[string]any{"name": "n2", "role": "replica", "state": "running"},
		},
	}, nil)
	stdout, _, exit := runCLI(t,
		"patroni", "status", "--url", srv.URL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	if strings.Contains(stdout, `"leader_name"`) {
		t.Errorf("leader_name should be omitted when no leader:\n%s", stdout)
	}
}

// TestPatroniStatus_TextRender: human-readable mode shows the
// leader + member table.
func TestPatroniStatus_TextRender(t *testing.T) {
	srv := patroniFixture(t, map[string]any{
		"scope": "x",
		"members": []any{
			map[string]any{"name": "primary", "role": "leader", "state": "running",
				"host": "primary.example", "port": 5432, "timeline": 3},
		},
	}, nil)
	stdout, _, exit := runCLI(t,
		"patroni", "status", "--url", srv.URL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`patroni cluster "x"`,
		"Leader: primary (TLI 3)",
		"NAME",
		"ROLE",
		"primary",
		"leader",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text missing %q:\n%s", want, stdout)
		}
	}
}

// TestPatroniStatus_Unreachable: a closed server URL maps to
// storage.unreachable / ExitUnreachable.
func TestPatroniStatus_Unreachable(t *testing.T) {
	_, stderr, exit := runCLI(t,
		"patroni", "status",
		"--url", "http://127.0.0.1:1",
		"-o", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("unreachable should not exit OK; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "storage.unreachable") {
		t.Errorf("expected storage.unreachable code; got\n%s", stderr)
	}
}

// TestPatroniHistory_HappyPath: the positional-array shape is
// parsed into named-field events.
func TestPatroniHistory_HappyPath(t *testing.T) {
	srv := patroniFixture(t, nil, []any{
		[]any{2.0, "0/15A2B388", "no recovery target specified",
			"2026-04-28T09:12:00+00:00", "node-2"},
	})
	stdout, _, exit := runCLI(t,
		"patroni", "history",
		"--url", srv.URL,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("history: exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"timeline": 2`,
		`"switch_lsn": "0/15A2B388"`,
		`"new_leader": "node-2"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("body missing %q:\n%s", want, stdout)
		}
	}
}

// TestPatroniHistory_EmptyTextRender: a cluster with no
// promotions surfaces a friendly empty-state line.
func TestPatroniHistory_EmptyTextRender(t *testing.T) {
	srv := patroniFixture(t, nil, []any{})
	stdout, _, exit := runCLI(t,
		"patroni", "history", "--url", srv.URL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, "no recorded promotions") {
		t.Errorf("expected empty-state message:\n%s", stdout)
	}
}

// TestPatroniFollow_RequiresURL: cobra-level missing-flag case.
func TestPatroniFollow_RequiresURL(t *testing.T) {
	_, stderr, exit := runCLI(t, "patroni", "follow", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --url should exit Misuse; got %d\n%s", exit, stderr)
	}
}

// TestPatroniFollow_DurationCappedRunExitsCleanly: with a small
// --duration the command runs for the window then returns
// ExitOK with the exited_cleanly summary body. Smoke-tests the
// whole pipeline (CLI flag parsing → follower → dispatcher
// shutdown).
func TestPatroniFollow_DurationCappedRunExitsCleanly(t *testing.T) {
	srv := patroniFixture(t, map[string]any{
		"scope": "test",
		"members": []any{
			map[string]any{"name": "n1", "role": "leader", "state": "running",
				"host": "n1.example.com", "port": 5432, "timeline": 1},
		},
	}, nil)

	stdout, stderr, exit := runCLI(t,
		"patroni", "follow",
		"--url", srv.URL,
		"--interval", "30ms",
		"--duration", "150ms",
		"-o", "ndjson")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d, want ExitOK\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	// We should see at least one leader_change event for the
	// initial observation. NDJSON streams one Result/Event per
	// line; we check substring presence rather than parsing each
	// line, since other event flow rules apply.
	if !strings.Contains(stdout, "leader_change") {
		t.Errorf("expected leader_change event in stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "n1") {
		t.Errorf("expected leader name in stdout:\n%s", stdout)
	}
}

// TestPatroniFollow_BadURLSurfaceUsage: an invalid URL flag is a
// usage error; surfaced via buildPatroniClient.
func TestPatroniFollow_BadURLSurfaceUsage(t *testing.T) {
	_, stderr, exit := runCLI(t,
		"patroni", "follow",
		"--url", "ftp://wrong-scheme/",
		"--duration", "100ms",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("bad URL should exit Misuse; got %d\n%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag; stderr=%s", stderr)
	}
}
