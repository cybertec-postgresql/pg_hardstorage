package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRepoScrub_RequiresURL surfaces the missing-arg
// error from `repo scrub` with no URL.  After the
// arg-error enrichment landed (see args_error.go),
// cobra's bare "accepts 1 arg(s)" is rewritten into the
// friendlier shape the operator actually wants — the
// assertion checks both the count and the placeholder
// name so a future regression can't slip the old shape
// back in.
func TestRepoScrub_RequiresURL(t *testing.T) {
	_, stderr, exit := runCLI(t, "repo", "scrub", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("missing URL should not exit 0\nstderr=%s", stderr)
	}
	for _, want := range []string{
		"needs 1 argument",
		"<url>", // the placeholder from `Use: "scrub <url>"`
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("expected missing-arg message to include %q; stderr=%s", want, stderr)
		}
	}
}

// TestRepoScrub_SamplePercentValidation rejects nonsense values.
func TestRepoScrub_SamplePercentValidation(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	for _, sp := range []string{"0", "101", "-5"} {
		_, stderr, exit := runCLI(t,
			"repo", "scrub", repoURL,
			"--sample-percent", sp,
			"-o", "json",
		)
		if exit != int(output.ExitMisuse) {
			t.Errorf("sample-percent=%s: exit=%d want ExitMisuse\nstderr=%s",
				sp, exit, stderr)
		}
	}
}

// TestRepoScrub_FreshRepoNoIntegrityFindings: a freshly-init'd
// repo has no chunks; scrub completes cleanly with mismatch_count=0.
func TestRepoScrub_FreshRepoNoIntegrityFindings(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	stdout, stderr, exit := runCLI(t,
		"repo", "scrub", repoURL,
		"--full",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("scrub on fresh repo: exit=%d\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"mismatch_count": 0`) {
		t.Errorf("expected mismatch_count=0 in result: %s", stdout)
	}
	if !strings.Contains(stdout, `"sample_percent": 100`) {
		t.Errorf("--full should imply sample_percent=100: %s", stdout)
	}
}

// TestRepoScrub_HumanReadableTextRender: confirm the text body has
// the operator-friendly summary lines (text mode is the cron-job
// default).
func TestRepoScrub_HumanReadableTextRender(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	// Force text mode — runCLI runs without a TTY so the
	// dispatcher's auto-detect picks JSON otherwise.
	stdout, _, exit := runCLI(t,
		"repo", "scrub", repoURL,
		"--full",
		"-o", "text",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("scrub: exit=%d\nstdout=%s", exit, stdout)
	}
	for _, want := range []string{
		"repo scrub — 100% sample",
		"Referenced chunks:",
		"Mismatches:        0",
		"no integrity findings",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text render missing %q:\n%s", want, stdout)
		}
	}
}

// TestRepoScrub_ResultIsCronFriendly: structure matches what an
// operator's cron-wired tool would parse (sample_percent + bytes
// + duration). Lock the schema down so monitoring tools depend
// on stable fields.
func TestRepoScrub_ResultIsCronFriendly(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	stdout, _, _ := runCLI(t,
		"repo", "scrub", repoURL,
		"--full",
		"-o", "json",
	)
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(res.Result)
	for _, want := range []string{
		`"sample_percent":`,
		`"bytes_scanned":`,
		`"duration_ms":`,
		`"started_at":`,
		`"stopped_at":`,
		`"mismatch_count":`,
		`"sampled":`,
		`"ok":`,
		`"referenced_total":`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing field %q:\n%s", want, body)
		}
	}
}
