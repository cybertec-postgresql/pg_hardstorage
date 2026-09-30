package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// M6: `repo gc -o json` must print ONE JSON document. The safety-floor
// warning (and the dry-run approval notice) were emitted as events
// beside the result, so `repo gc --apply --min-chunk-age 0 -o json | jq`
// saw two documents. In JSON mode they now travel in the result body.
func TestRepoGC_JSONIsOneDocument(t *testing.T) {
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"repo", "gc", repoURL, "--apply", "--min-chunk-age", "0", "--tombstone-grace", "0", "-o", "json"},
		{"repo", "gc", repoURL, "--require-approval", "nope", "-o", "json"},
	} {
		stdout, stderr, exit := runCmd(t, args...)
		if exit != int(output.ExitOK) {
			t.Fatalf("%v: exit=%d\n%s\n%s", args, exit, stdout, stderr)
		}
		dec := json.NewDecoder(strings.NewReader(stdout))
		var first map[string]any
		if err := dec.Decode(&first); err != nil {
			t.Fatalf("%v: stdout is not JSON: %v\n%s", args, err, stdout)
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			t.Fatalf("%v: stdout holds more than one JSON document (second: %v, err %v):\n%s", args, extra, err, stdout)
		}
		if strings.TrimSpace(stderr) != "" {
			t.Fatalf("%v: unexpected stderr output in JSON mode:\n%s", args, stderr)
		}
		if !strings.Contains(stdout, `"notices"`) {
			t.Errorf("%v: the warning must be carried in the result body:\n%s", args, stdout)
		}
	}
}
