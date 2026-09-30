package chunked_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/commitlsn"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/chunked"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// sweptChunks makes every chunk look deleted to Stat once armed — what a
// concurrent `repo gc --apply` sweep looks like to the commit-time check.
type sweptChunks struct {
	storage.StoragePlugin
	armed atomic.Bool
}

func (s *sweptChunks) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if s.armed.Load() && strings.HasPrefix(key, "chunks/") {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return s.StoragePlugin.Stat(ctx, key)
}

// A logical stream holds no backup lease, so gc can sweep a chunk a batch
// merely deduplicated against. The batch must not commit over it, nor
// confirm its LSN to PostgreSQL.
func TestSink_AdoptedChunkSweptBeforeCommit_Refuses(t *testing.T) {
	sp := openFS(t, "file://"+t.TempDir())
	defer sp.Close()
	rec := logicalreceiver.Record{
		WALStart: pglogrepl.LSN(0x1000),
		Data:     append(commitlsn.Message(0x1040), make([]byte, 64)...),
	}
	opts := func(dep string) chunked.Options {
		return chunked.Options{Deployment: dep, StreamName: "events",
			Slot: "pg_hardstorage_logical_events", Plugin: "pgoutput", BatchBytes: 32}
	}
	// db1 writes the chunk; db2's identical batch will adopt it.
	a, err := chunked.New(casdefault.New(sp), sp, opts("db1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.OnRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	wrapped := &sweptChunks{StoragePlugin: sp}
	b, err := chunked.New(casdefault.New(wrapped), wrapped, opts("db2"))
	if err != nil {
		t.Fatal(err)
	}
	wrapped.armed.Store(true)
	err = b.OnRecord(context.Background(), rec)
	if !errors.Is(err, repo.ErrAdoptedChunkSwept) {
		t.Fatalf("batch over a swept adopted chunk: err = %v, want repo.ErrAdoptedChunkSwept", err)
	}
	if got := b.SyncedLSN(); got != 0 {
		t.Fatalf("SyncedLSN advanced to %s over a segment that did not commit", got)
	}
	for info, lerr := range sp.List(context.Background(), "logical/db2/") {
		if lerr == nil {
			t.Fatalf("a segment manifest was committed over the swept chunk: %s", info.Key)
		}
	}
}
