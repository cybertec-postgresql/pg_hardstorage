package cli_test

import (
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestTimetableEmit_RequiresRepo: --repo is mandatory.
func TestTimetableEmit_RequiresRepo(t *testing.T) {
	_, stderr, exit := runCLI(t, "timetable", "emit", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag: %s", stderr)
	}
}

// TestTimetableEmit_DefaultEmitsRepoScopedJobs: with --repo only
// (no --deployment), the repo-scoped jobs (gc, scrub, gameday
// report) are emitted; per-deployment jobs are skipped.
func TestTimetableEmit_DefaultEmitsRepoScopedJobs(t *testing.T) {
	stdout, _, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("emit: exit=%d\n%s", exit, stdout)
	}
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(env.Result)
	bs := string(body)
	// Repo-scoped jobs present.
	for _, want := range []string{
		"pg_hardstorage_repo_scrub_hourly",
		"pg_hardstorage_repo_gc_weekly",
		"pg_hardstorage_repo_scrub_quarterly_full",
		"pg_hardstorage_gameday_report_quarterly",
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("body missing job %q:\n%s", want, bs)
		}
	}
	// Per-deployment jobs skipped (no --deployment).
	for _, skipped := range []string{
		"wal_audit_hourly (needs --deployment)",
		"wal_prune_daily (needs --deployment)",
		"anomaly_check_daily (needs --deployment)",
	} {
		if !strings.Contains(bs, skipped) {
			t.Errorf("body missing skipped %q:\n%s", skipped, bs)
		}
	}
}

// TestTimetableEmit_WithDeployment_AllJobs: with --deployment set,
// every job is emitted.
func TestTimetableEmit_WithDeployment_AllJobs(t *testing.T) {
	stdout, _, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--deployment", "db1",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("emit: exit=%d\n%s", exit, stdout)
	}
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(env.Result)
	bs := string(body)
	for _, want := range []string{
		"pg_hardstorage_repo_scrub_hourly",
		"pg_hardstorage_wal_audit_hourly",
		"pg_hardstorage_anomaly_check_daily",
		"pg_hardstorage_wal_prune_daily",
		"pg_hardstorage_repo_gc_weekly",
		"pg_hardstorage_repo_scrub_quarterly_full",
		"pg_hardstorage_gameday_report_quarterly",
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("body missing job %q:\n%s", want, bs)
		}
	}
}

// TestTimetableEmit_SQLFraming_BeginCommit: the SQL output is
// wrapped in BEGIN; / COMMIT; — atomic apply.
func TestTimetableEmit_SQLFraming_BeginCommit(t *testing.T) {
	stdout, _, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--deployment", "db1",
		"-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("text emit: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, "BEGIN;") {
		t.Errorf("missing BEGIN; in output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "COMMIT;") {
		t.Errorf("missing COMMIT; in output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "timetable.add_job(") {
		t.Errorf("missing timetable.add_job calls:\n%s", stdout)
	}
}

// TestTimetableEmit_ArgsSubstitution: {{repo}} and {{deployment}}
// placeholders are correctly substituted in the JSONB job_parameters.
func TestTimetableEmit_ArgsSubstitution(t *testing.T) {
	stdout, _, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://acme/backups",
		"--deployment", "production",
		"-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("emit: exit=%d", exit)
	}
	// The wal-audit job should have the deployment + repo
	// substituted into its argv. The encoder is encoding/json's
	// canonical no-space-after-comma form (we switched away from
	// the hand-rolled jsonQuote in favour of json.Marshal, which
	// correctly handles control characters that the manual quoter
	// missed).
	if !strings.Contains(stdout, `"wal","audit","production","--repo","s3://acme/backups"`) {
		t.Errorf("wal audit argv substitution wrong:\n%s", stdout)
	}
	// The repo gc job should have the repo substituted.
	if !strings.Contains(stdout, `"repo","gc","s3://acme/backups","--apply"`) {
		t.Errorf("repo gc argv substitution wrong:\n%s", stdout)
	}
}

// TestTimetableEmit_IncludeFilter: --include narrows the job set.
func TestTimetableEmit_IncludeFilter(t *testing.T) {
	stdout, _, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--deployment", "db1",
		"--include", "wal_audit_hourly,anomaly_check_daily",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("emit: exit=%d", exit)
	}
	var env output.Result
	stdjson.Unmarshal([]byte(stdout), &env)
	body, _ := stdjson.Marshal(env.Result)
	bs := string(body)
	for _, want := range []string{
		"pg_hardstorage_wal_audit_hourly",
		"pg_hardstorage_anomaly_check_daily",
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("included job missing: %q\n%s", want, bs)
		}
	}
	for _, notWant := range []string{
		"pg_hardstorage_repo_gc_weekly",
		"pg_hardstorage_repo_scrub_hourly",
	} {
		if strings.Contains(bs, notWant) {
			t.Errorf("excluded job leaked through filter: %q\n%s", notWant, bs)
		}
	}
}

// TestTimetableEmit_ExcludeFilter: --exclude removes specific jobs.
func TestTimetableEmit_ExcludeFilter(t *testing.T) {
	stdout, _, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--deployment", "db1",
		"--exclude", "gameday_report_quarterly,repo_scrub_quarterly_full",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("emit: exit=%d", exit)
	}
	var env output.Result
	stdjson.Unmarshal([]byte(stdout), &env)
	body, _ := stdjson.Marshal(env.Result)
	bs := string(body)
	if strings.Contains(bs, "pg_hardstorage_gameday_report_quarterly") {
		t.Errorf("excluded job leaked through:\n%s", bs)
	}
	if strings.Contains(bs, "pg_hardstorage_repo_scrub_quarterly_full") {
		t.Errorf("excluded job leaked through:\n%s", bs)
	}
	// Non-excluded jobs still present.
	if !strings.Contains(bs, "pg_hardstorage_repo_gc_weekly") {
		t.Errorf("non-excluded job dropped:\n%s", bs)
	}
}

// TestTimetableEmit_CustomPrefix: --prefix overrides the default
// pg_hardstorage_ namespace.
func TestTimetableEmit_CustomPrefix(t *testing.T) {
	stdout, _, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--prefix", "acme_prod_",
		"-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("emit: exit=%d", exit)
	}
	if !strings.Contains(stdout, "'acme_prod_repo_scrub_hourly'") {
		t.Errorf("custom prefix not applied:\n%s", stdout)
	}
	if strings.Contains(stdout, "'pg_hardstorage_repo_scrub_hourly'") {
		t.Errorf("default prefix leaked through:\n%s", stdout)
	}
}

// TestTimetableEmit_CustomBinary: --binary overrides the program
// path (useful for operators with the binary in /opt/local/bin or
// running fips builds).
func TestTimetableEmit_CustomBinary(t *testing.T) {
	stdout, _, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--binary", "/opt/pg_hardstorage_fips",
		"-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("emit: exit=%d", exit)
	}
	if !strings.Contains(stdout, `job_command       => '/opt/pg_hardstorage_fips'`) {
		t.Errorf("custom binary not applied:\n%s", stdout)
	}
}

// TestTimetableEmit_SchemaStable: the JSON body carries the v1
// schema string.
func TestTimetableEmit_SchemaStable(t *testing.T) {
	stdout, _, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--deployment", "db1",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"schema": "pg_hardstorage.timetable.emit.v1"`) {
		t.Errorf("schema field missing:\n%s", stdout)
	}
}

// TestTimetableEmit_ApplyRequiresPGConnection: --apply alone is
// a usage error.
func TestTimetableEmit_ApplyRequiresPGConnection(t *testing.T) {
	_, stderr, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--apply",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("--apply alone should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "--pg-connection") {
		t.Errorf("error should name --pg-connection: %s", stderr)
	}
}

// TestTimetableEmit_PGConnectionWithoutApply: a stray
// --pg-connection without --apply is a usage error (helps catch
// the typo case where the operator forgot --apply).
func TestTimetableEmit_PGConnectionWithoutApply(t *testing.T) {
	_, stderr, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--pg-connection", "postgres://x@/y",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("--pg-connection without --apply should exit Misuse; got %d\nstderr=%s",
			exit, stderr)
	}
	if !strings.Contains(stderr, "only meaningful with --apply") {
		t.Errorf("error should explain why: %s", stderr)
	}
}

// TestTimetableEmit_ApplyToUnreachableDB: --apply against a
// definitely-unreachable DSN surfaces a structured connect-time
// error. We use a port nothing's listening on plus a tight
// connect_timeout to keep the test fast.
func TestTimetableEmit_ApplyToUnreachableDB(t *testing.T) {
	// 127.0.0.1:1 is reserved + nothing should be there.
	dsn := "postgres://nobody@127.0.0.1:1/postgres?connect_timeout=2"
	_, stderr, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--deployment", "db1",
		"--apply",
		"--pg-connection", dsn,
		"-o", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("apply against unreachable should not exit OK; stderr=%s", stderr)
	}
	// pg.Connect maps connection-failure to a structured error;
	// we just want to confirm we got a non-OK exit + something
	// resembling a connect failure in stderr.
	if !strings.Contains(stderr, `"error"`) {
		t.Errorf("expected structured error envelope in stderr:\n%s", stderr)
	}
}

// TestTimetableEmit_ApplyBadDSN: a malformed DSN surfaces as
// usage.bad_pg_dsn (the same error pg.Connect emits for parse
// failures).
func TestTimetableEmit_ApplyBadDSN(t *testing.T) {
	_, stderr, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--apply",
		"--pg-connection", "::not a valid dsn::",
		"-o", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("bad DSN should not exit OK; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "usage.bad_pg_dsn") {
		t.Errorf("expected usage.bad_pg_dsn:\n%s", stderr)
	}
}

// TestTimetableEmit_SanitisePGDSN_RedactsPassword: an internal
// helper test — the result body's pg_connection field must not
// leak the password. We exercise this indirectly via the
// unreachable-DB test (the body never lands because of the
// connect failure), so cover sanitisePGDSN's behaviour with a
// direct unit-test-ish check via the result body of a hypothetical
// success path. Since we can't easily reach success without a
// real pg_timetable, we just verify the sanitiser doesn't echo
// the password back in stderr on the connect-failure path.
func TestTimetableEmit_PasswordNotEchoedOnFailure(t *testing.T) {
	dsn := "postgres://user:s3cret_p4ssw0rd@127.0.0.1:1/postgres?connect_timeout=2"
	_, stderr, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--deployment", "db1",
		"--apply",
		"--pg-connection", dsn,
		"-o", "json")
	if exit == int(output.ExitOK) {
		t.Skip("apply unexpectedly succeeded; skipping password-leak check")
	}
	if strings.Contains(stderr, "s3cret_p4ssw0rd") {
		t.Errorf("password leaked in error output:\n%s", stderr)
	}
}

// TestTimetableEmit_SQLValidatesRoughly: the SQL has well-formed
// timetable.add_job(...) blocks with the expected required fields
// (job_name, job_schedule, job_command, job_parameters, job_kind).
// We don't run psql against pg_timetable here — that's an
// integration concern — but we sanity-check the SQL shape.
func TestTimetableEmit_SQLValidatesRoughly(t *testing.T) {
	stdout, _, exit := runCLI(t,
		"timetable", "emit",
		"--repo", "s3://example/repo",
		"--deployment", "db1",
		"-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	for _, want := range []string{
		"job_name          =>",
		"job_schedule      =>",
		"job_command       =>",
		"job_parameters    =>",
		"job_kind          => 'PROGRAM'::timetable.command_kind",
		"::jsonb",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("SQL missing %q:\n%s", want, stdout)
		}
	}
}
