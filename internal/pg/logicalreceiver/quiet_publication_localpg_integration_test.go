//go:build integration

package logicalreceiver_test

// Runs against a throwaway cluster from the local initdb/pg_ctl (no
// Docker): export PATH=/usr/lib/postgresql/17/bin:$PATH. Skipped when
// the binaries are absent.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
)

func startLocalLogicalPG(t *testing.T) string {
	t.Helper()
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		t.Skip("initdb not on PATH (export PATH=/usr/lib/postgresql/17/bin:$PATH)")
	}
	pgctl := filepath.Join(filepath.Dir(initdb), "pg_ctl")
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	sock, err := os.MkdirTemp("", "pgsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	if out, err := exec.Command(initdb, "-D", data, "-U", "postgres", "--auth=trust", "-N").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	opts := fmt.Sprintf("-p %d -k %s -c listen_addresses='' -c wal_level=logical -c wal_sender_timeout=2s", port, sock)
	if out, err := exec.Command(pgctl, "-D", data, "-o", opts, "-l", filepath.Join(dir, "log"), "-w", "start").CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(pgctl, "-D", data, "-m", "immediate", "stop").Run() })
	return fmt.Sprintf("host=%s port=%d user=postgres dbname=postgres sslmode=disable", sock, port)
}

// idleSink receives nothing (the publication is quiet) and so never
// moves its SyncedLSN.
type idleSink struct{}

func (idleSink) OnRecord(context.Context, logicalreceiver.Record) error { return nil }
func (idleSink) SyncedLSN() pglogrepl.LSN                              { return 0 }
func (idleSink) Flush(context.Context) error                           { return nil }

// A quiet publication on a busy database: the walsender decodes and
// discards every change and sends only keepalives. The slot's
// confirmed_flush_lsn was acknowledged from the sink's SyncedLSN alone,
// which never moved, so the slot pinned WAL forever. With nothing
// pending, the keepalive's WAL end must be acknowledged (as
// pg_recvlogical does).
func TestIntegration_LocalPG_QuietPublicationAdvancesSlot(t *testing.T) {
	dsn := startLocalLogicalPG(t)
	ctx := context.Background()
	c, err := pg.Connect(ctx, dsn, pg.ModeRegular)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	sqlExec := func(q string) string {
		t.Helper()
		res := c.PgConn().Exec(ctx, q)
		rs, err := res.ReadAll()
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if len(rs) > 0 && len(rs[len(rs)-1].Rows) > 0 && len(rs[len(rs)-1].Rows[0]) > 0 {
			return string(rs[len(rs)-1].Rows[0][0])
		}
		return ""
	}
	sqlExec(`CREATE TABLE quiet(id int primary key); CREATE TABLE busy(v text)`)
	sqlExec(`CREATE PUBLICATION quietpub FOR TABLE quiet`)
	sqlExec(`SELECT pg_create_logical_replication_slot('qslot', 'pgoutput')`)
	before := sqlExec(`SELECT confirmed_flush_lsn::text FROM pg_replication_slots WHERE slot_name='qslot'`)

	rc, err := pg.Connect(ctx, dsn, pg.ModeReplication)
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- logicalreceiver.Stream(sctx, rc, logicalreceiver.StreamOptions{
			Slot:                 "qslot",
			PluginArgs:           []string{"proto_version '1'", "publication_names 'quietpub'"},
			StatusUpdateInterval: 300 * time.Millisecond,
		}, idleSink{})
	}()
	// Busy database, quiet publication.
	for i := 0; i < 40; i++ {
		sqlExec(fmt.Sprintf(`INSERT INTO busy SELECT md5(g::text) FROM generate_series(1, %d) g`, 500))
		time.Sleep(100 * time.Millisecond)
	}
	sqlExec(`SELECT pg_switch_wal()`)
	<-done

	after := sqlExec(`SELECT confirmed_flush_lsn::text FROM pg_replication_slots WHERE slot_name='qslot'`)
	b, _ := pglogrepl.ParseLSN(before)
	a, _ := pglogrepl.ParseLSN(after)
	if a <= b {
		t.Fatalf("slot confirmed_flush_lsn did not advance on keepalive-only traffic (%s -> %s): a quiet publication pins WAL forever", before, after)
	}
}
