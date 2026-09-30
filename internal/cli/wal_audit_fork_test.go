package cli_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

// TestWalAudit_NewTimelineMustCoverFromItsFork: TLI 1 is archived
// through segment 10, TLI 2 forked inside segment 8 (per its history
// file, captured into the repo's timeline store the way `wal stream`
// does) and is archived only from 11. Segment numbers alone read that as
// contiguous; TLI 2's segments 8..10 exist nowhere, so `wal audit` must
// fail.
func TestWalAudit_NewTimelineMustCoverFromItsFork(t *testing.T) {
	repoURL := initRepoForTest(t)
	for seg := uint64(0); seg <= 10; seg++ {
		plantWALSegment(t, repoURL, "db1", 1, seg)
	}
	plantWALSegment(t, repoURL, "db1", 2, 11)

	u, err := url.Parse(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	const segSize = 16 << 20
	hist := fmt.Sprintf("1\t0/%X\tno recovery target specified\n", 8*segSize+0x40)
	if err := timeline.New(sp).Put(context.Background(), "db1", 2, []byte(hist)); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runCmd(t, "wal", "audit", "db1", "--repo", repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("exit %d, want ExitVerifyFailed: TLI 2's segments 8..10 are missing\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	if !strings.Contains(stderr, "TLI 2: segments #8..#10") {
		t.Errorf("gap not reported as TLI 2 #8..#10:\n%s", stderr)
	}
}
