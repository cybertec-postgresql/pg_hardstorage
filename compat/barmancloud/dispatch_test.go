package barmancloud

import (
	"reflect"
	"strings"
	"testing"
)

// runWithStubbedDispatch swaps dispatchNative for a recorder
// + sets env via the envLookup hook.  Returns the captured
// argv from the LAST dispatch call.  Mirrors the walg shim's
// test fixture.
func runWithStubbedDispatch(t *testing.T,
	env map[string]string,
	stub func(args []string) dispatchResult,
	verb func(argv []string) int,
	argv []string,
) (lastArgs []string, exitCode int) {
	t.Helper()

	prevEnv := envLookup
	envLookup = func(k string) string { return env[k] }
	t.Cleanup(func() { envLookup = prevEnv })

	prev := dispatchNative
	dispatchNative = func(args []string) dispatchResult {
		lastArgs = append([]string(nil), args...)
		if stub != nil {
			return stub(args)
		}
		return dispatchResult{ExitCode: 0}
	}
	t.Cleanup(func() { dispatchNative = prev })

	exitCode = verb(argv)
	return
}

func TestWalArchive_DispatchesNativeWalPush(t *testing.T) {
	got, rc := runWithStubbedDispatch(t,
		map[string]string{
			"PGDATA":                  "/var/lib/postgresql/data/pgdata",
			"AWS_REGION":              "us-east-1",
			"AWS_S3_FORCE_PATH_STYLE": "true",
		},
		nil,
		ExecuteWalArchive,
		[]string{
			"--gzip",
			"--endpoint-url", "http://minio:9000",
			"--cloud-provider", "aws-s3",
			"s3://bucket/prefix",
			"discovery",
			"pg_wal/000000010000000000000006",
		},
	)
	if rc != 0 {
		t.Fatalf("exit %d", rc)
	}
	wantHead := []string{"wal", "push", "discovery", "/var/lib/postgresql/data/pgdata/pg_wal/000000010000000000000006"}
	for i, w := range wantHead {
		if got[i] != w {
			t.Errorf("argv[%d] = %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
	// The synthesised --repo URL must include the operator's
	// endpoint + path_style + region — that's the only way
	// the native S3 plugin reaches MinIO.
	repoURLIdx := -1
	for i, a := range got {
		if a == "--repo" {
			repoURLIdx = i + 1
			break
		}
	}
	if repoURLIdx < 0 {
		t.Fatalf("no --repo in dispatched argv: %v", got)
	}
	repo := got[repoURLIdx]
	for _, want := range []string{
		"s3://bucket/prefix",
		"endpoint=http%3A%2F%2Fminio%3A9000",
		"path_style=true",
		"region=us-east-1",
	} {
		if !strings.Contains(repo, want) {
			t.Errorf("repo URL %q missing %q", repo, want)
		}
	}
}

func TestWalRestore_DispatchesNativeWalFetch(t *testing.T) {
	got, rc := runWithStubbedDispatch(t,
		map[string]string{"PGDATA": "/var/lib/postgresql/data/pgdata"},
		nil,
		ExecuteWalRestore,
		[]string{
			"--endpoint-url", "http://minio:9000",
			"--cloud-provider", "aws-s3",
			"s3://bucket/prefix",
			"discovery",
			"000000010000000000000004",
			"pg_wal/RECOVERYXLOG",
		},
	)
	if rc != 0 {
		t.Fatalf("exit %d", rc)
	}
	want := []string{
		"wal", "fetch", "discovery",
		"000000010000000000000004",
		"/var/lib/postgresql/data/pgdata/pg_wal/RECOVERYXLOG",
	}
	if !reflect.DeepEqual(got[:5], want) {
		t.Errorf("argv head = %v, want %v", got[:5], want)
	}
}

func TestBackup_DispatchesNativeBackup(t *testing.T) {
	got, rc := runWithStubbedDispatch(t,
		map[string]string{
			"PGHOST": "/controller/run",
			"PGPORT": "5432",
		},
		nil,
		ExecuteBackup,
		[]string{
			"--user", "postgres",
			"--name", "backup-20260508101757",
			"--gzip",
			"--endpoint-url", "http://minio:9000",
			"--cloud-provider", "aws-s3",
			"s3://bucket/prefix",
			"discovery",
		},
	)
	if rc != 0 {
		t.Fatalf("exit %d", rc)
	}
	if got[0] != "backup" || got[1] != "discovery" {
		t.Errorf("argv head = %v, want [backup discovery ...]", got[:2])
	}
	// --label propagates from --name.
	hasLabel := false
	for i, a := range got {
		if a == "--label" && i+1 < len(got) && got[i+1] == "backup-20260508101757" {
			hasLabel = true
		}
	}
	if !hasLabel {
		t.Errorf("expected --label backup-20260508101757; got %v", got)
	}
	// DSN must include the socket path.
	hasDSN := false
	for i, a := range got {
		if a == "--pg-connection" && i+1 < len(got) && strings.Contains(got[i+1], "host=/controller/run") {
			hasDSN = true
		}
	}
	if !hasDSN {
		t.Errorf("DSN missing host=/controller/run; got argv %v", got)
	}
}

func TestBuildRepoURL_RejectsNonS3Scheme(t *testing.T) {
	_, err := buildRepoURL("/some/local/path", commonFlags{}, bcEnv{})
	if err == nil {
		t.Errorf("expected error for non-s3 path")
	}
	if err != nil && !strings.Contains(err.Error(), "must start with") {
		t.Errorf("error message = %q", err.Error())
	}
}

func TestDeploymentName_Precedence(t *testing.T) {
	cases := []struct {
		env    bcEnv
		stanza string
		want   string
	}{
		{bcEnv{deployment: "explicit"}, "ignored", "explicit"},
		{bcEnv{}, "stanza-name", "stanza-name"},
		{bcEnv{pgHost: "/controller/run"}, "", "default"},
		{bcEnv{pgHost: "real-host"}, "", "real-host"},
		{bcEnv{}, "", "default"},
	}
	for i, c := range cases {
		got := c.env.deploymentName(c.stanza)
		if got != c.want {
			t.Errorf("case %d: got %q, want %q (env=%+v stanza=%q)", i, got, c.want, c.env, c.stanza)
		}
	}
}
