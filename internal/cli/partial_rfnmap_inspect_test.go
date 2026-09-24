package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Regression (H4): --relfilenode-map is documented to take what
// `partial inspect -o json` writes, but inspect names the heap path
// "heap_path" (inside result.table_mappings) while the loader only
// read "path". Every table came back NotFound, nothing was extracted,
// and the command exited 0. Both the whole inspect envelope and its
// bare table_mappings array must work.
func TestPartialRestore_RelfilenodeMapAcceptsInspectOutput(t *testing.T) {
	mappings := `[{"qualified":"public.users","oid":16390,"relfilenode":2619,` +
		`"heap_path":"base/16384/2619","heap_bytes":7,"toast_path":"base/16384/2620"}]`
	for name, body := range map[string]string{
		"envelope": `{"schema":"pg_hardstorage.result.v1","command":"pg_hardstorage partial inspect",` +
			`"result":{"deployment":"db1","backup_id":"db1.full.ins","table_mappings":` + mappings + `}}`,
		"table_mappings": mappings,
	} {
		t.Run(name, func(t *testing.T) {
			w := newReadWorld(t)
			w.commitWithFilesCLI(t, "db1", "db1.full.ins", []cliFileSpec{
				{"base/16384/2619", [][]byte{[]byte("present")}},
				{"base/16384/2620", [][]byte{[]byte("toast")}},
			})
			mapPath := filepath.Join(t.TempDir(), "inspect.json")
			if err := os.WriteFile(mapPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "extract")
			stdout, stderr, exit := runCLI(t, "partial", "restore", "db1",
				"--repo", w.repoURL, "--backup", "db1.full.ins", "--tables", "public.users",
				"--target", target, "--relfilenode-map", mapPath, "-o", "json")
			if exit != int(output.ExitOK) {
				t.Fatalf("exit=%d\n%s\n%s", exit, stdout, stderr)
			}
			for _, p := range []string{"base/16384/2619", "base/16384/2620"} {
				if _, err := os.Stat(filepath.Join(target, p)); err != nil {
					t.Errorf("%s not extracted: %v", p, err)
				}
			}
		})
	}
}
