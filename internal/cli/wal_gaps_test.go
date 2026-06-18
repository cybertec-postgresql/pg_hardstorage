package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// plantGapRecord drops a synthetic gapstate.Record at the
// canonical path. Used by the wal-gaps tests so we don't have
// to spin up a Coordinator just to populate the repo.
func plantGapRecord(t *testing.T, repoDir, deployment, body, filename string) {
	t.Helper()
	dir := filepath.Join(repoDir, "wal", deployment, "gaps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWalGaps_RequiresFlags: --repo is mandatory.
func TestWalGaps_RequiresFlags(t *testing.T) {
	_, stderr, exit := runCLI(t,
		"wal", "gaps", "db1",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse\n%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag; got\n%s", stderr)
	}
}

// TestWalGaps_NoRecords: when the deployment has no gap records,
// the JSON body shows total=0 and the text mode says "no WAL
// gaps". Validates the happy-path "everything's fine" surface.
func TestWalGaps_NoRecords(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatal("repo init failed")
	}

	stdout, _, exit := runCLI(t, "wal", "gaps", "db1",
		"--repo", repoURL,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	// The renderer compacts the body when it's small; assert
	// against the parsed envelope rather than a literal string
	// so the test is format-agnostic.
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	m, ok := res.Result.(map[string]any)
	if !ok {
		t.Fatalf("Result is %T, want map[string]any", res.Result)
	}
	if total, _ := m["total"].(float64); total != 0 {
		t.Errorf("total = %v, want 0", total)
	}
}

// TestWalGaps_ListsPlantedRecords: a synthetic record under
// wal/<deployment>/gaps/ becomes one entry in the records
// array. Validates the round-trip end-to-end through the CLI.
func TestWalGaps_ListsPlantedRecords(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatal("repo init failed")
	}

	plantGapRecord(t, repoDir, "db1", `{
  "schema": "pg_hardstorage.wal.gap.v1",
  "deployment": "db1",
  "slot_name": "pg_hardstorage_db1",
  "slot_role": "leader",
  "timeline": 7,
  "gap_start_lsn": "0/3000028",
  "gap_end_lsn": "0/30001A0",
  "gap_bytes": 420,
  "detected_at": "2026-04-30T12:00:00Z"
}`, "7-1234567890.json")

	stdout, _, exit := runCLI(t, "wal", "gaps", "db1",
		"--repo", repoURL,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"total": 1`,
		`"deployment": "db1"`,
		`"gap_bytes": 420`,
		`"slot_name": "pg_hardstorage_db1"`,
		`"timeline": 7`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in output:\n%s", want, stdout)
		}
	}
}

// TestWalGaps_TimelineFilter: --timeline N excludes records on
// other timelines.
func TestWalGaps_TimelineFilter(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatal("repo init failed")
	}

	plantGapRecord(t, repoDir, "db1", `{
  "schema": "pg_hardstorage.wal.gap.v1",
  "deployment": "db1", "slot_name": "s", "timeline": 3,
  "gap_start_lsn": "0/0", "gap_end_lsn": "0/100", "gap_bytes": 100,
  "detected_at": "2026-04-30T12:00:00Z"
}`, "3-1.json")
	plantGapRecord(t, repoDir, "db1", `{
  "schema": "pg_hardstorage.wal.gap.v1",
  "deployment": "db1", "slot_name": "s", "timeline": 5,
  "gap_start_lsn": "0/0", "gap_end_lsn": "0/200", "gap_bytes": 200,
  "detected_at": "2026-04-30T13:00:00Z"
}`, "5-1.json")

	stdout, _, exit := runCLI(t, "wal", "gaps", "db1",
		"--repo", repoURL,
		"--timeline", "5",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"total": 1`) {
		t.Errorf("expected total=1 (TLI 5 only); got\n%s", stdout)
	}
	if !strings.Contains(stdout, `"gap_bytes": 200`) {
		t.Errorf("expected the TLI-5 record; got\n%s", stdout)
	}
	if strings.Contains(stdout, `"gap_bytes": 100`) {
		t.Errorf("TLI-3 record should be filtered out; got\n%s", stdout)
	}
}

// TestWalGaps_LimitTruncates: --limit caps the RECORDS returned (newest-first)
// but `total` still reports the TRUE number of gaps — undercounting permanent
// WAL loss would let an operator under-react.
func TestWalGaps_LimitTruncates(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatal("repo init failed")
	}

	for i, ts := range []string{"2026-04-30T11:00:00Z", "2026-04-30T12:00:00Z", "2026-04-30T13:00:00Z"} {
		plantGapRecord(t, repoDir, "db1", `{
  "schema": "pg_hardstorage.wal.gap.v1",
  "deployment": "db1", "slot_name": "s", "timeline": 1,
  "gap_start_lsn": "0/0", "gap_end_lsn": "0/100", "gap_bytes": 100,
  "detected_at": "`+ts+`"
}`, "1-"+string(rune('1'+i))+".json")
	}

	stdout, _, exit := runCLI(t, "wal", "gaps", "db1",
		"--repo", repoURL,
		"--limit", "2",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	// total reflects the TRUE count (3), not the truncated 2.
	if !strings.Contains(stdout, `"total": 3`) {
		t.Errorf("total should be the true count 3, not the truncated count, after --limit 2; got\n%s", stdout)
	}
	// Exactly 2 records returned...
	if n := strings.Count(stdout, `"detected_at"`); n != 2 {
		t.Errorf("expected 2 records after --limit 2; got %d\n%s", n, stdout)
	}
	// ...and they're the NEWEST two (13:00 + 12:00 present, oldest 11:00 absent).
	if !strings.Contains(stdout, "2026-04-30T13:00:00Z") || strings.Contains(stdout, "2026-04-30T11:00:00Z") {
		t.Errorf("--limit must return the newest records; got\n%s", stdout)
	}
}

// TestWalGaps_HelpDiscoverable: under `wal --help`, the gaps
// subcommand must appear so an operator running
// `pg_hardstorage wal --help` finds it.
func TestWalGaps_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "wal", "--help")
	if !strings.Contains(stdout, "gaps") {
		t.Errorf("wal --help should list 'gaps':\n%s", stdout)
	}
}
