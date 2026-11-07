package backup

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// The WAL path and the backup path must agree on what a deployment name
// may be: the name is interpolated into storage keys on both, and a name
// one path accepts and the other refuses is either a hole or an outage.
// walsink cannot import backup (backup imports walsink), so the rule
// exists twice; this is what keeps the copies identical.
func TestWALDeploymentRuleMatchesBackupRule(t *testing.T) {
	for _, name := range []string{
		"", ".", "..", "db1", "1db", "a", "db.prod", "db..prod", "a b", "ünïcode",
		"../escape", "a/../../escape", "a/b", `a\b`, "a\x00b", "a\nb", "a\tb", "a\x7fb",
		"..%2f..%2fescape", strings.Repeat("x", 300), "-", "_",
	} {
		b, w := ValidateDeployment(name), walsink.ValidateDeployment(name)
		if (b == nil) != (w == nil) {
			t.Errorf("%q: backup rule says %v, WAL rule says %v", name, b, w)
		}
	}
}

func TestWALSegmentCounts(t *testing.T) {
	ctx := context.Background()
	sp := newLeaseSP(t)
	put := func(key string) {
		t.Helper()
		if _, err := sp.Put(ctx, key, strings.NewReader("{}"), storage.PutOptions{ContentLength: 2}); err != nil {
			t.Fatal(err)
		}
	}
	put(walsink.SegmentPath("db1", 1, "000000010000000000000001"))
	put(walsink.SegmentPath("db1", 1, "000000010000000000000002"))
	put(walsink.SegmentPath("db1", 2, "000000020000000000000003"))
	put(walsink.SegmentPath("walonly", 1, "000000010000000000000001"))
	// Not segment manifests: timeline history, gap records, a commit temp.
	put("wal/db1/history/00000002.history")
	put("wal/db1/gaps/00000001-0000000001000000.json")
	put("wal/quiet/timelines/1.history")
	put("manifests/hasbackup/backups/b1/manifest.json")

	got, err := WALSegmentCounts(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["db1"] != 3 || got["walonly"] != 1 {
		t.Fatalf("WALSegmentCounts = %v, want db1:3 walonly:1", got)
	}
}
