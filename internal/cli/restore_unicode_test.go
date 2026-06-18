// restore_unicode_test.go — non-ASCII --to-name through the full CLI
// preview pipeline (flag parse → restore.Preview → JSON render).
//
// A PostgreSQL named restore point (--to-name) is free-form text, so
// operators in any locale use it. Unlike deployment names (ASCII
// slugs, gated by ValidDeploymentName) the restore-point name is NOT
// identifier-constrained. These tests pin that an Arabic / Cyrillic /
// CJK / emoji name survives flag parsing and round-trips through the
// JSON Result body byte-for-byte — no mojibake, no truncation.
package cli_test

import (
	stdjson "encoding/json"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestRestorePreview_UnicodeToName_RoundTrips(t *testing.T) {
	names := map[string]string{
		"arabic":         "نقطة-الاستعادة",
		"russian":        "точка-восстановления",
		"chinese":        "恢复点-生产",
		"japanese":       "復元ポイント",
		"emoji":          "restore-✅-🚀",
		"mixed_scripts":  "بيانات-данные-数据",
		"embedded_quote": "o'brien's-點",
	}

	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-unicode-name"))

	for label, name := range names {
		t.Run(label, func(t *testing.T) {
			stdout, stderr, exit := runRestore(t,
				"db1", id,
				"--repo", w.repoURL,
				"--target", t.TempDir()+"/restored",
				"--to-name", name,
				"--preview",
				"-o", "json",
			)
			if exit != int(output.ExitOK) {
				t.Fatalf("preview exit=%d\nstderr=%s", exit, stderr)
			}
			var res output.Result
			if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, stdout)
			}
			body, _ := res.Result.(map[string]any)
			rec, ok := body["recovery"].(map[string]any)
			if !ok {
				t.Fatalf("body missing recovery block: %v", body)
			}
			got, _ := rec["target_name"].(string)
			if got != name {
				t.Errorf("recovery.target_name round-trip mismatch:\n  want %q\n  got  %q", name, got)
			}
			// The other target kinds must be absent for a name target.
			if lsn, _ := rec["target_lsn"].(string); lsn != "" {
				t.Errorf("target_lsn should be empty for a name target; got %q", lsn)
			}
		})
	}
}
