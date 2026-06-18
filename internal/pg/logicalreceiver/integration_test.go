//go:build integration

// End-to-end logical-replication tests for the logicalreceiver
// package. These spin a real PostgreSQL via the pg testkit, create a
// publication + logical slot, and drive Stream against live decoded
// traffic — coverage the unit file cannot provide and the existing
// L3 logical scenarios deliberately skip (they are negative-path
// only, because before the `sql` scenario step landed there was no
// way to issue CREATE PUBLICATION).
package logicalreceiver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	pgtestkit "github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

// countingSink decodes just enough of the pgoutput protocol to count
// message kinds by their leading type byte ('B'=Begin, 'I'=Insert,
// 'C'=Commit, 'R'=Relation). It tracks the highest ServerWALEnd seen
// and reports it as SyncedLSN so the receive loop's standby-status
// feedback advances the slot's confirmed_flush_lsn.
type countingSink struct {
	mu       sync.Mutex
	inserts  int
	updates  int
	deletes  int
	truncs   int
	begins   int
	commits  int
	relers   int
	frames   int
	lastLSN  pglogrepl.LSN
	descLSNs bool // set true if any frame's WALStart went backwards
	prevLSN  pglogrepl.LSN
}

func (s *countingSink) OnRecord(_ context.Context, rec logicalreceiver.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames++
	if len(rec.Data) > 0 {
		switch rec.Data[0] {
		case 'B':
			s.begins++
		case 'I':
			s.inserts++
		case 'U':
			s.updates++
		case 'D':
			s.deletes++
		case 'T':
			s.truncs++
		case 'C':
			s.commits++
		case 'R':
			s.relers++
		}
	}
	// Frames must arrive in non-decreasing WALStart order — the sink
	// commits manifests in LSN order and relies on it.
	if rec.WALStart != 0 && rec.WALStart < s.prevLSN {
		s.descLSNs = true
	}
	if rec.WALStart > s.prevLSN {
		s.prevLSN = rec.WALStart
	}
	if rec.ServerWALEnd > s.lastLSN {
		s.lastLSN = rec.ServerWALEnd
	}
	return nil
}

func (s *countingSink) SyncedLSN() pglogrepl.LSN {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastLSN
}

// Flush satisfies the Sink interface. countingSink commits nothing —
// it advances lastLSN eagerly in OnRecord — so Flush is a no-op.
func (s *countingSink) Flush(context.Context) error { return nil }

// sinkCounts is a lock-free snapshot of a countingSink — safe to copy
// and return by value (the live sink can't be, it holds a Mutex).
type sinkCounts struct {
	inserts, updates, deletes, truncs int
	begins, commits, relers, frames   int
	lastLSN                           pglogrepl.LSN
	descLSNs                          bool
}

func (s *countingSink) snapshot() sinkCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sinkCounts{inserts: s.inserts, updates: s.updates, deletes: s.deletes,
		truncs: s.truncs, begins: s.begins, commits: s.commits, relers: s.relers,
		frames: s.frames, lastLSN: s.lastLSN, descLSNs: s.descLSNs}
}

// pubArgs builds the pgoutput START_REPLICATION plugin args the CLI
// uses (proto_version 2 + the publication list).
func pubArgs(publication string) []string {
	return []string{
		"proto_version '2'",
		fmt.Sprintf("publication_names '%s'", publication),
	}
}

// setupPublication creates a table + publication and returns the
// open *sql.DB (closed via t.Cleanup) for subsequent DML.
func setupPublication(t *testing.T, dsn, table, publication string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v text)", table)); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE %s", publication, table)); err != nil {
		t.Fatalf("create publication: %v", err)
	}
	return db
}

// makeSlot opens a short-lived replication connection, creates the
// logical slot, and registers a cleanup drop.
func makeSlot(t *testing.T, dsn, slot string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := pg.Connect(ctx, dsn, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect replication: %v", err)
	}
	if err := logicalreceiver.CreateLogicalSlot(ctx, c, slot, ""); err != nil {
		c.Close(ctx)
		t.Fatalf("create logical slot: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() {
		bg := context.Background()
		dc, err := pg.Connect(bg, dsn, pg.ModeReplication)
		if err != nil {
			return
		}
		defer dc.Close(bg)
		_ = logicalreceiver.DropLogicalSlot(bg, dc, slot)
	})
}

// TestStream_EndToEnd_InsertsFlow is the core happy-path test: every
// row inserted after the slot is created must surface as an 'I'
// (Insert) pgoutput message through Stream's sink.
func TestStream_EndToEnd_InsertsFlow(t *testing.T) {
	pgInst := pgtestkit.StartPostgres(t)
	const (
		table = "lr_e2e_t"
		pub   = "lr_e2e_pub"
		slot  = "lr_e2e_slot"
		rows  = 250
	)
	db := setupPublication(t, pgInst.DSN, table, pub)
	makeSlot(t, pgInst.DSN, slot)

	// Insert AFTER the slot exists so every row is in the slot's
	// decoded range.
	insCtx, insCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer insCancel()
	for i := 0; i < rows; i++ {
		if _, err := db.ExecContext(insCtx,
			fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table),
			i, fmt.Sprintf("row-%d", i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	streamConn, err := pg.Connect(context.Background(), pgInst.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect stream: %v", err)
	}
	sink := &countingSink{}
	// InactivityTimeout ends the stream once the backlog is drained
	// and PG goes quiet — that is the test's normal terminator.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err = logicalreceiver.Stream(ctx, streamConn, logicalreceiver.StreamOptions{
		Slot:                 slot,
		StartLSN:             0,
		PluginArgs:           pubArgs(pub),
		StatusUpdateInterval: time.Second,
		InactivityTimeout:    6 * time.Second,
	}, sink)
	if err == nil {
		t.Fatal("Stream should end via inactivity timeout, got nil")
	}
	if !errMentions(err, "inactivity timeout") {
		t.Fatalf("Stream ended with %v, want an inactivity-timeout error", err)
	}

	got := sink.snapshot()
	if got.inserts != rows {
		t.Errorf("decoded %d Insert messages, want %d (frames=%d begins=%d commits=%d relations=%d)",
			got.inserts, rows, got.frames, got.begins, got.commits, got.relers)
	}
	if got.relers == 0 {
		t.Error("expected at least one Relation message before the first Insert")
	}
	if got.lastLSN == 0 {
		t.Error("sink never advanced its SyncedLSN — standby feedback would never move")
	}

	// The slot's confirmed_flush_lsn must have advanced from the
	// standby status updates Stream sent on the ticker.
	var confirmed sql.NullString
	if err := db.QueryRowContext(ctx,
		"SELECT confirmed_flush_lsn FROM pg_replication_slots WHERE slot_name=$1",
		slot).Scan(&confirmed); err != nil {
		t.Fatalf("query confirmed_flush_lsn: %v", err)
	}
	if !confirmed.Valid || confirmed.String == "" {
		t.Error("confirmed_flush_lsn is NULL/empty — slot feedback never reached PG")
	}
}

// TestStream_ContextCancel — cancelling the context must unblock
// Stream promptly with a context error, not hang on ReceiveMessage.
func TestStream_ContextCancel(t *testing.T) {
	pgInst := pgtestkit.StartPostgres(t)
	const (
		table = "lr_cancel_t"
		pub   = "lr_cancel_pub"
		slot  = "lr_cancel_slot"
	)
	setupPublication(t, pgInst.DSN, table, pub)
	makeSlot(t, pgInst.DSN, slot)

	streamConn, err := pg.Connect(context.Background(), pgInst.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect stream: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- logicalreceiver.Stream(ctx, streamConn, logicalreceiver.StreamOptions{
			Slot:                 slot,
			PluginArgs:           pubArgs(pub),
			StatusUpdateInterval: time.Second,
			// No InactivityTimeout: the only way out is ctx cancel.
		}, &countingSink{})
	}()

	// Let the stream settle, then cancel.
	time.Sleep(2 * time.Second)
	cancel()

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Errorf("Stream after cancel returned %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Stream did not return within 15s of context cancel — it is stuck")
	}
}

// TestStream_InactivityTimeout — with no traffic at all, Stream must
// return the inactivity-timeout error within roughly the configured
// budget. This is the path that creates a per-iteration deadline
// context; a regression of the defer-in-loop leak would still pass
// this test functionally, but running it under -count exercises the
// path repeatedly.
func TestStream_InactivityTimeout(t *testing.T) {
	pgInst := pgtestkit.StartPostgres(t)
	const (
		table = "lr_idle_t"
		pub   = "lr_idle_pub"
		slot  = "lr_idle_slot"
	)
	setupPublication(t, pgInst.DSN, table, pub)
	makeSlot(t, pgInst.DSN, slot)

	streamConn, err := pg.Connect(context.Background(), pgInst.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect stream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	err = logicalreceiver.Stream(ctx, streamConn, logicalreceiver.StreamOptions{
		Slot:                 slot,
		PluginArgs:           pubArgs(pub),
		StatusUpdateInterval: time.Second,
		InactivityTimeout:    4 * time.Second,
	}, &countingSink{})
	elapsed := time.Since(start)

	if err == nil || !errMentions(err, "inactivity timeout") {
		t.Fatalf("Stream returned %v, want inactivity-timeout error", err)
	}
	// PG sends keepalives, so each one resets the inactivity window;
	// the bound is "doesn't hang forever", not a tight deadline.
	if elapsed > 25*time.Second {
		t.Errorf("inactivity timeout took %s — far longer than expected", elapsed)
	}
}

// TestCreateLogicalSlot_Idempotent — creating the same slot twice
// must succeed both times (the duplicate-object path), so `logical
// add` can be re-run without churn.
func TestCreateLogicalSlot_Idempotent(t *testing.T) {
	pgInst := pgtestkit.StartPostgres(t)
	const slot = "lr_idem_slot"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := pg.Connect(ctx, pgInst.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close(ctx)
	t.Cleanup(func() {
		bg := context.Background()
		dc, err := pg.Connect(bg, pgInst.DSN, pg.ModeReplication)
		if err != nil {
			return
		}
		defer dc.Close(bg)
		_ = logicalreceiver.DropLogicalSlot(bg, dc, slot)
	})

	if err := logicalreceiver.CreateLogicalSlot(ctx, c, slot, ""); err != nil {
		t.Fatalf("first CreateLogicalSlot: %v", err)
	}
	if err := logicalreceiver.CreateLogicalSlot(ctx, c, slot, ""); err != nil {
		t.Errorf("second CreateLogicalSlot must be idempotent, got %v", err)
	}
}

// runStreamToIdle streams the slot into a fresh countingSink until the
// inactivity timeout fires (the test terminator), then returns the
// sink snapshot.
func runStreamToIdle(t *testing.T, dsn, slot, pub string, inactivity time.Duration) sinkCounts {
	t.Helper()
	streamConn, err := pg.Connect(context.Background(), dsn, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect stream: %v", err)
	}
	sink := &countingSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	err = logicalreceiver.Stream(ctx, streamConn, logicalreceiver.StreamOptions{
		Slot:                 slot,
		PluginArgs:           pubArgs(pub),
		StatusUpdateInterval: time.Second,
		InactivityTimeout:    inactivity,
	}, sink)
	if err == nil || !errMentions(err, "inactivity timeout") {
		t.Fatalf("Stream ended with %v, want inactivity-timeout error", err)
	}
	got := sink.snapshot()
	if got.descLSNs {
		t.Error("a frame's WALStart went backwards — receive order is not monotonic")
	}
	return got
}

// TestStream_DMLVariety — INSERT / UPDATE / DELETE / TRUNCATE must all
// decode into their pgoutput message kinds. The table has a primary
// key, so its default replica identity carries the key for UPDATE and
// DELETE.
func TestStream_DMLVariety(t *testing.T) {
	pgInst := pgtestkit.StartPostgres(t)
	const (
		table = "lr_dml_t"
		pub   = "lr_dml_pub"
		slot  = "lr_dml_slot"
	)
	db := setupPublication(t, pgInst.DSN, table, pub)
	makeSlot(t, pgInst.DSN, slot)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(fmt.Sprintf("INSERT INTO %s SELECT g, 'v'||g FROM generate_series(1,40) g", table))
	exec(fmt.Sprintf("UPDATE %s SET v = 'updated' WHERE id <= 10", table))
	exec(fmt.Sprintf("DELETE FROM %s WHERE id > 30", table))
	exec(fmt.Sprintf("TRUNCATE %s", table))

	got := runStreamToIdle(t, pgInst.DSN, slot, pub, 6*time.Second)
	if got.inserts != 40 {
		t.Errorf("inserts = %d, want 40", got.inserts)
	}
	if got.updates != 10 {
		t.Errorf("updates = %d, want 10", got.updates)
	}
	if got.deletes != 10 {
		t.Errorf("deletes = %d, want 10 (rows 31..40)", got.deletes)
	}
	if got.truncs != 1 {
		t.Errorf("truncates = %d, want 1", got.truncs)
	}
}

// TestStream_HighVolume drives 20k rows across 20 transactions — well
// past a single sink batch — and asserts every Insert surfaces and the
// manifest LSN ordering holds. This is the "big database" change-volume
// stress for the chunked-sink batcher.
func TestStream_HighVolume(t *testing.T) {
	pgInst := pgtestkit.StartPostgres(t)
	const (
		table   = "lr_vol_t"
		pub     = "lr_vol_pub"
		slot    = "lr_vol_slot"
		txns    = 20
		perTxn  = 1000
		wantRow = txns * perTxn
	)
	db := setupPublication(t, pgInst.DSN, table, pub)
	makeSlot(t, pgInst.DSN, slot)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for txn := 0; txn < txns; txn++ {
		base := txn * perTxn
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s SELECT g, md5(g::text) FROM generate_series($1::int, $2::int) g", table),
			base+1, base+perTxn); err != nil {
			t.Fatalf("txn %d insert: %v", txn, err)
		}
	}

	got := runStreamToIdle(t, pgInst.DSN, slot, pub, 8*time.Second)
	if got.inserts != wantRow {
		t.Errorf("inserts = %d, want %d", got.inserts, wantRow)
	}
	if got.begins != txns || got.commits != txns {
		t.Errorf("begins/commits = %d/%d, want %d/%d (one per transaction)",
			got.begins, got.commits, txns, txns)
	}
}

// TestStream_ResumeFromConfirmedFlush is the resume-correctness test:
// stream batch A to idle, insert batch B, stream again. The second
// stream starts from StartLSN 0, which makes PG resume from the slot's
// confirmed_flush_lsn — so it must see ONLY batch B. If our standby
// status feedback never advanced confirmed_flush, the second stream
// re-delivers batch A (inserts=200); if it over-advanced, batch B is
// partially skipped (inserts<100).
func TestStream_ResumeFromConfirmedFlush(t *testing.T) {
	pgInst := pgtestkit.StartPostgres(t)
	const (
		table = "lr_resume_t"
		pub   = "lr_resume_pub"
		slot  = "lr_resume_slot"
	)
	db := setupPublication(t, pgInst.DSN, table, pub)
	makeSlot(t, pgInst.DSN, slot)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	insert := func(lo, hi int) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s SELECT g, 'v'||g FROM generate_series($1::int,$2::int) g", table),
			lo, hi); err != nil {
			t.Fatalf("insert %d..%d: %v", lo, hi, err)
		}
	}

	insert(1, 100)
	first := runStreamToIdle(t, pgInst.DSN, slot, pub, 6*time.Second)
	if first.inserts != 100 {
		t.Fatalf("first stream: inserts = %d, want 100", first.inserts)
	}

	insert(101, 200)
	second := runStreamToIdle(t, pgInst.DSN, slot, pub, 6*time.Second)
	if second.inserts != 100 {
		t.Errorf("resume delivered %d inserts, want exactly 100 (batch B only) — "+
			"200 means confirmed_flush never advanced and batch A was re-sent; "+
			"<100 means it over-advanced and skipped rows", second.inserts)
	}
}

// TestStream_SlotAlreadyActive — a logical slot allows only one
// consumer. A second Stream against an already-streaming slot must
// fail cleanly (PG rejects with "replication slot is active"), not
// hang or corrupt the first consumer.
func TestStream_SlotAlreadyActive(t *testing.T) {
	pgInst := pgtestkit.StartPostgres(t)
	const (
		table = "lr_active_t"
		pub   = "lr_active_pub"
		slot  = "lr_active_slot"
	)
	setupPublication(t, pgInst.DSN, table, pub)
	makeSlot(t, pgInst.DSN, slot)

	// First consumer: long-lived, cancelled at test end.
	conn1, err := pg.Connect(context.Background(), pgInst.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect 1: %v", err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go func() {
		_ = logicalreceiver.Stream(ctx1, conn1, logicalreceiver.StreamOptions{
			Slot:                 slot,
			PluginArgs:           pubArgs(pub),
			StatusUpdateInterval: time.Second,
		}, &countingSink{})
	}()
	time.Sleep(3 * time.Second) // let consumer 1 attach

	// Second consumer on the same slot must be refused.
	conn2, err := pg.Connect(context.Background(), pgInst.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect 2: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	err = logicalreceiver.Stream(ctx2, conn2, logicalreceiver.StreamOptions{
		Slot:                 slot,
		PluginArgs:           pubArgs(pub),
		StatusUpdateInterval: time.Second,
	}, &countingSink{})
	if err == nil {
		t.Fatal("second Stream on an active slot returned nil — must be refused")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Stream hung until ctx deadline instead of failing fast: %v", err)
	}
}

func errMentions(err error, sub string) bool {
	return err != nil && contains(err.Error(), sub)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
