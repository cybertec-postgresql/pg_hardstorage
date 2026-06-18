package cli_test

import (
	"context"
	stdjson "encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/anomaly"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// commitSizedManifest plants a real (signed) manifest with controlled
// LogicalBytes / FileCount / duration values. Reuses the readWorld
// keystore so the CLI's loadVerifier accepts what we wrote.
//
// The metric arithmetic is intentionally simple: one File with Size
// = logicalBytes and a single chunk of length 1 (so file_count = 1
// and unique_chunk_count = 1 are stable; logical_bytes is the
// variable). For tests that need to vary file_count or chunk_count,
// pass fileCount > 1 — we'll mint that many files each with size
// `logicalBytes/fileCount` (rounded down).
func (w *readWorld) commitSizedManifest(t *testing.T, deployment, suffix string, stoppedAt time.Time, durationSec int, logicalBytes int64, fileCount int) {
	t.Helper()
	if fileCount < 1 {
		fileCount = 1
	}
	files := make([]backup.FileEntry, 0, fileCount)
	per := logicalBytes / int64(fileCount)
	for i := 0; i < fileCount; i++ {
		// Make each file's chunk hash unique so unique_chunk_count
		// scales with fileCount. Synthetic content drives the hash.
		chunkBody := []byte(suffix + ":" + itoaTest(i))
		files = append(files, backup.FileEntry{
			Path: "data/" + itoaTest(i),
			Size: per,
			Mode: 0o600,
			Chunks: []backup.ChunkRef{{
				Hash: repo.HashOf(chunkBody), Offset: 0, Len: per,
			}},
		})
	}
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         deployment + ".full." + suffix,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        stoppedAt.Add(-time.Duration(durationSec) * time.Second),
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:            files,
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit sized %s/%s: %v", deployment, suffix, err)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestAnomaly_RequiresRepo: the obvious missing-flag case.
func TestAnomaly_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, stderr, exit := runCLI(t, "anomaly", "check", "db1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag: %s", stderr)
	}
}

// TestAnomaly_EmptyDeployment: a deployment with no backups returns
// a clean Result with TotalBackups=0 — not an error. Cron-driven
// anomaly checks against pre-bootstrap deployments shouldn't alarm.
func TestAnomaly_EmptyDeployment(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t,
		"anomaly", "check", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	var body struct {
		TotalBackups int `json:"total_backups"`
		FlaggedCount int `json:"flagged_count"`
	}
	bodyOf(t, stdout, &body)
	if body.TotalBackups != 0 || body.FlaggedCount != 0 {
		t.Errorf("empty deployment: %+v", body)
	}
}

// TestAnomaly_BaselineWarmup: with only 1-2 priors plus the
// candidate, MinSamples is unmet — the report has Skipped non-empty
// and the run exits 0 (no flag). This is the cold-start day-1
// behaviour every operator hits.
func TestAnomaly_BaselineWarmup(t *testing.T) {
	w := newReadWorld(t)
	t0 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		w.commitSizedManifest(t, "db1", itoaTest(i), t0.Add(time.Duration(i)*time.Hour),
			60, 1_000_000, 5)
	}
	stdout, _, exit := runCLI(t,
		"anomaly", "check", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("warmup should exit 0; got %d\n%s", exit, stdout)
	}
	var body struct {
		TotalBackups int `json:"total_backups"`
		FlaggedCount int `json:"flagged_count"`
		Reports      []struct {
			Skipped string `json:"skipped"`
		} `json:"reports"`
	}
	bodyOf(t, stdout, &body)
	if body.TotalBackups != 2 {
		t.Errorf("TotalBackups=%d, want 2", body.TotalBackups)
	}
	if body.FlaggedCount != 0 {
		t.Errorf("warmup should not flag; got %d", body.FlaggedCount)
	}
	if len(body.Reports) != 1 || body.Reports[0].Skipped == "" {
		t.Errorf("expected one report with Skipped non-empty: %+v", body.Reports)
	}
}

// TestAnomaly_StableBaselineNoFlag: 5 identical-sized backups +
// candidate of the same size — no flag.
func TestAnomaly_StableBaselineNoFlag(t *testing.T) {
	w := newReadWorld(t)
	t0 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		w.commitSizedManifest(t, "db1", itoaTest(i), t0.Add(time.Duration(i)*time.Hour),
			60, 1_000_000, 5)
	}
	stdout, _, exit := runCLI(t,
		"anomaly", "check", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("stable should exit 0; got %d\n%s", exit, stdout)
	}
	var body struct {
		FlaggedCount int `json:"flagged_count"`
	}
	bodyOf(t, stdout, &body)
	if body.FlaggedCount != 0 {
		t.Errorf("stable baseline should not flag; got %d", body.FlaggedCount)
	}
}

// TestAnomaly_OutlierFlagsAndExitsVerifyFailed: 5 identical-sized
// priors then a 100x larger candidate flips the exit to
// ExitVerifyFailed (9). Audit chain captures the finding.
func TestAnomaly_OutlierFlagsAndExitsVerifyFailed(t *testing.T) {
	w := newReadWorld(t)
	t0 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		w.commitSizedManifest(t, "db1", itoaTest(i), t0.Add(time.Duration(i)*time.Hour),
			60, 1_000_000, 5)
	}
	// Outlier — 100x logical bytes.
	w.commitSizedManifest(t, "db1", "outlier", t0.Add(10*time.Hour),
		60, 100_000_000, 5)

	stdout, stderr, exit := runCLI(t,
		"anomaly", "check", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("outlier should exit ExitVerifyFailed (9); got %d\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	// The error Result envelope is rendered through stderr so a
	// success-path body on stdout never gets mixed with a failure
	// envelope. Cron tooling routes on the "anomaly.detected" code.
	if !strings.Contains(stderr, `"code": "anomaly.detected"`) {
		t.Errorf("expected anomaly.detected code in error result:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	// And the per-backup detail surfaces in the message so a
	// human reading the cron mail sees what flagged.
	if !strings.Contains(stderr, "db1.full.outlier") {
		t.Errorf("expected outlier backup ID in error message:\n%s", stderr)
	}
}

// TestAnomaly_AllScansEveryBackup: --all walks the full chain. With
// 5 stable priors + 1 outlier, --all reports 6 scored (or 6 with
// some skips for the early ones below MinSamples), exactly one of
// which is flagged. Exit code stays ExitVerifyFailed.
func TestAnomaly_AllScansEveryBackup(t *testing.T) {
	w := newReadWorld(t)
	t0 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		w.commitSizedManifest(t, "db1", itoaTest(i), t0.Add(time.Duration(i)*time.Hour),
			60, 1_000_000, 5)
	}
	w.commitSizedManifest(t, "db1", "outlier", t0.Add(10*time.Hour),
		60, 100_000_000, 5)
	stdout, _, exit := runCLI(t,
		"anomaly", "check", "db1", "--repo", w.repoURL, "--all", "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("--all with outlier should still flip exit to ExitVerifyFailed; got %d\n%s",
			exit, stdout)
	}
}

// TestAnomaly_TextRender confirms the operator-friendly text body
// surfaces the warming-up message when there's not enough history
// (the most common cron-driven view in early life).
func TestAnomaly_TextRender(t *testing.T) {
	w := newReadWorld(t)
	t0 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1; i++ {
		w.commitSizedManifest(t, "db1", itoaTest(i), t0.Add(time.Duration(i)*time.Hour),
			60, 1_000_000, 5)
	}
	stdout, _, exit := runCLI(t,
		"anomaly", "check", "db1", "--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"anomaly check — deployment db1",
		"Threshold:",
		"Window:",
		"Total backups:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text render missing %q:\n%s", want, stdout)
		}
	}
}

// TestAnomaly_BadFlags: --threshold and --min-samples reject
// nonsense values at parse time so a typo doesn't silently produce
// a useless run.
func TestAnomaly_BadFlags(t *testing.T) {
	w := newReadWorld(t)
	for _, args := range [][]string{
		{"anomaly", "check", "db1", "--repo", w.repoURL, "--threshold", "0", "-o", "json"},
		{"anomaly", "check", "db1", "--repo", w.repoURL, "--threshold", "-1", "-o", "json"},
		{"anomaly", "check", "db1", "--repo", w.repoURL, "--min-samples", "1", "-o", "json"},
	} {
		_, stderr, exit := runCLI(t, args...)
		if exit != int(output.ExitMisuse) {
			t.Errorf("args=%v: exit=%d want ExitMisuse\nstderr=%s", args, exit, stderr)
		}
	}
}

// TestAnomaly_SchemaStable: the JSON body carries the v1 schema
// string (cron tooling depends on the schema field for forward
// compat).
func TestAnomaly_SchemaStable(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t,
		"anomaly", "check", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	// Empty deployment carries no Reports[] but the body still has the
	// shape; the per-Report Schema constant is verified separately.
	if anomaly.Schema != "pg_hardstorage.anomaly.v1" {
		t.Errorf("schema constant drifted: %s", anomaly.Schema)
	}
	// Sanity: the Result envelope decodes cleanly.
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, stdout)
	}
}
