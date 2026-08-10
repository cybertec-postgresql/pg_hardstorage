package cli_test

// wal_fetch_sysid_cli_test.go — the COMMAND-level identity check, one
// level up from checkFetchSystemIdentifier's unit test.
//
// The unit test proves the predicate; this proves the wiring the
// abandoned two-cluster pg_upgrade journey wanted (see that memory):
// the real `wal fetch` command, reading a real segment manifest from a
// real repo, with --expect-system-identifier threaded from the flag,
// refuses a segment archived by a different cluster and delivers no
// file. This is what PostgreSQL's restore_command actually invokes at
// the sysid boundary, minus two live clusters — the manifest is
// written with an explicit foreign SystemIdentifier, which is all the
// guard reads.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

const (
	seedSysID    = "7000000000000000001"
	foreignSysID = "7999999999999999999"
)

// pushSegAs writes one real, valid WAL segment for (tli, segNum) under
// the given system_identifier and returns its canonical name. Real
// PushSegmentFile — same chunking, manifest, and commit path the
// archiver uses; only the stamped identity differs.
func pushSegAs(t *testing.T, repoURL, sysID string, tli uint32, segNum uint64) string {
	t.Helper()
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	name := walsink.SegmentFileName(tli, segNum, walsink.SegmentSize)
	body := make([]byte, walsink.SegmentSize)
	// A trivial non-zero fill so chunking has content; the guard reads
	// the manifest's sysid, not the bytes.
	for i := range body {
		body[i] = byte((segNum + uint64(i)) & 0xff)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := walsink.PushSegmentFile(context.Background(), repo.NewCAS(sp), sp, path,
		walsink.PushOptions{
			Deployment:       "db1",
			SystemIdentifier: sysID,
			SegmentSize:      walsink.SegmentSize,
		}); err != nil {
		t.Fatalf("push seg %s as %s: %v", name, sysID, err)
	}
	return name
}

func TestWalFetch_ForeignSystemIdentifier_RefusedByCommand(t *testing.T) {
	repoURL := initRepoForTest(t)
	// The seed cluster's own segment, then a contiguous segment
	// archived by a DIFFERENT cluster (the pg_upgrade / reused-name
	// shape).
	_ = pushSegAs(t, repoURL, seedSysID, 1, 5)
	foreign := pushSegAs(t, repoURL, foreignSysID, 1, 6)

	target := filepath.Join(t.TempDir(), "out.wal")
	stdout, stderr, exit := runCmd(t,
		"wal", "fetch", "db1", foreign, target,
		"--repo", repoURL,
		"--expect-system-identifier", seedSysID,
		"--output", "json",
	)
	if exit == 0 {
		t.Fatalf("wal fetch served a foreign-lineage segment (exit 0).\n\n"+
			"PostgreSQL's restore_command would replay another cluster's WAL into this "+
			"recovery. The command must refuse.\nstdout:\n%s", stdout)
	}
	out := stdout + stderr
	if !strings.Contains(out, "system_identifier_mismatch") {
		t.Errorf("refusal missing the typed code:\n%s", out)
	}
	if !strings.Contains(out, foreignSysID) || !strings.Contains(out, seedSysID) {
		t.Errorf("refusal should name both identities:\n%s", out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a refused fetch must write no file, got err=%v", err)
	}
}

func TestWalFetch_MatchingSystemIdentifier_Served(t *testing.T) {
	repoURL := initRepoForTest(t)
	name := pushSegAs(t, repoURL, seedSysID, 1, 5)

	target := filepath.Join(t.TempDir(), "out.wal")
	_, stderr, exit := runCmd(t,
		"wal", "fetch", "db1", name, target,
		"--repo", repoURL,
		"--expect-system-identifier", seedSysID,
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("wal fetch refused a segment from the SAME cluster (exit %d): %s", exit, stderr)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("matching-sysid fetch delivered no file: %v", err)
	}
	if info.Size() != walsink.SegmentSize {
		t.Errorf("delivered %d bytes, want a full %d-byte segment", info.Size(), walsink.SegmentSize)
	}
}

func TestWalFetch_NoExpectation_ServesEvenForeign(t *testing.T) {
	// Backward compatibility: an old restore_command with no
	// --expect-system-identifier must behave exactly as before —
	// serve whatever is archived. The guard arms only when the flag
	// is set.
	repoURL := initRepoForTest(t)
	foreign := pushSegAs(t, repoURL, foreignSysID, 1, 6)

	target := filepath.Join(t.TempDir(), "out.wal")
	_, stderr, exit := runCmd(t,
		"wal", "fetch", "db1", foreign, target,
		"--repo", repoURL,
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("wal fetch WITHOUT --expect-system-identifier refused a segment (exit %d): %s\n\n"+
			"an unarmed fetch must serve archived WAL unchanged, or every pre-#26 "+
			"restore_command breaks on upgrade", exit, stderr)
	}
}
