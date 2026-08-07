package cli_test

// backup_undelete_walgap_cli_test.go — the WIRING proof for the
// resurrection WAL re-check: the shipped `backup undelete` command
// (not the helper called directly) must record the pruned window and
// say so in its result body. Helper-level tests live with the helper;
// this one exists because the campaign's burned lesson is that a
// working helper proves nothing about the command that must run it.

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

func TestBackupUndelete_RecordsPrunedWALGap(t *testing.T) {
	w := newReadWorld(t)
	ctx := context.Background()

	// A real committed backup stopping in segment 3...
	ts := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.20260501T090000Z",
		Deployment: "db1", Tenant: "default",
		Type: backup.BackupTypeFull, PGVersion: 17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/2000028", StopLSN: "0/3000000", Timeline: 1,
		StartedAt: ts, StoppedAt: ts.Add(30 * time.Second),
		BackupLabel: "START WAL LOCATION: 0/2000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{{Path: "PG_VERSION", Size: 3, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("17\n")), Offset: 0, Len: 3}}}},
	}
	if _, err := repo.NewCAS(w.sp).PutChunk(ctx, []byte("17\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Commit(ctx, m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	// ...tombstoned, then the janitors prune its forward window: only
	// segments 8-9 survive.
	if err := w.store.SoftDelete(ctx, "db1", m.BackupID, "manual", "aged out"); err != nil {
		t.Fatal(err)
	}
	for _, seg := range []uint64{8, 9} {
		name := walsink.SegmentFileName(1, seg, walsink.SegmentSize)
		start := pglogrepl.LSN(seg * uint64(walsink.SegmentSize))
		sm := &walsink.SegmentManifest{
			Schema: walsink.Schema, Deployment: "db1",
			SystemIdentifier: "7000000000000000001",
			Timeline:         1, SegmentNumber: seg, SegmentName: name,
			StartLSN: start.String(), EndLSN: (start + pglogrepl.LSN(walsink.SegmentSize)).String(),
			SegmentSize: walsink.SegmentSize, CreatedAt: time.Now().UTC(),
		}
		raw, err := sm.MarshalToBytes()
		if err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("wal/db1/%08X/%s.json", 1, name)
		if _, err := w.sp.Put(ctx, key, bytes.NewReader(raw),
			storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
			t.Fatal(err)
		}
	}

	stdout, errb, exit := runCLI(t, "backup", "undelete", "db1", m.BackupID,
		"--repo", w.repoURL, "--reason", "operator resurrect", "-o", "json")
	if exit != 0 {
		t.Fatalf("undelete exit = %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, errb)
	}
	var body struct {
		Outcomes []struct {
			BackupID       string `json:"backup_id"`
			Restored       bool   `json:"restored"`
			WALGapRecorded string `json:"wal_gap_recorded"`
		} `json:"outcomes"`
	}
	// stdout carries the resurrected_wal_gap EVENT before the result —
	// decode the stream of JSON values and unwrap the last one.
	dec := stdjson.NewDecoder(strings.NewReader(stdout))
	var last stdjson.RawMessage
	for {
		var v stdjson.RawMessage
		if err := dec.Decode(&v); err != nil {
			break
		}
		last = v
	}
	if last == nil {
		t.Fatalf("no JSON value on stdout:\n%s", stdout)
	}
	bodyOf(t, string(last), &body)
	if len(body.Outcomes) != 1 || !body.Outcomes[0].Restored {
		t.Fatalf("unexpected outcomes: %+v", body.Outcomes)
	}
	if got, want := body.Outcomes[0].WALGapRecorded, "0/3000000..0/8000000"; got != want {
		t.Fatalf("wal_gap_recorded = %q, want %q\n\nThe command resurrected a backup whose "+
			"forward WAL the janitors pruned and did not record the window — its "+
			"--to-latest will promote silently behind, and no preflight can refuse.", got, want)
	}
	recs, err := gapstate.New(w.sp).List(ctx, "db1")
	if err != nil || len(recs) != 1 {
		t.Fatalf("gap records = %d (err=%v), want 1", len(recs), err)
	}
	if recs[0].GapStartLSN != "0/3000000" || recs[0].GapEndLSN != "0/8000000" {
		t.Errorf("recorded window wrong: %+v", recs[0])
	}
}
