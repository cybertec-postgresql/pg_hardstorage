//go:build integration

// End-to-end coverage for the logical-decoding Runner. Issue #72
// (closes the gap left by PR #69, whose corresponding test was
// written against an older pg.Conn surface).
//
// What this protects against:
//
//   - A break in the supervised-runner glue between Manager (state
//     file), Runner (per-stream goroutines), the chunked Sink (writes
//     segment manifests + chunks to the repo), and logicalreceiver
//     (decodes pgoutput frames from the replication stream).  Unit
//     tests cover each piece in isolation; this is the first test
//     that proves they wire together against a real PG.
//
//   - A regression where Runner's per-stream supervisor swallows a
//     fatal error and silently quits.  We assert the chunked sink
//     COMMITTED at least one segment to the repo — the durable
//     evidence the pipeline actually ran end-to-end.
//
//   - A regression where the slot is never created (the Runner
//     does an idempotent CreateLogicalSlot before opening the
//     stream).  We probe the slot post-run; its absence would mean
//     the receiver was never wired up.
//
//   - A regression where ctx cancellation deadlocks Runner.Run.
//     We cancel after a short wall-clock window and require Run to
//     return within a generous timeout — anything stuck past it is
//     a goroutine leak.
//
// Scope intentionally bounded:
//
//   - One Stream against pgoutput.  The receiver-level test
//     (internal/pg/logicalreceiver/integration_test.go) already
//     covers DML variety, high-volume, resume-from-confirmed-flush,
//     and slot-already-active edges.  This test's contribution is
//     the Runner+Sink+Manager layer above the receiver.
//
//   - One INSERT batch with synchronous timing.  Hot-reload mid-run
//     and registry rescans are covered by runner_hotreload_test.go
//     against fakes; we keep this happy-path test simple.
//
// Wall-clock ≈ 15-25 s against the testkit's PG container.
package logical_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestRunner_EndToEnd_PgoutputChunkedSink drives the full Runner
// pipeline against a live PG: registers one stream via Manager,
// starts Runner.Run in a goroutine, INSERTs rows, then cancels the
// runner ctx and asserts a chunked-sink segment manifest landed in
// the repo.
func TestRunner_EndToEnd_PgoutputChunkedSink(t *testing.T) {
	srv := testkit.StartPostgres(t)

	// Set up the source DB: a table to decode and a publication
	// that covers it.  Publication must exist BEFORE we create the
	// logical slot, otherwise pgoutput's first START_REPLICATION
	// will error with "publication does not exist".
	const (
		streamName  = "lr_runner_e2e"
		deployment  = "lr_runner_e2e_dep"
		slotName    = "lr_runner_e2e_slot"
		publication = "lr_runner_e2e_pub"
		tableName   = "lr_runner_e2e_t"
		rowCount    = 200
	)
	bgCtx := context.Background()
	setupPublication(t, srv.DSN, tableName, publication)

	// Repo + state file.  The Manager's state file is a per-process
	// JSON; we land it in the testkit temp dir so a t.Cleanup will
	// wipe it with the rest of the test artefacts.
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(bgCtx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	statePath := filepath.Join(t.TempDir(), "logical-streams.json")
	mgr := logical.NewManager(statePath)
	if _, err := mgr.Add(logical.AddOptions{
		Name:        streamName,
		Deployment:  deployment,
		Slot:        slotName,
		Plugin:      "pgoutput",
		Publication: publication,
		SinkKind:    "chunked",
		RepoURL:     repoURL,
	}); err != nil {
		t.Fatalf("Manager.Add: %v", err)
	}

	// Drive the Runner.  ctx scopes the test; OnEvent captures
	// supervisor events so a regression that mis-classifies a
	// retry vs a hard fail is visible in test logs.
	runCtx, runCancel := context.WithTimeout(bgCtx, 90*time.Second)
	defer runCancel()

	// eventCount is incremented from OnEvent, which fires on
	// multiple per-stream supervisor goroutines concurrently —
	// race-detector caught the bare-int read/write at this very
	// line on the integration build.  atomic.Int64 keeps the
	// counter race-free without serialising the OnEvent callback.
	var eventCount atomic.Int64
	r := &logical.Runner{
		Manager: mgr,
		ConnectionFor: func(s *logical.Stream) string {
			// All streams in this test bind to the testkit's
			// single PG.  In production this resolves to the
			// agent's local DeploymentConfig.PGConnection.
			return srv.DSN
		},
		OnEvent: func(ev *output.Event) {
			eventCount.Add(1)
			// Surface critical/error events directly so a
			// stuck supervisor is debuggable from the test
			// log without bumping verbosity.
			if ev.Severity <= output.SeverityError {
				t.Logf("[runner event] %s/%s: %s", ev.Component, ev.Op, ev.Body)
			}
		},
		// Disable hot-reload polling — single-stream test, no
		// registry mutation while running.  Without this the
		// Runner spawns a 30-s ticker that just adds noise.
		RescanInterval: -1,
	}

	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(runCtx) }()

	// Give the Runner a beat to create the slot.  The supervisor
	// opens a replication conn, calls CreateLogicalSlot, then
	// opens a SECOND replication conn for Stream — so we need to
	// wait until the slot is visible to pg_replication_slots
	// before driving INSERTs.  Polling is cheap; the slot
	// typically appears within 100 ms.
	if err := waitForSlot(t, srv.DSN, slotName, 30*time.Second); err != nil {
		runCancel()
		<-runDone
		t.Fatalf("slot %q never appeared: %v", slotName, err)
	}

	// Drive a batch of INSERTs.  These flow through the live
	// replication stream → logicalreceiver decodes pgoutput
	// frames → chunked.Sink buffers + commits.
	db, err := sql.Open("pgx", srv.DSN)
	if err != nil {
		t.Fatalf("open pgx: %v", err)
	}
	defer db.Close()
	insCtx, insCancel := context.WithTimeout(bgCtx, 30*time.Second)
	defer insCancel()
	for i := 0; i < rowCount; i++ {
		if _, err := db.ExecContext(insCtx,
			fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", tableName),
			i, fmt.Sprintf("runner-e2e-row-%d", i)); err != nil {
			runCancel()
			<-runDone
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// Give the sink time to commit at least one batch.  chunked.Sink
	// flushes on a ticker AND on Flush() at shutdown; the
	// shutdown-flush is what guarantees durability before Run returns.
	// 5 s of post-INSERT slack covers the receive→decode→batch→commit
	// pipeline for a 200-row insert plus the standby-status round-trip.
	time.Sleep(5 * time.Second)

	// Trigger shutdown.  Runner.Run must:
	//   1. cancel each per-stream supervisor ctx
	//   2. let the supervisor drain (Flush with a fresh background
	//      ctx, see runStreamOnce)
	//   3. return nil
	runCancel()
	select {
	case err := <-runDone:
		// Run() returns the ctx cancellation error verbatim
		// when ctx is cancelled.  Treat ctx.Canceled as clean
		// shutdown.
		if err != nil && err != context.Canceled {
			t.Errorf("Runner.Run returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("Runner.Run did not return within 45s of ctx cancel — supervisor deadlock")
	}

	// Inventory the repo to prove chunked.Sink committed at least
	// one segment manifest.  The chunked sink writes to
	// `wal_logical/<deployment>/<stream>/<segment>.json` (per
	// chunked.SegmentPath); we list that prefix and require at
	// least one entry.
	_, sp, err := repo.Open(bgCtx, repoURL)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer sp.Close()

	manifestKeys := listSegmentManifests(t, sp, deployment, streamName)
	if len(manifestKeys) == 0 {
		t.Fatalf("no chunked-sink segment manifests landed in repo after %d INSERTs + 5s flush window (events seen: %d)",
			rowCount, eventCount.Load())
	}
	t.Logf("ran %d INSERTs through Runner; chunked sink committed %d segment manifest(s); supervisor emitted %d events",
		rowCount, len(manifestKeys), eventCount.Load())
	if t.Failed() {
		for _, k := range manifestKeys {
			t.Logf("  segment manifest: %s", k)
		}
	}
}

// setupPublication creates a single-table publication on the source
// DB and returns nothing.  Called BEFORE Manager.Add registers the
// stream; the Runner's first START_REPLICATION will fail if the
// publication doesn't exist yet.
//
// Mirrors the helper at internal/pg/logicalreceiver/integration_test.go's
// setupPublication; copied here so the test is self-contained
// (logicalreceiver's helper is unexported package-internal).
func setupPublication(t *testing.T, dsn, table, publication string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for publication setup: %v", err)
	}
	defer conn.Close(ctx)
	// REPLICA IDENTITY FULL so DELETE/UPDATE rows would also
	// decode (this test only does INSERTs, but a future extension
	// shouldn't have to remember to set this).
	for _, stmt := range []string{
		fmt.Sprintf("DROP TABLE IF EXISTS %s", table),
		fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v text)", table),
		fmt.Sprintf("ALTER TABLE %s REPLICA IDENTITY FULL", table),
		fmt.Sprintf("DROP PUBLICATION IF EXISTS %s", publication),
		fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE %s", publication, table),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup stmt %q: %v", stmt, err)
		}
	}
}

// waitForSlot polls pg_replication_slots for the named slot to
// appear, returning nil on first sighting or an error on timeout.
// The Runner creates the slot asynchronously in its supervisor
// goroutine; this lets the test wait without racing.
func waitForSlot(t *testing.T, dsn, slotName string, timeout time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open pgx for slot poll: %w", err)
	}
	defer db.Close()
	deadline := time.Now().Add(timeout)
	for {
		var n int
		err := db.QueryRowContext(ctx,
			"SELECT count(*) FROM pg_replication_slots WHERE slot_name = $1",
			slotName).Scan(&n)
		if err == nil && n > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("slot %q not present within %v (last query err: %v)",
				slotName, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// listSegmentManifests returns every chunked-sink segment manifest
// key for the given deployment+stream.  Used at the end of the
// happy-path test to assert at least one segment committed.
// Returns an empty slice (not an error) when nothing matches —
// that's the failure mode the caller wants to report.
func listSegmentManifests(t *testing.T, sp storage.StoragePlugin, deployment, streamName string) []string {
	t.Helper()
	// chunked.SegmentPath builds keys under
	// `logical/<deployment>/<stream>/<lsn>.json` (chunked.go:299).
	prefix := "logical/" + deployment + "/" + streamName + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got []string
	// The fs plugin's List is a generator-style iterator.  Drain
	// it; .Plugin is the concrete type but the StoragePlugin
	// interface exposes List via for-range.
	if fsp, ok := sp.(*fs.Plugin); ok {
		for obj, err := range fsp.List(ctx, prefix) {
			if err != nil {
				t.Logf("List error: %v", err)
				return got
			}
			if strings.HasSuffix(obj.Key, ".json") {
				got = append(got, obj.Key)
			}
		}
		return got
	}
	// Non-fs plugin: also try the generic List signature.
	for obj, err := range sp.List(ctx, prefix) {
		if err != nil {
			t.Logf("List error: %v", err)
			return got
		}
		if strings.HasSuffix(obj.Key, ".json") {
			got = append(got, obj.Key)
		}
	}
	return got
}
