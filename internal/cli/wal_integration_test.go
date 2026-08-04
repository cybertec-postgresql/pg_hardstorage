// Build-tagged integration test: drives `pg_hardstorage wal stream`
// end-to-end against a real PG 17 testcontainer. Run with
// `make test-integration`.
//
//go:build integration

package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/spf13/cobra"
)

// walStreamTimeout bounds a `wal stream --once` run from the test side.
//
// The streamer blocks until its segment commits, and nothing in these
// tests could interrupt it: cli.Run binds its own signal-derived
// context (root.go:58) and overwrites whatever a caller set, so a test
// cannot hand the command a deadline. The side goroutine that drives
// WAL has a context, but that context governs only the writer.
//
// When the segment never arrived, the run therefore blocked until the
// PACKAGE timeout. One such hang consumed the entire 10m budget for
// internal/cli and took several hundred unrelated tests down with it,
// reported only as `panic: test timed out after 10m0s` — the same
// package finished in ~220s on every other leg of the matrix.
//
// 3 minutes is far above the few seconds these runs take when WAL
// flows, so it fires only on a genuine stall. It is deliberately not
// tuned close to the observed time: the bound exists to stop a hang
// from consuming the package budget, not to assert performance, and a
// tight one just converts a slow runner into a red build. Even if one
// test does stall, the package still finishes well inside its 10m.
const walStreamTimeout = 3 * time.Minute

// walStreamSegBudget is the bound for a run that must commit a 64 MiB
// segment instead of the default 16 MiB — four times the data to write,
// checkpoint and upload.
//
// It is generous on purpose. A bound exists to stop a stall from
// consuming the package budget, not to assert performance; one set
// close to the observed time turns a slow runner into a red build. The
// 64 MiB case measured ~61s locally and ~97s on CI, where a shared 90s
// bound cut it off just short of finishing.
const walStreamSegBudget = 5 * time.Minute

// runWalStreamBounded executes root and returns its exit code, failing
// the test if it has not returned within budget.
//
// On timeout the command goroutine is deliberately left running: it is
// blocked reading the replication connection, and the container
// teardown that testkit.StartPostgres registers closes that connection
// moments later, so it unwinds on its own.
//
// That is also why the failure does not quote the command's output.
// The goroutine still holds the bytes.Buffers the root command writes
// to, and those are not safe for concurrent use — reading them here to
// enrich the message would trip the race detector and report a data
// race in the test harness instead of the stall that actually failed.
func runWalStreamBounded(t *testing.T, root *cobra.Command, budget time.Duration) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- cli.Run(root) }()
	exit, returned := awaitBounded(done, budget)
	switch {
	case returned:
		return exit
	default:
		t.Fatalf("`wal stream` did not return within %s — it was still waiting for the "+
			"segment that ends a --once run. Unbounded, it would block until the package "+
			"timeout and take every test after it down with it. Check the preceding "+
			"`driving WAL failed` error: if the side connection that produces WAL never "+
			"came up, no segment could ever commit.", budget)
		return -1
	}
}

// awaitBounded waits for done to deliver an exit code, or for budget
// to elapse. It reports (exit, true) when the command returned and
// (0, false) on timeout.
//
// Split out from runWalStreamBounded so the decision itself can be
// tested. Inline, the timeout arm ends in t.Fatalf, and a helper whose
// only failure path aborts the test cannot be exercised by one — which
// is how a bound protecting several hundred tests would end up being
// the only thing in the package with no coverage.
func awaitBounded(done <-chan int, budget time.Duration) (int, bool) {
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case exit := <-done:
		return exit, true
	case <-timer.C:
		// A select picks at random among cases that are BOTH ready, so
		// a command finishing just as the budget expires can lose the
		// draw. Check once more before reporting a stall: "did not
		// return within 3m" about a command that returned is the most
		// misleading thing this helper could say, because it sends the
		// reader looking for a hang that never happened.
		select {
		case exit := <-done:
			return exit, true
		default:
			return 0, false
		}
	}
}

// driveWAL produces WAL on a side connection until ctx is cancelled,
// which the caller does the moment the streamer returns.
//
// It loops deliberately. These tests used to write ONE burst after a
// fixed 500 ms head start, betting the streamer had created its
// replication slot by then. A slot created after the burst has already
// landed begins at an LSN past all of it, so `--once` waits for a
// segment boundary nothing will ever cross: the stream hangs even
// though every write succeeded and no error is reported anywhere. On a
// loaded CI runner that bet lost and the whole internal/cli package
// timed out.
//
// Looping removes the race rather than widening the window — whenever
// the slot appears, WAL is still being produced after it.
//
// rowsPerRound sets the per-round volume: 2048 rows of 16 KiB is about
// one 16 MiB segment's worth, so each round should close at least one
// segment.
func driveWAL(ctx context.Context, dsn, table string, rowsPerRound int) error {
	c, err := pg.Connect(ctx, dsn, pg.ModeRegular)
	if err != nil {
		return err
	}
	defer c.Close(ctx)

	exec := func(sql string) error {
		return c.PgConn().ExecParams(ctx, sql, nil, nil, nil, nil).Read().Err
	}
	if err := exec("SELECT pg_switch_wal()"); err != nil {
		return err
	}
	if err := exec("CREATE TABLE " + table + " (i int, payload text)"); err != nil {
		return err
	}
	insert := fmt.Sprintf(
		"INSERT INTO %s SELECT g, repeat('x', 16384) FROM generate_series(1, %d) g",
		table, rowsPerRound)

	for ctx.Err() == nil {
		for _, sql := range []string{insert, "CHECKPOINT", "SELECT pg_switch_wal()"} {
			if err := exec(sql); err != nil {
				// The caller cancels ctx as soon as the streamer is done,
				// which aborts whatever statement is in flight. That is
				// how this loop normally ends, not a failure.
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
	return nil
}

// TestIntegration_WalStream_OneSegment runs the actual cobra command
// in --once mode against a real PG: the test generates WAL in a side
// goroutine, the command commits its first segment, exits cleanly, and
// we assert the JSON Result reflects a clean stop with SyncedLSN
// advanced past the start.
func TestIntegration_WalStream_OneSegment(t *testing.T) {
	srv := testkit.StartPostgres(t)

	// Initialize the repo BEFORE running the command — repo.Open
	// refuses missing repos.
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}

	// Build a fresh root command and capture its output buffers.
	root := cli.NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"wal", "stream", "db1",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--status-interval", "250ms",
		"--once",
		"--output", "json",
	})

	// Generate WAL on a side connection while the command runs. We
	// produce more than one segment's worth so the --once watcher has
	// reliable signal to fire.
	walCtx, walCancel := context.WithTimeout(context.Background(), walStreamTimeout+30*time.Second)
	defer walCancel()
	var walDriveErr error
	var walWG sync.WaitGroup
	walWG.Add(1)
	go func() {
		defer walWG.Done()
		walDriveErr = driveWAL(walCtx, srv.DSN, "wal_test", 2048)
	}()

	// Run the command. --once should make it exit after the first
	// segment commits.
	exit := runWalStreamBounded(t, root, walStreamTimeout)
	walCancel()
	walWG.Wait()
	if walDriveErr != nil {
		t.Fatalf("driving WAL failed: %v — the streamer had no segment to observe, so "+
			"every assertion below would be about a run that never had input", walDriveErr)
	}

	if exit != 0 {
		t.Errorf("exit code = %d; want 0 (clean stop)\nstderr: %s", exit, stderr.String())
	}

	out := stdout.String()
	t.Logf("stdout: %s", out)

	// Loose JSON assertions — we don't unmarshal because the schema
	// envelope is still v1 and we'd just re-derive it.  Each
	// expected substring is given in both compact and indented
	// shapes (no space / one space after the colon) because the
	// output dispatcher pretty-prints by default.  A substring
	// match against either shape is enough — the schema additions
	// we want this test to survive are field-level, not formatting.
	for _, want := range [][2]string{
		{`"command":"pg_hardstorage wal stream"`, `"command": "pg_hardstorage wal stream"`},
		{`"deployment":"db1"`, `"deployment": "db1"`},
		{`"slot":"pg_hardstorage_db1"`, `"slot": "pg_hardstorage_db1"`},
		{`"clean_stop":true`, `"clean_stop": true`},
	} {
		if !strings.Contains(out, want[0]) && !strings.Contains(out, want[1]) {
			t.Errorf("stdout missing %q (or its indented form)", want[0])
		}
	}

	// The synced LSN must be a non-zero, segment-aligned value.
	if strings.Contains(out, `"synced_lsn":"0/0"`) || strings.Contains(out, `"synced_lsn": "0/0"`) {
		t.Errorf("SyncedLSN reported as 0/0 — segment did not commit:\n%s", out)
	}
}

// TestIntegration_WalStream_RefusesSystemIdentifierChange is the
// pg_upgrade guard, end-to-end: a deployment whose archived WAL was
// stamped with one cluster's system identifier must REFUSE to stream
// from a cluster with a different identifier (the pg_upgrade / clone /
// restore signature) — interleaving two clusters' WAL under one lineage
// would corrupt PITR. We plant a segment manifest carrying a bogus
// "old" identifier, then point `wal stream` at a real cluster (whose
// identifier is some unrelated random value) and assert the refusal.
func TestIntegration_WalStream_RefusesSystemIdentifierChange(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatalf("repo open: %v", err)
	}
	defer sp.Close()

	// Plant a committed segment manifest from a DIFFERENT ("pre-upgrade")
	// cluster. The container's real identifier is a random ~19-digit
	// value, so this fixed bogus one will not collide.
	const oldSys = "1234567890123456789"
	name := walsink.SegmentFileName(1, 7, walsink.SegmentSize)
	m := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       "db1",
		SystemIdentifier: oldSys,
		Timeline:         1,
		SegmentNumber:    7,
		SegmentName:      name,
		StartLSN:         "0/7000000",
		EndLSN:           "0/8000000",
		SegmentSize:      16 << 20,
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	key := walsink.SegmentPath("db1", 1, name)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}

	root := cli.NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"wal", "stream", "db1",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--once",
		"--output", "json",
	})
	exit := runWalStreamBounded(t, root, walStreamTimeout)
	if exit != int(output.ExitPreflight) {
		t.Fatalf("exit = %d, want ExitPreflight(%d) on a system-identifier change\nstdout: %s\nstderr: %s",
			exit, int(output.ExitPreflight), stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "preflight.system_identifier_changed") {
		t.Errorf("expected preflight.system_identifier_changed:\n%s", stderr.String())
	}
	// The override flag (--allow-system-identifier-change) bypassing the
	// guard is covered by the unit test TestGuardSystemIdentifier.
}

// TestIntegration_WalSegSize_ProbeMatchesInitdb validates the live
// wal_segment_size probe (pg.QueryWALSegmentSize) against real clusters
// initialised both ways: the default 16 MiB and a non-default 64 MiB set
// via `initdb --wal-segsize=64`. The pure decision logic is unit-tested
// in wal_segsize_test.go; this pins that the actual SQL probe the guard
// relies on returns the cluster's real geometry.
func TestIntegration_WalSegSize_ProbeMatchesInitdb(t *testing.T) {
	cases := []struct {
		name      string
		initdb    string
		wantBytes int64
	}{
		{"default_16MiB", "", 16 << 20},
		{"nondefault_64MiB", "--wal-segsize=64", 64 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var srv *testkit.Postgres
			if tc.initdb == "" {
				srv = testkit.StartPostgres(t)
			} else {
				srv = testkit.StartPostgresWithInitdbArgs(t, tc.initdb)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := pg.Connect(ctx, srv.DSN, pg.ModeRegular)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer c.Close(ctx)
			got, err := pg.QueryWALSegmentSize(ctx, c)
			if err != nil {
				t.Fatalf("QueryWALSegmentSize: %v", err)
			}
			if got != tc.wantBytes {
				t.Errorf("wal_segment_size = %d bytes, want %d", got, tc.wantBytes)
			}
		})
	}
}

// TestIntegration_WalStream_RefusesNonDefaultWalSegSize is the
// end-to-end guard test: a cluster initialised with a non-16 MiB
// wal_segment_size (`initdb --wal-segsize=64`) must make `wal stream`
// REFUSE up front with the preflight.wal_segment_size error — the
// streamer chops WAL at a hard-coded 16 MiB and would otherwise emit
// segments PG cannot fetch back at recovery. This exercises the whole
// path (probe SQL → guard → command exit), not just the helper.
// TestIntegration_WalStream_NonDefaultWalSegSize is the end-to-end
// proof that pg_hardstorage now SUPPORTS a non-default wal_segment_size:
// it streams from a 64 MiB-segment cluster (`initdb --wal-segsize=64`),
// commits a real 64 MiB segment named the way PG names it, and the
// restore_command path (`wal fetch`) reassembles it to exactly 64 MiB.
func TestIntegration_WalStream_NonDefaultWalSegSize(t *testing.T) {
	const segBytes = 64 << 20
	srv := testkit.StartPostgresWithInitdbArgs(t, "--wal-segsize=64")

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}

	root := cli.NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"wal", "stream", "db1",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--status-interval", "250ms",
		"--once",
		"--output", "json",
	})

	// Generate well over one 64 MiB segment of WAL so the --once watcher
	// has a full segment to commit. The writer must outlive the stream's
	// own budget, or it falls silent while the streamer is still waiting.
	walCtx, walCancel := context.WithTimeout(context.Background(), walStreamSegBudget+30*time.Second)
	defer walCancel()
	var walDriveErr error
	var walWG sync.WaitGroup
	walWG.Add(1)
	go func() {
		defer walWG.Done()
		walDriveErr = driveWAL(walCtx, srv.DSN, "wal_test", 8192)
	}()

	exit := runWalStreamBounded(t, root, walStreamSegBudget)
	walCancel()
	walWG.Wait()
	if walDriveErr != nil {
		t.Fatalf("driving WAL failed: %v — the streamer had no segment to observe, so "+
			"every assertion below would be about a run that never had input", walDriveErr)
	}

	if exit != 0 {
		t.Fatalf("exit = %d; want 0 (clean stop streaming a 64 MiB cluster)\nstderr: %s", exit, stderr.String())
	}

	// Inspect the committed segment manifest: it must record the 64 MiB
	// segment size, and its EndLSN must be one segment past its StartLSN.
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	var (
		segName string
		segMan  *walsink.SegmentManifest
	)
	for info, lerr := range sp.List(context.Background(), "wal/db1/00000001/") {
		if lerr != nil {
			t.Fatal(lerr)
		}
		k := info.Key
		if !strings.HasSuffix(k, ".json") || strings.Contains(k, ".json.tmp.") {
			continue
		}
		rc, gerr := sp.Get(context.Background(), k)
		if gerr != nil {
			t.Fatal(gerr)
		}
		raw, _ := io.ReadAll(rc)
		_ = rc.Close()
		m, perr := walsink.ParseSegmentManifest(raw)
		if perr != nil {
			continue
		}
		segMan = m
		segName = m.SegmentName
		break
	}
	if segMan == nil {
		t.Fatalf("no committed 64 MiB segment manifest found\nstdout: %s", stdout.String())
	}
	if segMan.SegmentSize != segBytes {
		t.Errorf("manifest SegmentSize = %d, want %d (64 MiB)", segMan.SegmentSize, segBytes)
	}
	// Name must be canonical 24-hex and parse back at 64 MiB.
	if _, _, perr := walsink.ParseSegmentName(segName, segBytes); perr != nil {
		t.Errorf("segment name %q does not parse at 64 MiB: %v", segName, perr)
	}

	// restore_command path: fetch the segment back and confirm it
	// reassembles to exactly 64 MiB.
	target := filepath.Join(t.TempDir(), "fetched.wal")
	fr := cli.NewRoot()
	var fout, ferr bytes.Buffer
	fr.SetOut(&fout)
	fr.SetErr(&ferr)
	fr.SetArgs([]string{
		"wal", "fetch", "db1", segName, target,
		"--repo", repoURL, "--output", "json",
	})
	if fexit := cli.Run(fr); fexit != 0 {
		t.Fatalf("wal fetch exit=%d for 64 MiB segment %q\nstderr: %s", fexit, segName, ferr.String())
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat fetched segment: %v", err)
	}
	if st.Size() != segBytes {
		t.Errorf("fetched segment size = %d, want %d (64 MiB)", st.Size(), segBytes)
	}
}

// TestIntegration_WalStream_FreshSlot_NoWALLossUnderLoad regresses
// the bug where ensureSlot used CreatePhysicalSlot (no RESERVE_WAL),
// leaving restart_lsn unpinned during the gap between slot create
// and START_REPLICATION.  Under a busy primary PG could recycle
// past the segment the streamer wanted to start from, surfacing as
//
//	ERROR: requested WAL segment 0000...002E has already been removed
//
// With the fix (ensureSlot → replication.EnsureSlot which uses
// RESERVE_WAL) the slot pins WAL the moment it's created, so the
// streamer's first START_REPLICATION lands on a segment PG
// provably retains.
//
// The test pumps WAL aggressively immediately AFTER the streamer
// starts — emulating the seed-phase race we hit in
// L3_wal_stream_continuous — and asserts the streamer commits at
// least one segment cleanly.
func TestIntegration_WalStream_FreshSlot_NoWALLossUnderLoad(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}

	// Saturate the primary with WAL on a side goroutine.  This
	// runs continuously; the --once flag in the streamer is what
	// terminates the test.
	loadCtx, loadCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer loadCancel()
	var loadWG sync.WaitGroup
	loadWG.Add(1)
	go func() {
		defer loadWG.Done()
		c, err := pg.Connect(loadCtx, srv.DSN, pg.ModeRegular)
		if err != nil {
			return
		}
		defer c.Close(loadCtx)
		_ = c.PgConn().ExecParams(loadCtx,
			"CREATE TABLE freshslot_test (i int, payload text)",
			nil, nil, nil, nil).Read()
		// Drive WAL until the test is done.  Each batch fills
		// roughly one segment; the loop produces many segments
		// over the test window.
		for {
			if loadCtx.Err() != nil {
				return
			}
			_ = c.PgConn().ExecParams(loadCtx,
				"INSERT INTO freshslot_test SELECT g, repeat('x', 16384) FROM generate_series(1, 2048) g",
				nil, nil, nil, nil).Read()
			_ = c.PgConn().ExecParams(loadCtx, "SELECT pg_switch_wal()", nil, nil, nil, nil).Read()
		}
	}()

	root := cli.NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"wal", "stream", "freshslot",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--status-interval", "250ms",
		"--once",
		"--output", "json",
	})
	exit := runWalStreamBounded(t, root, walStreamTimeout)
	loadCancel()
	loadWG.Wait()

	if exit != 0 {
		t.Errorf("exit code = %d; want 0 (clean stop)\nstdout: %s\nstderr: %s",
			exit, stdout.String(), stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "has already been removed") {
		t.Errorf("streamer hit the pre-fix WAL-recycled error:\n%s", out)
	}
	if !strings.Contains(out, `"clean_stop":true`) && !strings.Contains(out, `"clean_stop": true`) {
		t.Errorf("streamer did not stop cleanly:\n%s", out)
	}
	// The fresh-deployment path must use the slot's restart_lsn
	// (not the legacy fresh-no-slot fallback).  resume_strategy
	// is in the start event, not the result body — the result
	// body just shows the start LSN itself; assert it's
	// non-zero, which is the bare-minimum signal that the slot
	// pinned a real position.
	if strings.Contains(out, `"start_lsn":"0/0"`) || strings.Contains(out, `"start_lsn": "0/0"`) {
		t.Errorf("start LSN was 0/0 — slot did not pin a real position:\n%s", out)
	}
}

// TestIntegration_WalStream_VerboseEmitsProgress regresses issue #53:
// `pg_hardstorage wal stream --verbose` must emit one
// `wal.stream.progress` event per --status-interval against a real
// PG (not just the unit-test fake), with the body fields operators
// rely on — last_segment_streamed, current_partial_segment,
// bytes_per_second.
//
// Why this needs the real-PG path: the unit tests in
// wal_stream_progress_test.go cover buildProgressEvent and the
// ticker goroutine in isolation, but they cannot prove that
// streamAttempt actually wires the ticker between the
// `wal.stream.starting` emit and the in-flight Stream call — a
// future refactor of streamAttempt could silently break that
// wiring with all unit tests still green.
func TestIntegration_WalStream_VerboseEmitsProgress(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}

	root := cli.NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	// Force text output: progress events flow through the
	// dispatcher's Event channel, which the streamer suppresses
	// under --output json (the v1 streamer's design — JSON
	// consumers parse the final result, not the event stream).
	// In go test, stdout is not a TTY so the default would
	// auto-select JSON; --output text mirrors what an operator
	// sees in an interactive terminal (the exact shape the
	// issue-#53 bug report shows).
	root.SetArgs([]string{
		"wal", "stream", "vdb",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--status-interval", "100ms",
		"--once",
		"--verbose",
		"--output", "text",
	})

	// Drive WAL on the side so a segment commits and --once
	// fires.  Same shape as TestIntegration_WalStream_OneSegment.
	walCtx, walCancel := context.WithTimeout(context.Background(), walStreamTimeout+30*time.Second)
	defer walCancel()
	var walDriveErr error
	var walWG sync.WaitGroup
	walWG.Add(1)
	go func() {
		defer walWG.Done()
		walDriveErr = driveWAL(walCtx, srv.DSN, "vt", 2048)
	}()

	exit := runWalStreamBounded(t, root, walStreamTimeout)
	walCancel()
	walWG.Wait()
	if walDriveErr != nil {
		t.Fatalf("driving WAL failed: %v — the streamer had no segment to observe, so "+
			"every assertion below would be about a run that never had input", walDriveErr)
	}

	if exit != 0 {
		t.Fatalf("exit = %d; want 0\nstdout: %s\nstderr: %s",
			exit, stdout.String(), stderr.String())
	}
	out := stdout.String()
	t.Logf("stdout: %s", out)

	// The progress event MUST appear at least once — with a
	// 100 ms interval and the seed/setup work that runs before
	// Stream starts, several ticks are guaranteed.  Text
	// renderer formats it as "[INFO ] wal.stream.progress …".
	if !strings.Contains(out, "wal.stream.progress") {
		t.Fatalf("expected `wal.stream.progress` in --verbose stdout; got:\n%s", out)
	}
	// Body fields operators rely on per issue #53.  Substring
	// match (the text renderer emits them as JSON-ish "key": value
	// rows under `body:`); each must be present at least once.
	// Deliberately NOT in this list: `last_segment_streamed`.
	// That field is conditionally emitted (only once SyncedLSN
	// has crossed a segment boundary on this attempt), and
	// --once + short status-interval races with the first
	// commit.  TestBuildProgressEvent_BodyFields locks the
	// field's shape from the pure-function side; this test
	// covers only the unconditional body shape.
	for _, want := range []string{
		"current_partial_segment",
		"bytes_per_second",
		"tick_interval_ms",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("progress event body missing field %q in:\n%s", want, out)
		}
	}
}

// TestIntegration_WalStream_NoVerboseNoProgress is the complement:
// without --verbose, the streamer must NOT spam progress events
// (preserves the pre-#53 behaviour for operators who explicitly
// do not opt in).
func TestIntegration_WalStream_NoVerboseNoProgress(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}

	root := cli.NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"wal", "stream", "qdb",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--status-interval", "100ms",
		"--once",
		"--output", "text",
		// no --verbose
	})

	walCtx, walCancel := context.WithTimeout(context.Background(), walStreamTimeout+30*time.Second)
	defer walCancel()
	var walDriveErr error
	var walWG sync.WaitGroup
	walWG.Add(1)
	go func() {
		defer walWG.Done()
		walDriveErr = driveWAL(walCtx, srv.DSN, "qt", 2048)
	}()

	exit := runWalStreamBounded(t, root, walStreamTimeout)
	walCancel()
	walWG.Wait()
	if walDriveErr != nil {
		t.Fatalf("driving WAL failed: %v — the streamer had no segment to observe, so "+
			"every assertion below would be about a run that never had input", walDriveErr)
	}

	if exit != 0 {
		t.Fatalf("exit = %d; want 0", exit)
	}
	if strings.Contains(stdout.String(), "wal.stream.progress") {
		t.Errorf("without --verbose, progress events must NOT emit; got:\n%s", stdout.String())
	}
}

// TestIntegration_WalPreflight_HappyPath runs the standalone
// `wal preflight` subcommand against a default PG container.  Asserts
// exit 0 (no fatal findings) and that the JSON output carries the
// expected envelope.
func TestIntegration_WalPreflight_HappyPath(t *testing.T) {
	srv := testkit.StartPostgres(t)

	root := cli.NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"wal", "preflight", "db1",
		"--pg-connection", srv.DSN,
		"--output", "json",
	})
	exit := cli.Run(root)
	if exit != 0 {
		t.Errorf("exit code = %d; want 0 (preflight should pass on default PG)\nstderr: %s",
			exit, stderr.String())
	}
	out := stdout.String()
	// Tolerate both compact and indented JSON shapes (default
	// dispatcher pretty-prints) — same fix as the once-test above.
	for _, want := range [][2]string{
		{`"command":"pg_hardstorage wal preflight"`, `"command": "pg_hardstorage wal preflight"`},
		{`"deployment":"db1"`, `"deployment": "db1"`},
	} {
		if !strings.Contains(out, want[0]) && !strings.Contains(out, want[1]) {
			t.Errorf("stdout missing %q (or its indented form) in:\n%s", want[0], out)
		}
	}
}
