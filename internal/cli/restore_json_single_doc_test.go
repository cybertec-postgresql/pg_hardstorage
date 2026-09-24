package cli_test

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Regression (M5): `restore <dep> latest -o json` (and `--to <time>`)
// emitted the latest_resolved_with_skips / time_target_resolved_with_
// skips warning as its own JSON document before the Result, so stdout
// carried two documents and `| jq` consumers broke. In JSON mode the
// stdout stream must be exactly one document: the Result.
func TestRestore_JSONModeEmitsExactlyOneDocument(t *testing.T) {
	for _, extra := range [][]string{nil, {"--to", "now"}} {
		name := "latest"
		if extra != nil {
			name = "latest-to-time"
		}
		t.Run(name, func(t *testing.T) {
			w := newReadWorld(t)
			defer w.cleanup()
			commitBackupLSN(t, w, "db1", "b1", "0/3000028", "0/30001A0", time.Now().UTC().Add(-2*time.Hour))
			// A manifest that lists but cannot be verified: the resolver
			// skips it and warns.
			if _, err := w.sp.Put(context.Background(), "manifests/db1/backups/b2/manifest.json",
				strings.NewReader(`{"schema":"broken"}`), storage.PutOptions{}); err != nil {
				t.Fatal(err)
			}

			args := append([]string{"restore", "db1", "latest", "--repo", w.repoURL,
				"--target", filepath.Join(t.TempDir(), "r"), "--verify", "skip", "--verify-restore", "off",
				"-o", "json"}, extra...)
			stdout, errb, exit := runCLI(t, args...)
			if exit != int(output.ExitOK) {
				t.Fatalf("exit=%d\nstdout:\n%s\nstderr:\n%s", exit, stdout, errb)
			}
			dec := json.NewDecoder(strings.NewReader(stdout))
			docs := 0
			for {
				var v map[string]any
				if err := dec.Decode(&v); err == io.EOF {
					break
				} else if err != nil {
					t.Fatalf("stdout is not a JSON stream: %v\n%s", err, stdout)
				}
				docs++
				// The warning's content moves into the Result body.
				if body, _ := v["result"].(map[string]any); body == nil || body["skipped_manifests"] != float64(1) {
					t.Errorf("result.skipped_manifests = %v, want 1:\n%s", body["skipped_manifests"], stdout)
				}
			}
			if docs != 1 {
				t.Fatalf("stdout carries %d JSON documents, want exactly 1:\n%s", docs, stdout)
			}
		})
	}
}
