package inject_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
)

func TestRegistry_AllPrefixesRegistered(t *testing.T) {
	want := []string{
		// Original eleven (in registration order).
		"disk_full", "signal", "cgroup_squeeze", "toxiproxy", "sql",
		"patroni_switchover", "libfaketime", "network_block",
		"flip_random_byte", "pause_archive", "docker_pause",
		// Second wave, added after the 12h soak surfaced gaps
		// in the existing fault catalogue (see faults_extra.go).
		"checkpoint_storm", "drop_relation_mid_backup",
		"tablespace_unmount", "inode_exhaustion", "fd_exhaustion",
		"manifest_targeted_corruption", "concurrent_repo_writer",
		"truncated_wal_segment", "missing_wal_segment", "torn_page",
	}
	got := inject.DefaultRegistry.Names()
	gotMap := map[string]bool{}
	for _, n := range got {
		gotMap[n] = true
	}
	for _, w := range want {
		if !gotMap[w] {
			t.Errorf("registry missing %q (got: %v)", w, got)
		}
	}
}

func TestRegistry_Lookup_Unknown(t *testing.T) {
	_, err := inject.DefaultRegistry.Lookup("imaginary_fault")
	if err == nil || !strings.Contains(err.Error(), "unknown fault prefix") {
		t.Errorf("expected unknown-prefix error; got %v", err)
	}
}

// fixtureSet builds a static target set with one of each role
// needed by the fault tests below.
func fixtureSet(t *testing.T) (
	inject.TargetSet, *inject.FakeTarget, *inject.FakeTarget, *inject.FakeTarget,
) {
	t.Helper()
	agent := &inject.FakeTarget{NameStr: "agent-0", RoleStr: "agent"}
	pg := &inject.FakeTarget{NameStr: "pg-0", RoleStr: "pg",
		ExecResponses: map[string][]byte{
			// disk_full's default mount-for moved from
			// /var/lib/pg_hardstorage to /var/lib/pg_hardstorage/repo
			// so the fill lands in the bind-mounted host repo
			// instead of the cell's overlay (which used to
			// cascade-fail every other cell on the host).
			"df --output=avail -B1 /var/lib/pg_hardstorage/repo": []byte("Avail\n1048576000\n"),
		},
	}
	repo := &inject.FakeTarget{NameStr: "repo-0", RoleStr: "repo",
		ExecResponses: map[string][]byte{
			"sh -c find chunks/ -type f 2>/dev/null | shuf -n 1": []byte("chunks/aa/bbcdef.chk\n"),
		},
		Files: map[string][]byte{
			"chunks/aa/bbcdef.chk": []byte("hello-fake-chunk-payload"),
		},
	}
	ts := inject.NewStaticTargetSet([]inject.Target{agent, pg, repo}, 42)
	return ts, agent, pg, repo
}

func TestSignal_DispatchesKill9(t *testing.T) {
	ts, agent, _, _ := fixtureSet(t)
	rec, err := inject.DefaultRegistry.Apply(
		context.Background(),
		"signal(target=agent, sig=9)",
		ts)
	if err != nil {
		t.Fatal(err)
	}
	if got := agent.Signals(); len(got) != 1 || got[0] != 9 {
		t.Errorf("expected one SIGKILL on agent; got %v", got)
	}
	// Recovery for signals is no-op.
	if err := rec(context.Background()); err != nil {
		t.Errorf("NoRecovery shouldn't error: %v", err)
	}
}

func TestSignal_RandomTargetPicksOneAgent(t *testing.T) {
	a := &inject.FakeTarget{NameStr: "a-0", RoleStr: "agent"}
	b := &inject.FakeTarget{NameStr: "a-1", RoleStr: "agent"}
	ts := inject.NewStaticTargetSet([]inject.Target{a, b}, 42)

	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"signal(target=agent_random, sig=15)", ts)
	if err != nil {
		t.Fatal(err)
	}
	total := len(a.Signals()) + len(b.Signals())
	if total != 1 {
		t.Errorf("expected exactly one of {a,b} to receive a signal; got %d", total)
	}
}

// Issue #46: when a cell's container has already crashed / been
// OOM-killed, `docker kill` reports "is not running".  The signal
// fault must NOT mis-report that pre-existing crash as a fault
// failure — its down-then-up intent is already half-met, so it
// should fall through to Start and recover the container.
func TestSignal_AlreadyDownTargetIsRecoveredNotFailed(t *testing.T) {
	agent := &inject.FakeTarget{
		NameStr: "agent-0", RoleStr: "agent",
		SignalErr: fmt.Errorf("agent-0: %w", inject.ErrTargetNotRunning),
	}
	ts := inject.NewStaticTargetSet([]inject.Target{agent}, 42)

	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"signal(target=agent, sig=15)", ts)
	if err != nil {
		t.Fatalf("signal on an already-down target must not fail: %v", err)
	}
	if got := agent.StartCalls(); got != 1 {
		t.Errorf("expected the fault to Start (recover) the down container once; got %d", got)
	}
}

// A genuine signal-delivery failure (not "container not running")
// must still be reported as a fault failure.
func TestSignal_GenuineSignalFailureStillErrors(t *testing.T) {
	agent := &inject.FakeTarget{
		NameStr: "agent-0", RoleStr: "agent",
		SignalErr: errors.New("docker daemon unreachable"),
	}
	ts := inject.NewStaticTargetSet([]inject.Target{agent}, 42)

	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"signal(target=agent, sig=15)", ts)
	if err == nil {
		t.Fatal("a genuine Signal failure must still surface as a fault error")
	}
}

func TestDiskFull_FillsAndRecovers(t *testing.T) {
	ts, _, pg, _ := fixtureSet(t)
	rec, err := inject.DefaultRegistry.Apply(context.Background(),
		"disk_full(target=pg, fill=98%)", ts)
	if err != nil {
		t.Fatal(err)
	}
	calls := pg.ExecCalls()
	// Expect: df, then dd.
	if len(calls) < 2 {
		t.Fatalf("expected df + dd; got %v", calls)
	}
	if calls[0][0] != "df" {
		t.Errorf("first call should be df; got %v", calls[0])
	}
	if calls[1][0] != "sh" || !strings.Contains(strings.Join(calls[1], " "), "dd if=/dev/zero") {
		t.Errorf("second call should be dd via sh -c; got %v", calls[1])
	}
	// Recovery removes the spacer.
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := pg.ExecCalls()[len(pg.ExecCalls())-1]
	if last[0] != "rm" || last[1] != "-f" {
		t.Errorf("recovery should rm -f; got %v", last)
	}
	// Default cap kicks in: avail=1 GiB × 98% = ~1004 MiB, but
	// the default max_bytes cap (256 MiB) clamps the dd to 256
	// blocks.  Without the cap, a parallel-soak run filled the
	// host's docker storage from a single fault and cascade-failed
	// every other cell with ENOSPC.
	ddCmd := strings.Join(calls[1], " ")
	if !strings.Contains(ddCmd, "count=256") {
		t.Errorf("default max_bytes cap (256 MiB) should clamp dd to count=256; got %q", ddCmd)
	}
}

func TestDiskFull_MaxBytesCapsTheFill(t *testing.T) {
	// Explicit max_bytes overrides the default cap.  64 MiB
	// here, so the dd should land at count=64 regardless of
	// the (much larger) avail-times-fill product.
	ts, _, pg, _ := fixtureSet(t)
	if _, err := inject.DefaultRegistry.Apply(context.Background(),
		"disk_full(target=pg, fill=98%, max_bytes=67108864)", ts); err != nil {
		t.Fatal(err)
	}
	dd := strings.Join(pg.ExecCalls()[1], " ")
	if !strings.Contains(dd, "count=64") {
		t.Errorf("max_bytes=64MiB should clamp dd to count=64; got %q", dd)
	}
}

func TestDiskFull_BadMaxBytes(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	if _, err := inject.DefaultRegistry.Apply(context.Background(),
		"disk_full(target=pg, max_bytes=0)", ts); err == nil {
		t.Errorf("max_bytes=0 should be rejected")
	}
	if _, err := inject.DefaultRegistry.Apply(context.Background(),
		"disk_full(target=pg, max_bytes=not-a-number)", ts); err == nil {
		t.Errorf("non-numeric max_bytes should be rejected")
	}
}

func TestCgroupSqueeze_AppliesAndRestores(t *testing.T) {
	ts, _, pg, _ := fixtureSet(t)
	rec, err := inject.DefaultRegistry.Apply(context.Background(),
		"cgroup_squeeze(target=pg, max_bytes=33554432)", ts)
	if err != nil {
		t.Fatal(err)
	}
	// SetMemoryLimit replaces the old in-container `echo > /sys/
	// fs/cgroup/memory.max` writes — Docker mounts cgroupfs RO
	// inside the container, so the in-container approach failed
	// on every Fedora soak run.  The runtime calls
	// `docker update --memory=N` from outside instead.
	limits := pg.MemoryLimits()
	if len(limits) != 1 || limits[0] != 33554432 {
		t.Errorf("apply should set the new limit (33554432); got %v", limits)
	}
	// Recovery removes the limit (-1 sentinel).
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
	limits = pg.MemoryLimits()
	if len(limits) != 2 || limits[1] != -1 {
		t.Errorf("recovery should remove the limit (-1); got %v", limits)
	}
}

func TestCgroupSqueeze_RecoveryRestartsPG(t *testing.T) {
	ts, _, pg, _ := fixtureSet(t)
	rec, err := inject.DefaultRegistry.Apply(context.Background(),
		"cgroup_squeeze(target=pg, max_bytes=33554432)", ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A 32 MiB squeeze OOM-kills the postmaster; lifting the
	// limit alone leaves the cell dead for every later backup.
	// Recovery must issue a pg_ctl restart on the pg target.
	calls := pg.ExecCalls()
	if len(calls) != 1 {
		t.Fatalf("recovery should issue exactly one restart exec on the pg target; got %v", calls)
	}
	joined := strings.Join(calls[0], " ")
	if !strings.Contains(joined, "pg_ctl") || !strings.Contains(joined, "start") {
		t.Errorf("recovery exec should run pg_ctl start; got %v", calls[0])
	}
}

func TestCgroupSqueeze_RecoverySkipsNonPGTarget(t *testing.T) {
	ts, _, _, repo := fixtureSet(t)
	rec, err := inject.DefaultRegistry.Apply(context.Background(),
		"cgroup_squeeze(target=repo, max_bytes=33554432)", ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A repo target has no postmaster — recovery lifts the limit
	// but must not attempt a pg_ctl restart.
	if calls := repo.ExecCalls(); len(calls) != 0 {
		t.Errorf("recovery on a non-pg target should not exec anything; got %v", calls)
	}
}

func TestSQL_DispatchesPsql(t *testing.T) {
	ts, _, pg, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		`sql("SELECT pg_drop_replication_slot('foo')")`, ts)
	if err != nil {
		t.Fatal(err)
	}
	calls := pg.ExecCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one exec; got %v", calls)
	}
	// sql() dispatches via `su -s /bin/sh -c "psql -c '<stmt>'" <user>`
	// — `su`, not `sudo`, because the testbed images don't ship sudo.
	joined := strings.Join(calls[0], " ")
	if calls[0][0] != "su" || !strings.Contains(joined, "psql -c") {
		t.Errorf("expected su ... -c \"psql -c ...\"; got %v", calls[0])
	}
	if !strings.Contains(joined, "pg_drop_replication_slot") {
		t.Errorf("statement not propagated; got %v", calls[0])
	}
}

func TestPatroniSwitchover_DispatchesCurl(t *testing.T) {
	// Patroni's /cluster JSON shape — the primitive parses
	// this to pick (current leader, healthy replica) and POST
	// /failover with the body Patroni 4.x requires.  The
	// previous body-less POST to /switchover was rejected by
	// modern Patroni with HTTP 411 (Length Required).
	clusterBody := []byte(`{
		"members": [
			{"name": "p-0", "role": "leader", "state": "running"},
			{"name": "p-1", "role": "replica", "state": "streaming"},
			{"name": "p-2", "role": "replica", "state": "streaming"}
		]
	}`)
	patroni := &inject.FakeTarget{
		NameStr: "p-0",
		RoleStr: "patroni",
		ExecResponses: map[string][]byte{
			"curl -fsS http://127.0.0.1:8008/cluster": clusterBody,
		},
	}
	ts := inject.NewStaticTargetSet([]inject.Target{patroni}, 42)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"patroni_switchover()", ts)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	calls := patroni.ExecCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 execs (GET /cluster, POST /failover); got %d: %v", len(calls), calls)
	}
	// First call: GET /cluster.
	if calls[0][0] != "curl" || !strings.Contains(strings.Join(calls[0], " "), "/cluster") {
		t.Errorf("first exec should GET /cluster; got %v", calls[0])
	}
	// Second call: POST /failover with a body that names the
	// current leader and a candidate replica.
	second := strings.Join(calls[1], " ")
	if !strings.Contains(second, "POST") && !strings.Contains(second, "-XPOST") {
		t.Errorf("second exec should be a POST; got %v", calls[1])
	}
	if !strings.Contains(second, "/failover") {
		t.Errorf("second exec should hit /failover; got %v", calls[1])
	}
	if !strings.Contains(second, `"leader":"p-0"`) {
		t.Errorf("body should name the current leader p-0; got %v", calls[1])
	}
	if !strings.Contains(second, `"candidate":"p-1"`) && !strings.Contains(second, `"candidate":"p-2"`) {
		t.Errorf("body should name a healthy replica candidate; got %v", calls[1])
	}
}

// TestPatroniSwitchover_NoCandidateRejects locks the fail-fast
// behaviour when /cluster reports a singleton (no replica to
// promote).  Without this guard, Patroni's /failover would
// silently 400 and the soak run would record a misleading
// "patroni_switchover succeeded" event.
func TestPatroniSwitchover_NoCandidateRejects(t *testing.T) {
	clusterBody := []byte(`{
		"members": [
			{"name": "p-0", "role": "leader", "state": "running"}
		]
	}`)
	patroni := &inject.FakeTarget{
		NameStr: "p-0",
		RoleStr: "patroni",
		ExecResponses: map[string][]byte{
			"curl -fsS http://127.0.0.1:8008/cluster": clusterBody,
		},
	}
	ts := inject.NewStaticTargetSet([]inject.Target{patroni}, 42)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"patroni_switchover()", ts)
	if err == nil {
		t.Fatal("expected error when cluster has no replica candidate")
	}
	if !strings.Contains(err.Error(), "no healthy replica candidate") {
		t.Errorf("error should explain the no-candidate refusal; got %v", err)
	}
}

func TestLibfaketime_WritesEnvelope(t *testing.T) {
	ts, agent, _, _ := fixtureSet(t)
	rec, err := inject.DefaultRegistry.Apply(context.Background(),
		"libfaketime(target=agent, skew=+10m)", ts)
	if err != nil {
		t.Fatal(err)
	}
	if got := agent.Files["/etc/faketimerc"]; string(got) != "+10m\n" {
		t.Errorf("faketimerc content: got %q want %q", got, "+10m\n")
	}
	// Recovery removes the file.
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := agent.ExecCalls()[len(agent.ExecCalls())-1]
	if last[0] != "rm" || last[1] != "-f" {
		t.Errorf("recovery should rm -f; got %v", last)
	}
}

func TestNetworkBlock_AddsAndRemovesIptables(t *testing.T) {
	ts, agent, _, _ := fixtureSet(t)
	rec, err := inject.DefaultRegistry.Apply(context.Background(),
		"network_block(target=10.0.0.42, source=agent)", ts)
	if err != nil {
		t.Fatal(err)
	}
	calls := agent.ExecCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one apply call; got %v", calls)
	}
	if calls[0][1] != "-A" {
		t.Errorf("apply should be -A; got %v", calls[0])
	}
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := agent.ExecCalls()[len(agent.ExecCalls())-1]
	if last[1] != "-D" {
		t.Errorf("recovery should be -D; got %v", last)
	}
}

func TestFlipRandomByte_ReadsFlipsWrites(t *testing.T) {
	ts, _, _, repo := fixtureSet(t)
	original := append([]byte(nil), repo.Files["chunks/aa/bbcdef.chk"]...)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"flip_random_byte(prefix=chunks/, target=repo)", ts)
	if err != nil {
		t.Fatal(err)
	}
	got := repo.Files["chunks/aa/bbcdef.chk"]
	if len(got) != len(original) {
		t.Fatalf("length changed: %d vs %d", len(got), len(original))
	}
	diff := 0
	for i := range got {
		if got[i] != original[i] {
			diff++
		}
	}
	if diff != 1 {
		t.Errorf("expected exactly one byte flipped; got %d", diff)
	}
	if w := repo.Written(); len(w) != 1 || w[0] != "chunks/aa/bbcdef.chk" {
		t.Errorf("expected one write to the flipped file; got %v", w)
	}
}

func TestPauseArchive_TouchesAndRemoves(t *testing.T) {
	ts, agent, _, _ := fixtureSet(t)
	rec, err := inject.DefaultRegistry.Apply(context.Background(),
		"pause_archive(target=agent)", ts)
	if err != nil {
		t.Fatal(err)
	}
	calls := agent.ExecCalls()
	if calls[0][0] != "touch" {
		t.Errorf("expected touch; got %v", calls[0])
	}
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := agent.ExecCalls()[len(agent.ExecCalls())-1]
	if last[0] != "rm" || last[1] != "-f" {
		t.Errorf("recovery should rm -f sentinel; got %v", last)
	}
}

func TestToxiproxy_RequiresProxyTarget(t *testing.T) {
	// Fixture set has no `toxiproxy` role.
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"toxiproxy(proxy=repo, type=latency, latency=2000)", ts)
	if err == nil || !strings.Contains(err.Error(), "toxiproxy") {
		t.Errorf("expected toxiproxy-target error; got %v", err)
	}
}

func TestToxiproxy_DispatchesCLI(t *testing.T) {
	tox := &inject.FakeTarget{NameStr: "tox-0", RoleStr: "toxiproxy"}
	ts := inject.NewStaticTargetSet([]inject.Target{tox}, 42)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"toxiproxy(proxy=repo, type=latency, latency=2000)", ts)
	if err != nil {
		t.Fatal(err)
	}
	calls := tox.ExecCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one exec; got %v", calls)
	}
	joined := strings.Join(calls[0], " ")
	if !strings.Contains(joined, "toxiproxy-cli toxic add") {
		t.Errorf("expected toxiproxy-cli toxic add; got %v", calls[0])
	}
	if !strings.Contains(joined, "-t latency") {
		t.Errorf("expected -t latency; got %v", calls[0])
	}
	if !strings.Contains(joined, "-a latency=2000") {
		t.Errorf("expected -a latency=2000; got %v", calls[0])
	}
}

func TestApply_BadAction(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(), "no_open_paren", ts)
	if err == nil || !strings.Contains(err.Error(), "missing '('") {
		t.Errorf("expected parse error; got %v", err)
	}
}

func TestApply_UnknownFault(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(), "imaginary_fault()", ts)
	if err == nil || !strings.Contains(err.Error(), "unknown fault prefix") {
		t.Errorf("expected unknown-fault error; got %v", err)
	}
}

func TestTargetSet_PickByExactName(t *testing.T) {
	a := &inject.FakeTarget{NameStr: "a-0", RoleStr: "agent"}
	b := &inject.FakeTarget{NameStr: "a-1", RoleStr: "agent"}
	ts := inject.NewStaticTargetSet([]inject.Target{a, b}, 42)
	got, err := ts.Pick("a-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name() != "a-1" {
		t.Errorf("pick by exact name failed; got %v", got)
	}
}

func TestTargetSet_PickUnknown(t *testing.T) {
	a := &inject.FakeTarget{NameStr: "a-0", RoleStr: "agent"}
	ts := inject.NewStaticTargetSet([]inject.Target{a}, 42)
	if _, err := ts.Pick("nope"); err == nil {
		t.Errorf("expected error for unknown spec")
	}
}
