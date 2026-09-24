package basebackup

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/streaming"
)

func archiveNotice(secs string) *pgproto3.NoticeResponse {
	return &pgproto3.NoticeResponse{Severity: "WARNING", Code: "01000",
		Message: "still waiting for all required WAL segments to be archived (" + secs + " seconds elapsed)"}
}

// While pg_backup_stop waits for WAL archiving, PostgreSQL sends only a
// WARNING at 60 s, 120 s, 240 s, ... — nothing else. The 90 s inactivity
// watchdog aborted every backup whose archiving took longer than ~2
// minutes, with an opaque "inactivity timeout". The warning must widen
// the window, and a stall that does happen must be reported as archive
// lag, not as a hung server.
func TestArchiveWait_WidensWindowAndExplainsLag(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const base = 150 * time.Millisecond

	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	var aw archiveWait
	var reader *streaming.Reader
	reader = streaming.NewWithConn(ctx, clientConn, streaming.Options{
		InactivityTimeout: base,
		OnNotice:          func(n *pgproto3.NoticeResponse) { aw.observe(n, reader, base) },
	})
	defer reader.Close()
	be := pgproto3.NewBackend(serverConn, serverConn)

	go func() {
		// Archive-wait warning (PG would say 60; 1 keeps the test fast),
		// then a silence well past the base window, then data.
		be.Send(archiveNotice("1"))
		_ = be.Flush()
		time.Sleep(3 * base)
		be.Send(&pgproto3.CopyData{Data: []byte("d")})
		_ = be.Flush()
	}()
	if _, err := reader.Receive(ctx); err != nil {
		t.Fatalf("archive-wait silence killed the backup: %v", err)
	}

	// A real stall after the warning: reported as archive lag.
	_, err := reader.Receive(ctx)
	if !errors.Is(err, streaming.ErrInactivityTimeout) {
		t.Fatalf("want inactivity timeout, got %v", err)
	}
	if got := aw.explain(err); !errors.Is(got, ErrArchiveLag) {
		t.Fatalf("stall after archive-wait warnings not explained as archive lag: %v", got)
	}
	// An unrelated inactivity timeout stays as-is.
	var fresh archiveWait
	if got := fresh.explain(err); errors.Is(got, ErrArchiveLag) {
		t.Fatalf("plain timeout mislabelled as archive lag: %v", got)
	}
}

func TestArchiveWaitSeconds(t *testing.T) {
	if s, ok := archiveWaitSeconds(archiveNotice("120")); !ok || s != 120 {
		t.Fatalf("got %d,%v", s, ok)
	}
	if _, ok := archiveWaitSeconds(&pgproto3.NoticeResponse{Severity: "WARNING", Message: "something else"}); ok {
		t.Fatal("unrelated notice matched")
	}
}
