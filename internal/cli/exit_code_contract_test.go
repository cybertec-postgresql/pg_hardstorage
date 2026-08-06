package cli_test

// exit_code_contract_test.go — the exit codes an operator scripts
// against, asserted through the real Run path.
//
// internal/output already pins the documented namespace→exit table
// against ExitCodeFor, and root_test.go covers ExitOK and ExitMisuse.
// What neither does is drive a real command to a real failure and read
// the number the process would return. That gap matters because the
// mapping is only half the path: a command that swallows its structured
// error, or wraps it so AsOutputError no longer finds it, exits 1 while
// every unit test on the mapping stays green.
//
// This is the same contract that had `storage.no_space` documented as
// exit 8 (it exits 1) and `usage.no_pg_verifybackup` documented as exit
// 2 (it is verify.missing_tool, exit 9). Operators write
// `if [ $? -eq N ]` straight off those pages.
//
// Scope is deliberately what a test can provoke with no PostgreSQL, no
// network and no containers. The codes that need those are listed by
// TestExitCodeContract_CoverageIsVisible rather than quietly omitted —
// an uncovered contract row should be a thing you can see, not a thing
// you have to notice.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// exitCase is one end-to-end assertion: run these args, get this code.
type exitCase struct {
	name string
	want output.ExitCode
	args func(dir string) []string
}

var exitCases = []exitCase{
	{
		name: "success",
		want: output.ExitOK,
		args: func(string) []string { return []string{"version"} },
	},
	{
		name: "unknown flag is misuse",
		want: output.ExitMisuse,
		args: func(string) []string { return []string{"backup", "--definitely-not-a-flag"} },
	},
	{
		name: "listing a repository that does not exist",
		want: output.ExitNotFound,
		args: func(dir string) []string {
			return []string{"list", "db1", "--repo", "file://" + filepath.Join(dir, "absent")}
		},
	},
	{
		name: "initialising a repository twice",
		want: output.ExitConflict,
		args: func(dir string) []string {
			return []string{"repo", "init", "--repo", "file://" + filepath.Join(dir, "twice")}
		},
	},
}

func TestExitCodeContract_EndToEnd(t *testing.T) {
	dir := t.TempDir()

	// The conflict case needs the repo to exist first; its own first
	// run is the setup and must succeed, which is itself worth
	// asserting — a repo init that fails would make the conflict case
	// pass for the wrong reason.
	if _, _, exit := runCmd(t, "repo", "init", "--repo",
		"file://"+filepath.Join(dir, "twice")); exit != int(output.ExitOK) {
		t.Fatalf("setup: first `repo init` exited %d, want 0 — the conflict case below "+
			"would then be asserting the wrong failure", exit)
	}

	for _, tc := range exitCases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, exit := runCmd(t, tc.args(dir)...)
			if exit != int(tc.want) {
				t.Errorf("exit = %d, want %d (%s).\nstderr: %s\n\n"+
					"This is the number an operator scripts against. The unit "+
					"mapping in internal/output can be perfectly correct and this still be "+
					"wrong, if the command loses its structured error on the way out.",
					exit, int(tc.want), tc.name, stderr)
			}
		})
	}
}

// TestExitCodeContract_CoverageIsVisible reports which documented exit
// codes this file does NOT exercise end-to-end.
//
// It does not fail on a gap — several codes genuinely need PostgreSQL,
// a KMS or an unreachable backend, and a test that failed for that
// would just get an exemption list bolted on. It fails only if the
// covered set SHRINKS below what is achievable offline, which is the
// regression that would otherwise pass unnoticed.
func TestExitCodeContract_CoverageIsVisible(t *testing.T) {
	covered := map[output.ExitCode]bool{}
	for _, tc := range exitCases {
		covered[tc.want] = true
	}

	all := map[output.ExitCode]string{
		output.ExitOK:           "ExitOK",
		output.ExitError:        "ExitError",
		output.ExitMisuse:       "ExitMisuse",
		output.ExitAuth:         "ExitAuth",
		output.ExitPreflight:    "ExitPreflight",
		output.ExitAborted:      "ExitAborted",
		output.ExitNotFound:     "ExitNotFound",
		output.ExitConflict:     "ExitConflict",
		output.ExitUnreachable:  "ExitUnreachable",
		output.ExitVerifyFailed: "ExitVerifyFailed",
		output.ExitDoctorIssues: "ExitDoctorIssues",
	}

	var uncovered []string
	for code, name := range all {
		if !covered[code] {
			uncovered = append(uncovered, name)
		}
	}
	t.Logf("end-to-end exit-code coverage: %d of %d documented codes; not covered here: %v "+
		"(ExitDoctorIssues is covered by doctor_exit_on_issues_test.go; the rest need "+
		"PostgreSQL, a KMS, or an unreachable backend)",
		len(covered), len(all), uncovered)

	// The four below are reachable with nothing but a temp directory.
	// If one stops being covered, the contract lost end-to-end proof
	// for a code that costs nothing to prove.
	for _, must := range []output.ExitCode{
		output.ExitOK, output.ExitMisuse, output.ExitNotFound, output.ExitConflict,
	} {
		if !covered[must] {
			t.Errorf("exit code %d (%s) is reachable offline but no longer has an "+
				"end-to-end case", int(must), all[must])
		}
	}
}

// TestExitCodeContract_TempDirIsUsable guards the fixture rather than
// the product: every case above depends on writing into t.TempDir(),
// and a read-only or missing TMPDIR would make the notfound and
// conflict cases fail in ways that read like product bugs.
func TestExitCodeContract_TempDirIsUsable(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		t.Fatalf("cannot write into t.TempDir() (%s): %v — the exit-code cases would fail "+
			"for fixture reasons and look like product defects", dir, err)
	}
}
