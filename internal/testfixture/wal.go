// Package testfixture holds helpers shared by tests across packages.
// It is imported only from _test.go files.
package testfixture

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// PlantArchivedWAL commits one WAL segment manifest for (deployment,
// timeline): the state of any deployment whose WAL is being archived.
//
// Restore refuses a backup that neither embeds its WAL nor has any
// archived WAL on its timeline (preflight.backup_wal_missing) — such a
// backup can never reach consistency; recovery would wait forever.
// Tests that build synthetic backups to exercise a restore must model
// working archiving, as every real deployment they stand for has.
func PlantArchivedWAL(tb testing.TB, sp storage.StoragePlugin, deployment string, timeline uint32) {
	tb.Helper()
	const segNum = 3
	name := walsink.SegmentFileName(timeline, segNum, walsink.SegmentSize)
	start := pglogrepl.LSN(segNum * uint64(walsink.SegmentSize))
	m := &walsink.SegmentManifest{
		Schema: walsink.Schema, Deployment: deployment, SystemIdentifier: "7000000000000000001",
		Timeline: timeline, SegmentNumber: segNum, SegmentName: name,
		StartLSN: start.String(), EndLSN: (start + pglogrepl.LSN(walsink.SegmentSize)).String(),
		SegmentSize: walsink.SegmentSize, CreatedAt: time.Now().UTC(),
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		tb.Fatal(err)
	}
	key := fmt.Sprintf("wal/%s/%08X/%s.json", deployment, timeline, name)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		tb.Fatalf("plant WAL %s: %v", key, err)
	}
}
