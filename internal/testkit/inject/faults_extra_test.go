package inject_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
)

// --- checkpoint_storm -------------------------------------------------

func TestCheckpointStorm_DispatchesSuViaPsqlLoop(t *testing.T) {
	ts, _, pg, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"checkpoint_storm(target=pg, count=5)", ts)
	if err != nil {
		t.Fatal(err)
	}
	calls := pg.ExecCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one docker exec for the whole storm; got %d: %v", len(calls), calls)
	}
	joined := strings.Join(calls[0], " ")
	// Must run via `su -s /bin/sh` (the testbed images don't ship sudo).
	if calls[0][0] != "su" {
		t.Errorf("expected su to be the dispatching command; got %v", calls[0])
	}
	if !strings.Contains(joined, "CHECKPOINT") {
		t.Errorf("script should invoke CHECKPOINT; got %v", calls[0])
	}
	if !strings.Contains(joined, `"$i" -lt 5`) {
		t.Errorf("script should bound the loop by count=5; got %v", calls[0])
	}
	// OS user defaults to "postgres" — the testbed images' fixed
	// OS user.  An earlier draft of this fault defaulted to
	// "testkit" (a PG role, not an OS user); the soak found
	// that `su testkit` fails with "user does not exist"
	// within 3 minutes of the first 4-slot run.
	if !strings.HasSuffix(joined, "postgres") {
		t.Errorf("last arg should be the OS user (postgres); got %v", calls[0])
	}
}

func TestCheckpointStorm_BadCountRejected(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"checkpoint_storm(target=pg, count=0)", ts)
	if err == nil || !strings.Contains(err.Error(), "positive int") {
		t.Errorf("expected positive-int error; got %v", err)
	}
}

func TestCheckpointStorm_SleepMsInjected(t *testing.T) {
	ts, _, pg, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"checkpoint_storm(target=pg, count=3, sleep_ms=250)", ts)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(pg.ExecCalls()[0], " ")
	// 250 ms → "sleep 0.250" (GNU coreutils fractional form).
	if !strings.Contains(joined, "sleep 0.250") {
		t.Errorf("script should include `sleep 0.250` between checkpoints; got %v", joined)
	}
}

// --- drop_relation_mid_backup ----------------------------------------

func TestDropRelation_RunsCreateInsertCheckpointDropInOneExec(t *testing.T) {
	ts, _, pg, _ := fixtureSet(t)
	rec, err := inject.DefaultRegistry.Apply(context.Background(),
		"drop_relation_mid_backup(target=pg, rows=100)", ts)
	if err != nil {
		t.Fatal(err)
	}
	calls := pg.ExecCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one docker exec containing the whole pipeline; got %d: %v", len(calls), calls)
	}
	joined := strings.Join(calls[0], " ")
	for _, want := range []string{"CREATE TABLE", "INSERT INTO", "CHECKPOINT", "DROP TABLE", "testkit_drop_"} {
		if !strings.Contains(joined, want) {
			t.Errorf("script should include %q; got %v", want, joined)
		}
	}
	// Recovery: idempotent DROP IF EXISTS.
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := pg.ExecCalls()[len(pg.ExecCalls())-1]
	if !strings.Contains(strings.Join(last, " "), "DROP TABLE IF EXISTS testkit_drop_") {
		t.Errorf("recovery should DROP IF EXISTS; got %v", last)
	}
}

func TestDropRelation_BadRowsRejected(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"drop_relation_mid_backup(target=pg, rows=-1)", ts)
	if err == nil || !strings.Contains(err.Error(), "positive int") {
		t.Errorf("expected positive-int error; got %v", err)
	}
}

// --- tablespace_unmount ----------------------------------------------

func TestTablespaceUnmount_RequiresPath(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"tablespace_unmount(target=pg)", ts)
	if err == nil || !strings.Contains(err.Error(), `"path"`) {
		t.Errorf("expected missing-path error; got %v", err)
	}
}

func TestTablespaceUnmount_RejectsRelativePath(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"tablespace_unmount(target=pg, path=relative/dir)", ts)
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Errorf("expected absolute-path error; got %v", err)
	}
}

func TestTablespaceUnmount_DispatchesMv(t *testing.T) {
	ts, _, pg, _ := fixtureSet(t)
	rec, err := inject.DefaultRegistry.Apply(context.Background(),
		"tablespace_unmount(target=pg, path=/srv/tblspc/extra)", ts)
	if err != nil {
		t.Fatal(err)
	}
	apply := pg.ExecCalls()
	if len(apply) != 1 {
		t.Fatalf("expected one apply exec; got %d", len(apply))
	}
	joined := strings.Join(apply[0], " ")
	if !strings.Contains(joined, "mv /srv/tblspc/extra /srv/tblspc/extra.testkit-unmounted") {
		t.Errorf("apply script should mv the path aside; got %v", joined)
	}
	// PGDATA guard: PG_VERSION-bearing directories are refused.
	if !strings.Contains(joined, "PG_VERSION") {
		t.Errorf("apply script should refuse PGDATA-shaped directories; got %v", joined)
	}
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
	recoverCall := pg.ExecCalls()[len(pg.ExecCalls())-1]
	recJoined := strings.Join(recoverCall, " ")
	if !strings.Contains(recJoined, "mv /srv/tblspc/extra.testkit-unmounted /srv/tblspc/extra") {
		t.Errorf("recovery should mv back; got %v", recJoined)
	}
}

// --- inode_exhaustion ------------------------------------------------

func TestInodeExhaustion_DispatchesGuardedShellLoop(t *testing.T) {
	ts, _, _, repo := fixtureSet(t)
	rec, err := inject.DefaultRegistry.Apply(context.Background(),
		"inode_exhaustion(target=repo, path=/var/lib/pg_hardstorage/repo/.fill, max_iavail=1000000, cap=200000)", ts)
	if err != nil {
		t.Fatal(err)
	}
	apply := repo.ExecCalls()
	if len(apply) < 1 {
		t.Fatalf("expected at least one apply exec; got %v", apply)
	}
	joined := strings.Join(apply[0], " ")
	// Guard message + iavail check + creation loop.
	for _, want := range []string{
		"mkdir -p /var/lib/pg_hardstorage/repo/.fill",
		"df --output=iavail",
		"max_iavail=1000000",
		"target_count=200000",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("script should contain %q; got %v", want, joined)
		}
	}
	// Recovery: rm -rf the spacer dir.
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := repo.ExecCalls()[len(repo.ExecCalls())-1]
	if !(last[0] == "rm" && last[1] == "-rf" && last[2] == "/var/lib/pg_hardstorage/repo/.fill") {
		t.Errorf("recovery should rm -rf the spacer dir; got %v", last)
	}
}

func TestInodeExhaustion_BadMaxIAvail(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"inode_exhaustion(target=repo, max_iavail=0)", ts)
	if err == nil || !strings.Contains(err.Error(), "positive int") {
		t.Errorf("expected positive-int error; got %v", err)
	}
}

// --- fd_exhaustion ---------------------------------------------------

func TestFDExhaustion_EPERMTypedAsErrCapSysResource(t *testing.T) {
	// Default docker container caps drop SYS_RESOURCE; the
	// soak found this in 3 minutes via the prlimit error
	// "Operation not permitted".  The fault must surface a
	// typed sentinel (ErrCapSysResource) so the orchestrator
	// can classify "the cell needs SYS_RESOURCE wired up"
	// distinctly from generic apply failures.
	//
	// FakeTarget returns nil error from Exec by default, so to
	// simulate the EPERM path we need an ExecResponse that
	// drives the script to print the diagnostic string.  But
	// FakeTarget.Exec returns nil err on a configured response,
	// so we test the detection path via a hand-rolled target
	// that returns a non-nil error and an "Operation not
	// permitted" output.
	pg := &epermTarget{NameStr: "pg-0", RoleStr: "pg"}
	ts := inject.NewStaticTargetSet([]inject.Target{pg}, 42)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"fd_exhaustion(target=pg, limit=64)", ts)
	if err == nil {
		t.Fatal("expected error when prlimit fails with EPERM")
	}
	if !errors.Is(err, inject.ErrCapSysResource) {
		t.Errorf("expected error to wrap ErrCapSysResource; got %v", err)
	}
}

// epermTarget is a one-shot test fixture that emits the prlimit
// EPERM error shape so the typed-error path can be exercised.
// FakeTarget can't do this directly because its ExecResponses
// branch always returns nil err.
type epermTarget struct {
	NameStr string
	RoleStr string
}

func (e *epermTarget) Name() string { return e.NameStr }
func (e *epermTarget) Role() string { return e.RoleStr }
func (e *epermTarget) Exec(_ context.Context, _ ...string) ([]byte, error) {
	return []byte("prlimit: failed to set the NOFILE resource limit: Operation not permitted\n"),
		errors.New("exit status 1")
}
func (e *epermTarget) Signal(_ context.Context, _ int) error               { return nil }
func (e *epermTarget) Start(_ context.Context) error                       { return nil }
func (e *epermTarget) CopyOut(_ context.Context, _ string) ([]byte, error) { return nil, nil }
func (e *epermTarget) CopyIn(_ context.Context, _ string, _ []byte) error  { return nil }
func (e *epermTarget) SetMemoryLimit(_ context.Context, _ int64) error     { return nil }

func TestFDExhaustion_DispatchesPrlimitOnDiscoveredPID(t *testing.T) {
	pg := &inject.FakeTarget{
		NameStr: "pg-0",
		RoleStr: "pg",
		// The Apply script picks the oldest matching PID and
		// echoes it back to stdout.  FakeTarget can't actually
		// run a shell, but the fault's contract is "echo the
		// PID on stdout"; ExecResponses simulates that.
		ExecResponses: nil, // default ExecResponses returns nil; we'll match by command prefix
	}
	ts := inject.NewStaticTargetSet([]inject.Target{pg}, 42)
	// Pre-populate ExecResponses with the same shell command the
	// Apply will issue; FakeTarget keys on the joined argv.  We
	// can't predict the exact script text (it embeds runtime
	// args), so we let the default (nil) response satisfy the
	// PID echo: the apply script's final `echo` is an empty
	// stdout from FakeTarget's perspective, which is then
	// rejected by the fault's "no PID echoed" branch.  Use a
	// targeted ExecResponse instead by re-creating with the
	// exact script as the lookup key after Apply runs once.
	//
	// Simpler path: assert that Apply errors with "no PID
	// echoed" when ExecResponses doesn't simulate a PID — this
	// IS the fault's documented refusal-to-pretend behaviour.
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"fd_exhaustion(target=pg, limit=64)", ts)
	if err == nil || !strings.Contains(err.Error(), "no PID echoed") {
		t.Fatalf("expected no-PID-echoed error from empty exec response; got %v", err)
	}
	// And the issued exec script must mention prlimit + the
	// requested limit + the comm being searched (postgres).
	calls := pg.ExecCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one exec; got %d: %v", len(calls), calls)
	}
	joined := strings.Join(calls[0], " ")
	for _, want := range []string{"target_comm=postgres", "prlimit --nofile=64:64"} {
		if !strings.Contains(joined, want) {
			t.Errorf("script should contain %q; got %v", want, joined)
		}
	}
}

func TestFDExhaustion_BadRole(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"fd_exhaustion(target=pg, role=postmaster)", ts)
	if err == nil || !strings.Contains(err.Error(), "role must be one of") {
		t.Errorf("expected role-validation error; got %v", err)
	}
}

// --- manifest_targeted_corruption ------------------------------------

func TestManifestCorruption_DispatchesInContainerFlip(t *testing.T) {
	// The fault now flips bytes INSIDE the container via `dd
	// seek=N conv=notrunc`, not by tar-streaming the file back
	// through docker cp.  The earlier tar-stream shape failed
	// ~25 % of the time on small files (byte-flip landing in
	// the tar header → broken-pipe failures).  This test
	// pins the new in-container shape: locate + flip script,
	// both via shell exec.
	repo := &inject.FakeTarget{
		NameStr: "repo-0",
		RoleStr: "repo",
		ExecResponses: map[string][]byte{
			"sh -c find /var/lib/pg_hardstorage/repo/manifests -type f -name 'manifest.json' 2>/dev/null | shuf -n 1": []byte(
				"/var/lib/pg_hardstorage/repo/manifests/dep/backups/b1/manifest.json\n"),
		},
	}
	ts := inject.NewStaticTargetSet([]inject.Target{repo}, 42)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"manifest_targeted_corruption(target=repo, count=3)", ts)
	if err != nil {
		t.Fatal(err)
	}
	// Expect two execs: the locate + the in-container flip.
	calls := repo.ExecCalls()
	if len(calls) != 2 {
		t.Fatalf("expected locate + flip (2 execs); got %d: %v", len(calls), calls)
	}
	flip := strings.Join(calls[1], " ")
	for _, want := range []string{
		"stat -c %s /var/lib/pg_hardstorage/repo/manifests/dep/backups/b1/manifest.json",
		"seq 1 3",
		"shuf -i 0-",
		"dd if=/var/lib/pg_hardstorage/repo/manifests/dep/backups/b1/manifest.json",
		"conv=notrunc",
	} {
		if !strings.Contains(flip, want) {
			t.Errorf("flip script should contain %q; got %v", want, flip)
		}
	}
}

func TestManifestCorruption_NoManifestSkipsCleanly(t *testing.T) {
	// Early-soak window: the repo has no committed backups
	// yet, so manifests/**/manifest.json is empty.  The fault
	// must skip cleanly (no error, no write) — the soak's first
	// run found that the previous "no manifest.json" error
	// counted every transient pre-first-backup tick as a
	// fault_apply_failed event.  A vacuous skip is the
	// correct outcome: there is nothing to corrupt, so the
	// fault is satisfied.
	repo := &inject.FakeTarget{
		NameStr: "repo-0",
		RoleStr: "repo",
		ExecResponses: map[string][]byte{
			"sh -c find /var/lib/pg_hardstorage/repo/manifests -type f -name 'manifest.json' 2>/dev/null | shuf -n 1": []byte(""),
		},
	}
	ts := inject.NewStaticTargetSet([]inject.Target{repo}, 42)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"manifest_targeted_corruption(target=repo)", ts)
	if err != nil {
		t.Errorf("expected clean skip when no manifest exists; got %v", err)
	}
	if w := repo.Written(); len(w) != 0 {
		t.Errorf("no manifest → no writes; got %v", w)
	}
}

func TestManifestCorruption_CountPropagatesToFlipScript(t *testing.T) {
	// Verifies count= reaches the flip-loop's `seq 1 N`.  The
	// actual byte-level effect happens inside the container
	// and is integration-tested in the 4-slot soak.
	repo := &inject.FakeTarget{
		NameStr: "repo-0",
		RoleStr: "repo",
		ExecResponses: map[string][]byte{
			"sh -c find /repo/m -type f -name 'manifest.json' 2>/dev/null | shuf -n 1": []byte("/repo/m/x/manifest.json\n"),
		},
	}
	ts := inject.NewStaticTargetSet([]inject.Target{repo}, 42)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"manifest_targeted_corruption(target=repo, prefix=/repo/m, count=8)", ts)
	if err != nil {
		t.Fatal(err)
	}
	flip := strings.Join(repo.ExecCalls()[1], " ")
	if !strings.Contains(flip, "seq 1 8") {
		t.Errorf("flip script should loop seq 1 8 for count=8; got %v", flip)
	}
}

// --- concurrent_repo_writer ------------------------------------------

func TestConcurrentRepoWriter_RequiresDeploymentPgConnRepo(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	cases := []struct {
		action string
		want   string
	}{
		{`concurrent_repo_writer(target=pg, pg_conn=x, repo=y)`, `"deployment"`},
		{`concurrent_repo_writer(target=pg, deployment=d, repo=y)`, `"pg_conn"`},
		{`concurrent_repo_writer(target=pg, deployment=d, pg_conn=x)`, `"repo"`},
	}
	for _, c := range cases {
		_, err := inject.DefaultRegistry.Apply(context.Background(), c.action, ts)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("action %q: want error containing %q; got %v", c.action, c.want, err)
		}
	}
}

func TestConcurrentRepoWriter_LaunchesBackgroundAgent(t *testing.T) {
	pg := &inject.FakeTarget{
		NameStr:       "pg-0",
		RoleStr:       "pg",
		ExecResponses: map[string][]byte{},
	}
	ts := inject.NewStaticTargetSet([]inject.Target{pg}, 42)
	rec, err := inject.DefaultRegistry.Apply(context.Background(),
		`concurrent_repo_writer(target=pg, deployment=mydb, pg_conn=postgres://x, repo=file:///r)`, ts)
	// Apply errors because FakeTarget returns "" (no pidfile path),
	// which the fault rejects honestly.  Assert the script
	// content regardless — Apply's docker exec was issued.
	if err == nil || !strings.Contains(err.Error(), "no pidfile path") {
		t.Fatalf("expected no-pidfile-path error from empty exec response; got %v", err)
	}
	calls := pg.ExecCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one exec; got %d: %v", len(calls), calls)
	}
	joined := strings.Join(calls[0], " ")
	// Script must launch via setsid+su, name the binary, and
	// pass the three required CLI flags from the args.
	for _, want := range []string{
		"setsid",
		"/usr/local/bin/pg_hardstorage backup 'mydb'",
		"--pg-connection 'postgres://x'",
		"--repo 'file:///r'",
		"--include-wal",
		"--stall-timeout 5m",
		"pgbackup",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("script should contain %q; got %v", want, joined)
		}
	}
	// Recovery returned a closure; calling it is a no-op here
	// (the pidfile path was empty, so the closure has nothing
	// to clean up).  Important: it must not panic.
	if rec != nil {
		// (rec is nil when Apply errored above; defensive guard.)
		_ = rec(context.Background())
	}
}

// --- truncated_wal_segment -------------------------------------------

func TestTruncatedWALSegment_FindsAndTruncates(t *testing.T) {
	repo := &inject.FakeTarget{
		NameStr: "repo-0",
		RoleStr: "repo",
		ExecResponses: map[string][]byte{
			"sh -c find /var/lib/pg_hardstorage/repo/wal -type f 2>/dev/null | shuf -n 1": []byte(
				"/var/lib/pg_hardstorage/repo/wal/dep/1/000000010000000000000007.json\n"),
		},
	}
	ts := inject.NewStaticTargetSet([]inject.Target{repo}, 42)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"truncated_wal_segment(target=repo, bytes=1024)", ts)
	if err != nil {
		t.Fatal(err)
	}
	calls := repo.ExecCalls()
	if len(calls) != 2 {
		t.Fatalf("expected find + truncate (2 execs); got %d: %v", len(calls), calls)
	}
	if calls[1][0] != "truncate" || calls[1][1] != "-s" || calls[1][2] != "-1024" ||
		calls[1][3] != "/var/lib/pg_hardstorage/repo/wal/dep/1/000000010000000000000007.json" {
		t.Errorf("second exec should `truncate -s -1024 <path>`; got %v", calls[1])
	}
}

func TestTruncatedWALSegment_BadBytes(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"truncated_wal_segment(target=repo, bytes=0)", ts)
	if err == nil || !strings.Contains(err.Error(), "positive int") {
		t.Errorf("expected positive-int error; got %v", err)
	}
}

func TestTruncatedWALSegment_NoFilesSkipsCleanly(t *testing.T) {
	// Same early-soak skip semantics as manifest_corruption:
	// before the streamer commits its first WAL, the wal/
	// prefix is empty; the fault must skip rather than error.
	repo := &inject.FakeTarget{
		NameStr: "repo-0",
		RoleStr: "repo",
		ExecResponses: map[string][]byte{
			"sh -c find /var/lib/pg_hardstorage/repo/wal -type f 2>/dev/null | shuf -n 1": []byte(""),
		},
	}
	ts := inject.NewStaticTargetSet([]inject.Target{repo}, 42)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"truncated_wal_segment(target=repo)", ts)
	if err != nil {
		t.Errorf("expected clean skip when no WAL exists; got %v", err)
	}
	// One exec (the find that returned ""); NO truncate.
	calls := repo.ExecCalls()
	for _, c := range calls {
		if c[0] == "truncate" {
			t.Errorf("no WAL → no truncate; got %v", c)
		}
	}
}

// --- missing_wal_segment ---------------------------------------------

func TestMissingWALSegment_FindsAndRemoves(t *testing.T) {
	repo := &inject.FakeTarget{
		NameStr: "repo-0",
		RoleStr: "repo",
		ExecResponses: map[string][]byte{
			"sh -c find /repo/wal -type f 2>/dev/null | shuf -n 1": []byte(
				"/repo/wal/dep/1/000000010000000000000009.json\n"),
		},
	}
	ts := inject.NewStaticTargetSet([]inject.Target{repo}, 42)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"missing_wal_segment(target=repo, prefix=/repo/wal)", ts)
	if err != nil {
		t.Fatal(err)
	}
	calls := repo.ExecCalls()
	if len(calls) != 2 {
		t.Fatalf("expected find + rm (2 execs); got %d: %v", len(calls), calls)
	}
	if calls[1][0] != "rm" || calls[1][1] != "-f" ||
		calls[1][2] != "/repo/wal/dep/1/000000010000000000000009.json" {
		t.Errorf("second exec should `rm -f <path>`; got %v", calls[1])
	}
}

// --- torn_page -------------------------------------------------------

func TestTornPage_DispatchesShellWithDDOverwriteSecondSector(t *testing.T) {
	pg := &inject.FakeTarget{NameStr: "pg-0", RoleStr: "pg"}
	ts := inject.NewStaticTargetSet([]inject.Target{pg}, 42)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"torn_page(target=pg)", ts)
	if err != nil {
		t.Fatal(err)
	}
	calls := pg.ExecCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one apply exec; got %d: %v", len(calls), calls)
	}
	joined := strings.Join(calls[0], " ")
	for _, want := range []string{
		"find \"$pgdata/base\" -mindepth 2 -maxdepth 2 -type f",
		// PGDATA template DBs (OIDs 1, 4) excluded.
		`grep -vE "/base/(1|4)/"`,
		// System catalogs (relfilenode < 16384) also excluded —
		// the 8h soak's run #6 saw torn /base/5/2703.1 make
		// the whole cluster unreadable for every subsequent
		// iteration.  Awk strips the .segno/_fsm/_vm suffix
		// and keeps only relfilenodes >= 16384 (PG's
		// FirstNormalObjectId).
		"FirstNormalObjectId",
		`n + 0 >= 16384`,
		// dd with notrunc to overwrite the page tail.
		"dd if=/dev/urandom",
		"conv=notrunc",
		// Default tear: page_size=8192, tear_at=4096, tear_len=4096.
		"page_size=8192",
		"tear_at=4096",
		"tear_len=4096",
		"pages=1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("script should contain %q; got %v", want, joined)
		}
	}
}

func TestTornPage_BadPageSize(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"torn_page(target=pg, page_size=8000)", ts)
	if err == nil || !strings.Contains(err.Error(), "multiple of 512") {
		t.Errorf("expected page_size-multiple-of-512 error; got %v", err)
	}
}

func TestTornPage_TearAtOutOfRange(t *testing.T) {
	ts, _, _, _ := fixtureSet(t)
	_, err := inject.DefaultRegistry.Apply(context.Background(),
		"torn_page(target=pg, tear_at=8192)", ts)
	if err == nil || !strings.Contains(err.Error(), "[0, page_size)") {
		t.Errorf("expected tear_at-range error; got %v", err)
	}
}
