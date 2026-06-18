package cli_test

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// twoRepoDirs sets up file:// repos at <tmp>/src and <tmp>/dst,
// initialises both, and returns the URLs.
func twoRepoDirs(t *testing.T) (srcURL, dstURL string) {
	t.Helper()
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	dstDir := filepath.Join(tmp, "dst")
	for _, d := range []string{srcDir, dstDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srcURL = "file://" + srcDir
	dstURL = "file://" + dstDir
	if _, _, exit := runCLI(t, "repo", "init", srcURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init src")
	}
	if _, _, exit := runCLI(t, "repo", "init", dstURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init dst")
	}
	return srcURL, dstURL
}

// plantBackupAtSrc writes a fake chunk + a manifest referencing it,
// directly via the storage plugin, so the CLI test doesn't depend on
// running a real backup pipeline.
func plantBackupAtSrc(t *testing.T, srcURL, deployment, backupID string, chunkBody []byte) {
	t.Helper()
	u, err := url.Parse(srcURL)
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	// Plant the chunk at its canonical key.
	h := repo.HashOf(chunkBody)
	chunkKey := repo.ChunkKey(h)
	if _, err := sp.Put(context.Background(), chunkKey,
		bytes.NewReader(chunkBody),
		storage.PutOptions{ContentLength: int64(len(chunkBody))}); err != nil {
		t.Fatal(err)
	}

	// Plant a minimal manifest referencing it.
	manifest := []byte(`{"backup_id":"` + backupID +
		`","files":[{"path":"data","chunks":[{"hash":"` + h.String() + `"}]}]}`)
	manifestKey := "manifests/" + deployment + "/backups/" + backupID + "/manifest.json"
	if _, err := sp.Put(context.Background(), manifestKey,
		bytes.NewReader(manifest),
		storage.PutOptions{ContentLength: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
}

// TestRepoReplicate_RequiresFromAndTo: the cobra-level missing-flag
// case — both --from and --to are mandatory.
func TestRepoReplicate_RequiresFromAndTo(t *testing.T) {
	for _, args := range [][]string{
		{"repo", "replicate", "-o", "json"},
		{"repo", "replicate", "--to", "file:///tmp/x", "-o", "json"},
		{"repo", "replicate", "--from", "file:///tmp/x", "-o", "json"},
	} {
		_, _, exit := runCLI(t, args...)
		if exit == int(output.ExitOK) {
			t.Errorf("missing flag should not exit 0: args=%v", args)
		}
	}
}

// TestRepoReplicate_RefusesSameURL: --from == --to is an obvious
// usage error (the operator-friendly version of "you didn't mean
// that").
func TestRepoReplicate_RefusesSameURL(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	srcURL := "file://" + src
	if _, _, exit := runCLI(t, "repo", "init", srcURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init")
	}
	_, _, exit := runCLI(t, "repo", "replicate",
		"--from", srcURL, "--to", srcURL, "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("same-url should map to ExitMisuse; got %d", exit)
	}
}

// TestRepoReplicate_DestNotARepo: --to without an HSREPO at it is
// caught at openRepo, mapped to notfound.repo / ExitNotFound.
func TestRepoReplicate_DestNotARepo(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "dst")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	srcURL := "file://" + src
	if _, _, exit := runCLI(t, "repo", "init", srcURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init src")
	}
	// dst not initialised on purpose.
	_, stderr, exit := runCLI(t, "repo", "replicate",
		"--from", srcURL, "--to", "file://"+dst, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("expected non-zero exit when dst has no HSREPO; stderr=%s", stderr)
	}
}

// TestRepoReplicate_HappyPath: plant a backup at src, run replicate,
// verify result fields + the chunk + manifest land at dst.
func TestRepoReplicate_HappyPath(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	plantBackupAtSrc(t, srcURL, "db1", "db1.full.20260430T0900Z", []byte("payload-bytes"))

	stdout, stderr, exit := runCLI(t,
		"repo", "replicate",
		"--from", srcURL, "--to", dstURL,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("replicate: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(res.Result)
	bodyStr := string(body)
	for _, want := range []string{
		`"manifests_copied":1`,
		`"chunks_copied":1`,
		`"manifests_failed":0`,
		`"chunks_failed":0`,
		`"source_url":"` + srcURL + `"`,
		`"dest_url":"` + dstURL + `"`,
	} {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("body missing %q:\n%s", want, bodyStr)
		}
	}
}

// TestRepoReplicate_DryRunWritesNothing: --dry-run reports the work
// but doesn't write to dst (a follow-up `repo usage` on dst would
// show no growth).
func TestRepoReplicate_DryRunWritesNothing(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	plantBackupAtSrc(t, srcURL, "db1", "db1.full.dry", []byte("dry-run-payload"))

	stdout, _, exit := runCLI(t,
		"repo", "replicate",
		"--from", srcURL, "--to", dstURL,
		"--dry-run",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("dry-run: exit=%d\n%s", exit, stdout)
	}
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(env.Result)
	if !strings.Contains(string(body), `"dry_run":true`) {
		t.Errorf("dry-run flag missing in body:\n%s", body)
	}

	// Verify nothing wound up at dst.
	u, _ := url.Parse(dstURL)
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if _, err := sp.Stat(context.Background(),
		"manifests/db1/backups/db1.full.dry/manifest.json"); err == nil {
		t.Error("dry-run wrote a manifest to dst")
	}
}

// TestRepoReplicate_TextRender confirms the operator-friendly text
// body has the punch-list summary lines a human reads after a cron
// run.
func TestRepoReplicate_TextRender(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	plantBackupAtSrc(t, srcURL, "db1", "db1.full.text", []byte("text-payload"))

	stdout, _, exit := runCLI(t,
		"repo", "replicate",
		"--from", srcURL, "--to", dstURL,
		"-o", "text",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("text mode: exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"repo replicate — " + srcURL + " → " + dstURL,
		"Manifests:",
		"Chunks:",
		"Bytes copied:",
		"replication clean",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text render missing %q:\n%s", want, stdout)
		}
	}
}

// TestRepoReplicate_RejectsNegativeMbps: --max-mbps must be >=0.
// A negative value is a usage error.
func TestRepoReplicate_RejectsNegativeMbps(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	_, stderr, exit := runCLI(t,
		"repo", "replicate",
		"--from", srcURL, "--to", dstURL,
		"--max-mbps", "-5",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("--max-mbps=-5 should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag: %s", stderr)
	}
}

// TestRepoReplicate_MaxMbpsThrottlesAndReportsInBody: a small
// payload at a tight cap should still complete (the burst absorbs
// it) and the JSON body should record the cap value the operator
// configured. We assert the body shape; wall-clock throttling is
// covered by the unit tests for the throttle package.
func TestRepoReplicate_MaxMbpsThrottlesAndReportsInBody(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	plantBackupAtSrc(t, srcURL, "db1", "db1.full.throttled", []byte("payload-bytes"))

	stdout, stderr, exit := runCLI(t,
		"repo", "replicate",
		"--from", srcURL, "--to", dstURL,
		"--max-mbps", "100",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("throttled replicate: exit=%d\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(env.Result)
	if !strings.Contains(string(body), `"max_mbps":100`) {
		t.Errorf("body missing max_mbps=100:\n%s", body)
	}
	// And the run still completed normally.
	for _, want := range []string{
		`"manifests_copied":1`,
		`"chunks_copied":1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("throttled run missing %q:\n%s", want, body)
		}
	}
}

// TestRepoReplicate_MaxMbpsZeroIsTransparent: --max-mbps 0 (default)
// is documented as unbounded; the body should NOT carry max_mbps
// (omitempty drops it) and behaviour matches a plain replicate.
func TestRepoReplicate_MaxMbpsZeroIsTransparent(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	plantBackupAtSrc(t, srcURL, "db1", "db1.full.unthrottled", []byte("payload"))

	stdout, _, exit := runCLI(t,
		"repo", "replicate",
		"--from", srcURL, "--to", dstURL,
		"--max-mbps", "0",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(env.Result)
	if strings.Contains(string(body), `"max_mbps":`) {
		t.Errorf("max_mbps=0 should be omitted from body via omitempty:\n%s", body)
	}
}

// TestRepoReplicate_MaxMbpsTextRender: text mode should mention the
// bandwidth cap when one is in effect.
func TestRepoReplicate_MaxMbpsTextRender(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	plantBackupAtSrc(t, srcURL, "db1", "db1.full.txt", []byte("payload"))

	stdout, _, exit := runCLI(t,
		"repo", "replicate",
		"--from", srcURL, "--to", dstURL,
		"--max-mbps", "50",
		"-o", "text",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, "Bandwidth cap: 50 Mbps") {
		t.Errorf("text render missing bandwidth-cap line:\n%s", stdout)
	}
}

// TestRepoReplicate_RejectsBothMbpsAndSchedule: --max-mbps and
// --schedule are mutually exclusive.
func TestRepoReplicate_RejectsBothMbpsAndSchedule(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	_, stderr, exit := runCLI(t,
		"repo", "replicate",
		"--from", srcURL, "--to", dstURL,
		"--max-mbps", "50",
		"--schedule", "09:00-18:00=10mbps",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("both flags should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("expected mutually-exclusive message: %s", stderr)
	}
}

// TestRepoReplicate_RejectsBadScheduleExpr: an unparseable
// --schedule surfaces as usage.bad_flag.
func TestRepoReplicate_RejectsBadScheduleExpr(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	_, stderr, exit := runCLI(t,
		"repo", "replicate",
		"--from", srcURL, "--to", dstURL,
		"--schedule", "not-a-real-window-expr",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("bad --schedule should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag: %s", stderr)
	}
}

// TestRepoReplicate_ScheduleReportsInBody: a parseable schedule
// shows up in the result body and the run completes normally.
func TestRepoReplicate_ScheduleReportsInBody(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	plantBackupAtSrc(t, srcURL, "db1", "db1.full.sched", []byte("payload"))

	stdout, stderr, exit := runCLI(t,
		"repo", "replicate",
		"--from", srcURL, "--to", dstURL,
		"--schedule", "Mon-Fri,09:00-18:00=50mbps",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("scheduled replicate: exit=%d\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(env.Result)
	if !strings.Contains(string(body), `"schedule":"Mon-Fri,09:00-18:00=50mbps"`) {
		t.Errorf("body missing schedule:\n%s", body)
	}
	for _, want := range []string{
		`"manifests_copied":1`,
		`"chunks_copied":1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// TestRepoReplicate_ScheduleTextRender: the text body shows the
// schedule line when --schedule is in effect.
func TestRepoReplicate_ScheduleTextRender(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	plantBackupAtSrc(t, srcURL, "db1", "db1.full.txt2", []byte("payload"))

	stdout, _, exit := runCLI(t,
		"repo", "replicate",
		"--from", srcURL, "--to", dstURL,
		"--schedule", "Mon-Fri,09:00-18:00=50mbps",
		"-o", "text",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, "Schedule:") || !strings.Contains(stdout, "Mon-Fri") {
		t.Errorf("text render missing schedule line:\n%s", stdout)
	}
}

// TestRepoReplicate_Idempotent: a second run reports zero copies.
// Operators wiring this into pg_timetable need this to be cheap when
// no new backups have committed since the last run.
func TestRepoReplicate_Idempotent(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	plantBackupAtSrc(t, srcURL, "db1", "db1.full.idem", []byte("idem-payload"))

	if _, _, exit := runCLI(t,
		"repo", "replicate", "--from", srcURL, "--to", dstURL, "-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatal("first run failed")
	}

	stdout, _, exit := runCLI(t,
		"repo", "replicate", "--from", srcURL, "--to", dstURL, "-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("second run: exit=%d\n%s", exit, stdout)
	}
	// The dispatcher emits indented JSON; re-marshal the inner Result
	// compactly so the field-name + value assertions don't have to
	// account for whitespace.
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(env.Result)
	for _, want := range []string{
		`"manifests_copied":0`,
		`"manifests_skipped":1`,
		`"chunks_copied":0`,
		`"chunks_skipped":1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("second run not idempotent: missing %q\n%s", want, body)
		}
	}
}

// TestRepoReplicate_IncompleteExitsNonZero pins round-4 data-loss #2:
// when the destination ends up INCOMPLETE (here a manifest references a
// chunk that's absent at the source, so it can't be replicated), `repo
// replicate` must exit NON-ZERO so a scripted `replicate && rm source`
// can't silently trust a partial DR copy.
func TestRepoReplicate_IncompleteExitsNonZero(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	plantBackupAtSrc(t, srcURL, "db1", "db1.full.x", []byte("payload"))

	// Delete the referenced chunk at the source → the manifest now
	// points at a missing chunk; replication can't complete it.
	u, _ := url.Parse(srcURL)
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Delete(context.Background(), repo.ChunkKey(repo.HashOf([]byte("payload")))); err != nil {
		t.Fatal(err)
	}
	sp.Close()

	stdout, stderr, exit := runCLI(t,
		"repo", "replicate", "--from", srcURL, "--to", dstURL, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("incomplete replication must exit non-zero; got OK\nstdout=%s", stdout)
	}
	if !strings.Contains(stdout+stderr, "repo.replicate.incomplete") {
		t.Errorf("expected repo.replicate.incomplete:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
}
