package validate_test

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

func TestCorruptIndexName(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "torn index metapage",
			err:  &pgconn.PgError{Code: "XX002", Message: `index "orders_pkey" is not a btree`},
			want: "orders_pkey",
		},
		{
			name: "wrapped torn index metapage",
			err: errors.Join(errors.New("driving load"),
				&pgconn.PgError{Code: "XX002", Message: `index "customers_pkey" is not a btree`}),
			want: "customers_pkey",
		},
		{
			name: "XX002 but not the not-a-btree shape (e.g. torn heap page)",
			err:  &pgconn.PgError{Code: "XX002", Message: "could not read block 7 in file"},
			want: "",
		},
		{
			name: "different sqlstate",
			err:  &pgconn.PgError{Code: "23505", Message: `duplicate key value violates unique constraint "orders_pkey"`},
			want: "",
		},
		{
			name: "plain error",
			err:  errors.New("connection refused"),
			want: "",
		},
		{
			name: "nil",
			err:  nil,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validate.CorruptIndexNameForTest(tc.err); got != tc.want {
				t.Errorf("CorruptIndexNameForTest() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewDockerCellRuntime_DefaultsApplied(t *testing.T) {
	entry := config.FleetEntry{
		Name: "u24-pg17", OS: "ubuntu:24.04", PG: "17", Count: 1,
	}
	profile := config.Profile{Name: "small_oltp", TargetSizeGB: 10, Schema: "tpcc-lite"}
	r, err := validate.NewDockerCellRuntime(entry, "myproj", "u24-pg17", 15432, profile, nil, 42)
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "u24-pg17" {
		t.Errorf("name: %s", r.Name())
	}
	// Container must be project-prefixed so docker exec / kill /
	// cp targets the same name compose generated as
	// container_name (avoiding cross-run name conflicts).
	if r.Container != "myproj-u24-pg17" {
		t.Errorf("container: got %q; want %q", r.Container, "myproj-u24-pg17")
	}
	if r.HostPort != 15432 {
		t.Errorf("port: %d", r.HostPort)
	}
	if r.AgentBinary == "" {
		t.Errorf("AgentBinary default not applied")
	}
	if r.RepoURL == "" {
		t.Errorf("RepoURL default not applied")
	}
	if r.PGUser != "postgres" {
		t.Errorf("PGUser default: %s", r.PGUser)
	}
	if r.Schema == nil || r.Schema.Name() != "tpcc-lite" {
		t.Errorf("Schema not wired: %v", r.Schema)
	}
}

func TestNewDockerCellRuntime_EmptyProjectKeepsLegacyName(t *testing.T) {
	// Empty project => container name unchanged.  Standalone
	// tests that bypass compose still work without a project.
	entry := config.FleetEntry{Name: "x", OS: "ubuntu:24.04", PG: "17", Count: 1}
	profile := config.Profile{Name: "p", Schema: "tpcc-lite"}
	r, err := validate.NewDockerCellRuntime(entry, "", "lead", 15432, profile, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Container != "lead" {
		t.Errorf("empty project should leave container unprefixed; got %q", r.Container)
	}
}

func TestNewDockerCellRuntime_DefaultSchema(t *testing.T) {
	// Empty Profile.Schema → tpcc-lite (default).
	entry := config.FleetEntry{Name: "x", OS: "ubuntu:24.04", PG: "17", Count: 1}
	profile := config.Profile{}
	r, err := validate.NewDockerCellRuntime(entry, "p", "x", 15432, profile, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Schema.Name() != "tpcc-lite" {
		t.Errorf("expected tpcc-lite default; got %s", r.Schema.Name())
	}
}

func TestNewDockerCellRuntime_BadSchema(t *testing.T) {
	entry := config.FleetEntry{Name: "x", OS: "ubuntu:24.04", PG: "17", Count: 1}
	profile := config.Profile{Schema: "imaginary"}
	if _, err := validate.NewDockerCellRuntime(entry, "p", "x", 15432, profile, nil, 1); err == nil {
		t.Errorf("expected error for unknown schema")
	}
}

// We can't unit-test the docker / pgx paths without a runtime,
// but we CAN exercise the extractField helper through the
// thin export below.
func TestExtractField_Roundtrip(t *testing.T) {
	json := `{"result":{"backup_id":"db1.full.20260505T120000Z","verified":true}}`
	id := validate.ExtractFieldForTest(json, `"backup_id":"`, `"`)
	if id != "db1.full.20260505T120000Z" {
		t.Errorf("got %q", id)
	}
}

func TestExtractField_Missing(t *testing.T) {
	if got := validate.ExtractFieldForTest("no match here", `"backup_id":"`, `"`); got != "" {
		t.Errorf("expected empty string for missing prefix; got %q", got)
	}
}

// TestIsRepoAlreadyExists locks the three signals the
// idempotency check is allowed to rely on (exit code 7,
// structured `conflict.repo_exists` code, the human "already
// exists" substring).  The first attempt of this fix matched
// only `"code":"conflict.repo_exists"` which broke when the
// agent emitted `"code": "conflict.repo_exists"` (space
// after colon) — the soak then failed every cell that wasn't
// the race winner.
func TestIsRepoAlreadyExists(t *testing.T) {
	t.Run("exit_code_7_is_conflict", func(t *testing.T) {
		// `false` exits 1, but we want exit 7 specifically.
		// `sh -c 'exit 7'` gives us a real *exec.ExitError
		// with ExitCode()==7 — same shape ensureRepoInit
		// receives in production.
		cmd := exec.Command("sh", "-c", "exit 7")
		err := cmd.Run()
		if !validate.IsRepoAlreadyExistsForTest(err, nil) {
			t.Errorf("exit code 7 should be treated as conflict; got false")
		}
	})
	t.Run("structured_code_no_space", func(t *testing.T) {
		body := []byte(`{"error":{"code":"conflict.repo_exists","message":"x"}}`)
		if !validate.IsRepoAlreadyExistsForTest(errors.New("any err"), body) {
			t.Errorf("structured code (no space) should match")
		}
	})
	t.Run("structured_code_with_space", func(t *testing.T) {
		body := []byte(`{"error": {"code": "conflict.repo_exists", "message": "x"}}`)
		if !validate.IsRepoAlreadyExistsForTest(errors.New("any err"), body) {
			t.Errorf("structured code (space after colons) should match — this was the soak-bug")
		}
	})
	t.Run("human_message_fallback", func(t *testing.T) {
		body := []byte(`error: a repository already exists at file:///x`)
		if !validate.IsRepoAlreadyExistsForTest(errors.New("any err"), body) {
			t.Errorf("human-message fallback should match")
		}
	})
	t.Run("unrelated_error_passes_through", func(t *testing.T) {
		body := []byte(`{"error":{"code":"internal","message":"disk full"}}`)
		if validate.IsRepoAlreadyExistsForTest(errors.New("exit status 1"), body) {
			t.Errorf("unrelated error should NOT be treated as conflict")
		}
	})
}

func TestExtractField_NoTerminator(t *testing.T) {
	// Trailing prefix without closing quote returns "" — the
	// caller's TakeBackup falls back to a synthesised ID.
	if got := validate.ExtractFieldForTest(
		`"backup_id":"unterminated`, `"backup_id":"`, `"`); got != "" {
		t.Errorf("expected empty string when terminator missing; got %q", got)
	}
}
