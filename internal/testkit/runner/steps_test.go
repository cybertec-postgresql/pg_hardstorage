package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

// TestHighestArchivedWAL covers the repo-scan that the
// wait_wal_archived step polls: it must return the maximum committed
// segment EndLSN, ignore `.tmp.` staging files, and treat a
// not-yet-created WAL directory as "nothing archived" rather than an
// error.
func TestHighestArchivedWAL(t *testing.T) {
	tmp := t.TempDir()
	const deployment = "db1"
	dir := filepath.Join(tmp, "wal", deployment, "00000001")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(segNum uint64, startLSN, endLSN, suffix string) {
		t.Helper()
		m := &walsink.SegmentManifest{
			Schema:           walsink.Schema,
			Deployment:       deployment,
			SystemIdentifier: "7388",
			Timeline:         1,
			SegmentNumber:    segNum,
			SegmentName:      walsink.SegmentFileName(1, segNum, walsink.SegmentSize),
			StartLSN:         startLSN,
			EndLSN:           endLSN,
			SegmentSize:      walsink.SegmentSize,
		}
		body, err := m.MarshalToBytes()
		if err != nil {
			t.Fatal(err)
		}
		name := walsink.SegmentFileName(1, segNum, walsink.SegmentSize) + suffix
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Three committed segments, ascending — plus a staging temp whose
	// EndLSN is far higher and which MUST be ignored.
	write(0, "1/0", "1/10000000", ".json")
	write(1, "1/10000000", "2/20000000", ".json")
	write(2, "2/20000000", "3/30000000", ".json")
	write(9, "9/0", "F/F0000000", ".json.tmp.deadbeef")

	got, err := highestArchivedWAL("file://"+tmp, deployment)
	if err != nil {
		t.Fatalf("highestArchivedWAL: %v", err)
	}
	if got.String() != "3/30000000" {
		t.Errorf("highest = %s, want 3/30000000 (the .tmp. staging file must be ignored)", got)
	}

	empty, err := highestArchivedWAL("file://"+tmp, "no-such-deployment")
	if err != nil {
		t.Errorf("missing wal dir must not error: %v", err)
	}
	if empty != 0 {
		t.Errorf("missing wal dir: highest = %s, want 0", empty)
	}
}

// TestBuildAction_RawActionWins locks the precedence rule: when a
// scenario step sets `action:` directly, the convenience fields
// (kind / target / signal) are ignored.  Operators reach for the
// raw form when they need a primitive the convenience map doesn't
// know about, and any silent override of the raw action would be
// surprising.
func TestBuildAction_RawActionWins(t *testing.T) {
	got, err := buildAction(scenario.Step{
		Action:     "signal(target=patroni_random, sig=2)",
		InjectKind: "agent_kill",
		Signal:     9,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "signal(target=patroni_random, sig=2)" {
		t.Errorf("Action override lost; got %q", got)
	}
}

// TestBuildAction_KindMappings sweeps the convenience-mapping
// table.  Each entry must produce the exact action string the
// inject registry parses; a regression here turns a scenario
// into a silent no-op (or worse, a wrong target).
func TestBuildAction_KindMappings(t *testing.T) {
	cases := []struct {
		name string
		step scenario.Step
		want string
	}{
		{
			"agent_kill default sig=15",
			scenario.Step{InjectKind: "agent_kill"},
			"signal(target=agent_random, sig=15)",
		},
		{
			"agent_kill explicit signal",
			scenario.Step{InjectKind: "agent_kill", Signal: 9},
			"signal(target=agent_random, sig=9)",
		},
		{
			"agent_kill target override",
			scenario.Step{InjectKind: "agent_kill", Target: "agent_all", Signal: 15},
			"signal(target=agent_all, sig=15)",
		},
		{
			"pg_kill default sig=9",
			scenario.Step{InjectKind: "pg_kill"},
			"signal(target=pg_random, sig=9)",
		},
		{
			"pg_kill explicit signal",
			scenario.Step{InjectKind: "pg_kill", Signal: 15},
			"signal(target=pg_random, sig=15)",
		},
		{
			"patroni_failover default target",
			scenario.Step{InjectKind: "patroni_failover"},
			"patroni_switchover(target=patroni)",
		},
		{
			"patroni_failover target override",
			scenario.Step{InjectKind: "patroni_failover", Target: "patroni_random"},
			"patroni_switchover(target=patroni_random)",
		},
		{
			"patroni_switchover alias",
			scenario.Step{InjectKind: "patroni_switchover"},
			"patroni_switchover(target=patroni)",
		},
		{
			"disk_full default repo target",
			scenario.Step{InjectKind: "disk_full"},
			"disk_full(target=repo, fill=98%)",
		},
		{
			"pause_archive default agent target",
			scenario.Step{InjectKind: "pause_archive"},
			"pause_archive(target=agent)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildAction(c.step)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestBuildAction_RejectsEmpty makes sure we don't silently emit
// an empty action — an empty string would parse-fail downstream
// in inject.ParseAction with a less-actionable error.
func TestBuildAction_RejectsEmpty(t *testing.T) {
	_, err := buildAction(scenario.Step{})
	if err == nil {
		t.Fatal("expected error for empty step")
	}
	if !strings.Contains(err.Error(), "either `action:` or `kind:` is required") {
		t.Errorf("error should point at the action/kind requirement; got %v", err)
	}
}

// TestBuildAction_UnknownKind locks fail-fast on a typo.  Without
// this, a scenario with `kind: agent_kil` (one missing char)
// would silently dispatch to the unknown-kind error path at
// runtime — but only if a developer remembered to test it.  The
// test makes the contract explicit.
func TestBuildAction_UnknownKind(t *testing.T) {
	_, err := buildAction(scenario.Step{InjectKind: "agent_kil"})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
	if !strings.Contains(err.Error(), "unknown inject kind") {
		t.Errorf("error should name the unknown-kind rejection; got %v", err)
	}
	// Helpful: the error message points at the convenience-map
	// keys we DO know about so the operator can fix the typo
	// without grepping the source.
	for _, kw := range []string{"agent_kill", "pg_kill", "patroni_failover"} {
		if !strings.Contains(err.Error(), kw) {
			t.Errorf("error should list known kind %q to help operators fix typos; got %v", kw, err)
		}
	}
}

// TestResolveAgentBinary_CanonicalisesSymlinks pins the fix for
// the assert_restored_match sandbox failure on hosts where the
// pg_hardstorage tree is reached via a symlinked alias.
//
// Setup: tmp/alias is a symlink pointing at tmp/real/bin/agent.
// resolveAgentBinary called against the alias path must return
// the resolved /real/.../agent — not the alias — so the path
// the testkit threads through state.agentBin matches the
// canonical path the agent embeds in postgresql.auto.conf's
// restore_command via /proc/self/exe at backup time.
//
// Without canonicalisation:
//   - testkit's startRestoredSandbox bind-mounts /alias/agent
//   - agent's restore_command says exec /real/agent
//   - inside the sandbox /real/agent does not exist → sh: not
//     found → PG aborts on the first .history fetch → 180s
//     sandbox-ready timeout.  Reproduced in this repo when
//     /home/<user>/pg_hardstorage was a symlink to
//     /data/.../pg_hardstorage.
func TestResolveAgentBinary_CanonicalisesSymlinks(t *testing.T) {
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real", "bin")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realBin := filepath.Join(realDir, "pg_hardstorage")
	if err := os.WriteFile(realBin, []byte("not-a-real-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	aliasBin := filepath.Join(tmp, "alias-agent")
	if err := os.Symlink(realBin, aliasBin); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PG_HARDSTORAGE_BIN", aliasBin)
	got, err := resolveAgentBinary()
	if err != nil {
		t.Fatalf("resolveAgentBinary: %v", err)
	}

	wantReal, _ := filepath.EvalSymlinks(realBin)
	if got != wantReal {
		t.Errorf("resolveAgentBinary returned alias %q; want canonical %q (filepath.EvalSymlinks of the target)",
			got, wantReal)
	}
	if got == aliasBin {
		t.Errorf("resolveAgentBinary returned the alias unchanged — the sandbox bind-mount would not match the agent's embedded restore_command path")
	}
}
