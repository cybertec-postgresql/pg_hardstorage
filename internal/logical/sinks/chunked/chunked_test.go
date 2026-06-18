package chunked_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/chunked"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func TestSink_FlushOnRecord(t *testing.T) {
	dir := t.TempDir()
	sp := openFS(t, "file://"+dir)
	defer sp.Close()
	cas := casdefault.New(sp)

	s, err := chunked.New(cas, sp, chunked.Options{
		Deployment: "db1",
		StreamName: "events",
		Slot:       "pg_hardstorage_logical_events",
		Plugin:     "pgoutput",
		BatchBytes: 32, // tiny so a single record triggers the flush
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := logicalreceiver.Record{
		WALStart: pglogrepl.LSN(0x1000),
		Data:     make([]byte, 64), // > BatchBytes → flush
	}
	if err := s.OnRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if got := s.SyncedLSN(); uint64(got) <= 0x1000 {
		t.Errorf("SyncedLSN = %s; expected > 0x1000", got)
	}
}

func TestSink_FlushNoOpOnEmpty(t *testing.T) {
	dir := t.TempDir()
	sp := openFS(t, "file://"+dir)
	defer sp.Close()
	cas := casdefault.New(sp)

	s, err := chunked.New(cas, sp, chunked.Options{
		Deployment: "db1",
		StreamName: "events",
		Slot:       "pg_hardstorage_logical_events",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Errorf("Flush on empty sink should be a no-op; got %v", err)
	}
	if got := s.SyncedLSN(); got != 0 {
		t.Errorf("SyncedLSN should be 0 on a fresh sink; got %s", got)
	}
}

func TestSegmentPath(t *testing.T) {
	got := chunked.SegmentPath("db1", "events", pglogrepl.LSN(0x12345678))
	want := "logical/db1/events/0_12345678.json"
	if got != want {
		t.Errorf("SegmentPath = %q; want %q", got, want)
	}
}

func TestSink_Idempotent(t *testing.T) {
	dir := t.TempDir()
	sp := openFS(t, "file://"+dir)
	defer sp.Close()
	cas := casdefault.New(sp)

	mkSink := func() *chunked.Sink {
		s, err := chunked.New(cas, sp, chunked.Options{
			Deployment: "db1",
			StreamName: "events",
			Slot:       "pg_hardstorage_logical_events",
			BatchBytes: 16,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	rec := logicalreceiver.Record{
		WALStart: pglogrepl.LSN(0x2000),
		Data:     []byte("test record bytes 1234567890"),
	}
	s1 := mkSink()
	if err := s1.OnRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	// A second sink writing the same WALStart's batch should not
	// error — RenameIfNotExists keeps the existing manifest.
	s2 := mkSink()
	if err := s2.OnRecord(context.Background(), rec); err != nil {
		t.Errorf("re-flush of existing batch should be idempotent; got %v", err)
	}
}

func openFS(t *testing.T, repoURL string) storage.StoragePlugin {
	t.Helper()
	u, err := url.Parse(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	p := &fs.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	return p
}
