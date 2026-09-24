package cli_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestAudit_VerifyChain_FailureShowsFindings pins that a failed
// verify-chain still renders its findings body. The command returned
// only the error, whose message carries counts ("1 hash mismatch(es)"),
// so the operator learned THAT the chain was tampered with but not
// WHICH events -- the one thing needed to start the investigation.
func TestAudit_VerifyChain_FailureShowsFindings(t *testing.T) {
	repoURL := initRepoForTest(t)
	for i := 0; i < 3; i++ {
		if _, _, exit := runCmd(t, "audit", "append", "test.tick", "--repo", repoURL, "--output", "json"); exit != 0 {
			t.Fatalf("append %d: exit %d", i, exit)
		}
	}
	// Tamper with one stored event's body.
	root := strings.TrimPrefix(repoURL, "file://")
	tampered := false
	_ = filepath.WalkDir(filepath.Join(root, "audit"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || tampered {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil || !bytes.Contains(b, []byte("test.tick")) {
			return nil
		}
		if werr := os.WriteFile(p, bytes.Replace(b, []byte("test.tick"), []byte("test.tock"), 1), 0o644); werr != nil {
			t.Fatal(werr)
		}
		tampered = true
		return nil
	})
	if !tampered {
		t.Fatal("fixture: no stored audit event found to tamper with")
	}

	out, errb, exit := runCmd(t, "audit", "verify-chain", "--repo", repoURL, "--output", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("exit = %d, want %d\nstdout=%s\nstderr=%s", exit, output.ExitVerifyFailed, out, errb)
	}
	if !strings.Contains(out, `"hash_mismatches"`) || !strings.Contains(out, `"ok": false`) {
		t.Errorf("failed verify-chain dropped its findings body; stdout:\n%s", out)
	}
}
