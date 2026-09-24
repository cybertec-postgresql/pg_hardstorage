package tools

import (
	"context"
	"strings"
	"testing"
)

// The live-state tools build the child pg_hardstorage argv from
// MODEL-SUPPLIED values.  Root persistent flags (--cpu-profile,
// --mem-profile, --config, --otel-endpoint, --profile-port) are parsed
// from anywhere in argv, so an unvalidated value like
// "--cpu-profile=/home/op/.bashrc" smuggled in as a deployment name made
// the child os.Create (truncate) an arbitrary file.  Every tool must
// refuse such values before the runner is ever invoked.

// injectionValues are argv-smuggling payloads a hostile or confused
// model can put into any string argument.
var injectionValues = []string{
	"--cpu-profile=/home/op/.bashrc",
	"--mem-profile=/tmp/x",
	"--config=/tmp/evil.yaml",
	"-c/tmp/evil.yaml",
	"--otel-endpoint=http://attacker:4318",
	"--profile-port=6060",
	"--force",
	"-",
}

func TestLiveStateTools_RejectFlagSmuggling(t *testing.T) {
	withStubRepoResolver(t, nil)
	type tc struct {
		tool  string
		field string
		base  map[string]any
	}
	cases := []tc{
		{"read_doctor", "deployment", map[string]any{}},
		{"read_status", "deployment", map[string]any{}},
		{"read_status", "repo", map[string]any{"deployment": "db1"}},
		{"list_backups", "deployment", map[string]any{}},
		{"list_backups", "repo", map[string]any{"deployment": "db1"}},
		{"read_backup", "deployment", map[string]any{"backup_id": "latest"}},
		{"read_backup", "backup_id", map[string]any{"deployment": "db1"}},
		{"read_backup", "repo", map[string]any{"deployment": "db1", "backup_id": "latest"}},
		{"read_repo_usage", "repo", map[string]any{}},
		{"read_audit", "repo", map[string]any{}},
		{"read_audit", "action", map[string]any{"repo": "s3://acme/"}},
		{"read_audit", "deployment", map[string]any{"repo": "s3://acme/"}},
		{"read_audit", "since", map[string]any{"repo": "s3://acme/"}},
	}
	for _, c := range cases {
		for _, v := range injectionValues {
			captured := &stubRunner{stdout: map[string][]byte{}, exitCode: map[string]int{}}
			reg := NewRegistry()
			RegisterCoreTools(reg, &CLIRunner{Path: "/x", Runner: captured.run})
			tool, err := reg.Get(c.tool)
			if err != nil {
				t.Fatal(err)
			}
			args := map[string]any{}
			for k, bv := range c.base {
				args[k] = bv
			}
			args[c.field] = v
			_, err = tool.Run(context.Background(), args)
			if err == nil {
				t.Errorf("%s{%s:%q}: expected refusal, got success", c.tool, c.field, v)
			}
			if len(captured.calls) != 0 {
				t.Errorf("%s{%s:%q}: runner was invoked with %v — the value reached the child argv", c.tool, c.field, v, captured.calls)
			}
		}
	}
}

func TestReadAudit_RejectsNegativeLimit(t *testing.T) {
	captured := &stubRunner{stdout: map[string][]byte{}, exitCode: map[string]int{}}
	tool := &readAudit{runner: &CLIRunner{Path: "/x", Runner: captured.run}}
	if _, err := tool.Run(context.Background(), map[string]any{"repo": "s3://acme/", "limit": float64(-5)}); err == nil {
		t.Error("negative limit: expected refusal")
	}
	if len(captured.calls) != 0 {
		t.Errorf("runner invoked: %v", captured.calls)
	}
}

// Positionals go after a `--` terminator so that, even if a value slipped
// past validation, cobra could never parse it as a flag.  Flags (including
// the appended `-o json`) must stay BEFORE the terminator or they would be
// read as positionals.
func TestLiveStateTools_PositionalsAfterTerminator(t *testing.T) {
	withStubRepoResolver(t, nil)
	captured := &stubRunner{stdout: map[string][]byte{
		"doctor": []byte(`{}`), "status": []byte(`{}`), "list": []byte(`{}`),
		"show": []byte(`{}`), "repo": []byte(`{}`),
	}, exitCode: map[string]int{}}
	reg := NewRegistry()
	RegisterCoreTools(reg, &CLIRunner{Path: "/x", Runner: captured.run})
	calls := []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"read_doctor", map[string]any{"deployment": "db1"}, []string{"doctor", "-o", "json", "--", "db1"}},
		{"read_status", map[string]any{"deployment": "db1", "repo": "s3://a/"}, []string{"status", "--repo", "s3://a/", "-o", "json", "--", "db1"}},
		{"list_backups", map[string]any{"deployment": "db1", "repo": "s3://a/"}, []string{"list", "--repo", "s3://a/", "-o", "json", "--", "db1"}},
		{"read_backup", map[string]any{"deployment": "db1", "backup_id": "db1.full.20260101T000000Z.ab12", "repo": "s3://a/"}, []string{"show", "--repo", "s3://a/", "-o", "json", "--", "db1", "db1.full.20260101T000000Z.ab12"}},
		{"read_repo_usage", map[string]any{"repo": "file:///srv/repo"}, []string{"repo", "usage", "-o", "json", "--", "file:///srv/repo"}},
	}
	for _, c := range calls {
		captured.calls = nil
		tool, _ := reg.Get(c.tool)
		if _, err := tool.Run(context.Background(), c.args); err != nil {
			t.Fatalf("%s: %v", c.tool, err)
		}
		if !equalStrings(captured.calls[0], c.want) {
			t.Errorf("%s: argv = %v, want %v", c.tool, captured.calls[0], c.want)
		}
	}
}

// assertReadOnlyArgs is the last line of defence for every invocation
// (live-state tools AND execute_command).  It must refuse the root
// persistent flags that have side effects outside the command's own
// read path, in every spelling pflag accepts.
func TestRunJSON_RefusesSideEffectGlobalFlags(t *testing.T) {
	r := &CLIRunner{Path: "/x", Runner: func(_ context.Context, _ []string) ([]byte, []byte, int, error) {
		return nil, nil, 0, nil
	}}
	cases := [][]string{
		{"doctor", "--cpu-profile=/home/op/.bashrc"},
		{"doctor", "--cpu-profile", "/home/op/.bashrc"},
		{"doctor", "--mem-profile=/x"},
		{"doctor", "--profile-port=6060"},
		{"doctor", "--otel-endpoint", "http://x:4318"},
		{"doctor", "--otel-stdout"},
		{"doctor", "--config=/tmp/c.yaml"},
		{"doctor", "-c", "/tmp/c.yaml"},
		{"doctor", "-c/tmp/c.yaml"},
		{"doctor", "-qc", "/tmp/c.yaml"},
		{"doctor", "--on-error-llm"},
	}
	for _, args := range cases {
		if _, err := r.RunJSON(context.Background(), args...); err == nil {
			t.Errorf("args %v: expected refusal", args)
		} else if !strings.Contains(err.Error(), "refuse") {
			t.Errorf("args %v: unexpected error %v", args, err)
		}
	}
	// Ordinary read flags and shorthands must still pass.
	for _, args := range [][]string{
		{"status", "-q"},
		{"status", "--no-color"},
		{"doctor", "-o", "yaml"},
	} {
		if _, err := r.RunJSON(context.Background(), args...); err != nil {
			t.Errorf("args %v: unexpected refusal: %v", args, err)
		}
	}
}
