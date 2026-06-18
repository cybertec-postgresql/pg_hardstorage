package translate

import (
	"os"
	"strings"
	"testing"
)

func TestParse_HappyPath(t *testing.T) {
	f, err := os.Open("testdata/wal-g.env")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	env, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if env.KV["WALG_S3_PREFIX"] != "s3://acme-pg-backups/wal-g" {
		t.Errorf("WALG_S3_PREFIX = %q", env.KV["WALG_S3_PREFIX"])
	}
	if env.KV["PGHOST"] != "db1.example.com" {
		t.Errorf("PGHOST = %q", env.KV["PGHOST"])
	}
	if env.KV["WALG_DOWNLOAD_CONCURRENCY"] != "32" {
		t.Errorf("expected unquoted DOWNLOAD_CONCURRENCY=32; got %q",
			env.KV["WALG_DOWNLOAD_CONCURRENCY"])
	}
	// The `export ` prefix should be stripped.
	if env.KV["WALG_UPLOAD_CONCURRENCY"] != "16" {
		t.Errorf("expected `export `-prefixed UPLOAD_CONCURRENCY=16; got %q",
			env.KV["WALG_UPLOAD_CONCURRENCY"])
	}
}

func TestParse_MissingEquals(t *testing.T) {
	_, err := Parse(strings.NewReader("KEY_WITHOUT_VALUE\n"))
	if err == nil || !strings.Contains(err.Error(), "missing '='") {
		t.Errorf("expected missing-= error; got %v", err)
	}
}

func TestParse_CommentsAndBlanks(t *testing.T) {
	body := `
# leading blank + comment lines tolerated

KEY=value
   # indented comment

`
	env, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if got := env.KV["KEY"]; got != "value" {
		t.Errorf("KEY = %q", got)
	}
	if len(env.KV) != 1 {
		t.Errorf("expected 1 entry; got %v", env.KV)
	}
}

func TestTranslate_HappyPath(t *testing.T) {
	f, _ := os.Open("testdata/wal-g.env")
	defer f.Close()
	env, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Translate(env)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"deployments:",
		"name: db1.example.com",
		"pg_connection: \"postgres://pgbackup@db1.example.com:5432/postgres\"",
		"repo: \"s3://acme-pg-backups/wal-g\"",
		// libsodium triggers the encryption stub.
		"encryption:",
		"kek_ref: \"local:default\"",
		// lz4 emits a comment.
		"WALG_COMPRESSION_METHOD=lz4 ignored",
	}
	for _, fragment := range want {
		if !strings.Contains(res.YAML, fragment) {
			t.Errorf("YAML missing %q\n---\n%s", fragment, res.YAML)
		}
	}

	// Warnings: AWS creds, libsodium-key remediation, lz4 dropped.
	wantWarn := []string{
		"AWS credentials",
		"WAL-G envelope (libsodium/GPG/PGP) is not honoured",
		"WALG_COMPRESSION_METHOD=lz4 ignored",
	}
	for _, w := range wantWarn {
		found := false
		for _, got := range res.Warnings {
			if strings.Contains(got, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("warnings missing %q; got %v", w, res.Warnings)
		}
	}

	// Unmapped: AWS_ENDPOINT, AWS_REGION, WALG_DELTA_MAX_STEPS,
	// WALG_TAR_SIZE_THRESHOLD, WALG_DOWNLOAD_CONCURRENCY,
	// WALG_UPLOAD_CONCURRENCY all lack a direct mapping.
	wantUnmapped := []string{"AWS_ENDPOINT", "AWS_REGION", "WALG_DELTA_MAX_STEPS",
		"WALG_TAR_SIZE_THRESHOLD", "WALG_DOWNLOAD_CONCURRENCY",
		"WALG_UPLOAD_CONCURRENCY"}
	for _, u := range wantUnmapped {
		found := false
		for _, got := range res.Unmapped {
			if strings.Contains(got, u) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unmapped should mention %q; got %v", u, res.Unmapped)
		}
	}
}

func TestTranslate_FilePrefix(t *testing.T) {
	body := `WALG_FILE_PREFIX=/srv/wal-g
PGHOST=db.example.com
`
	env, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Translate(env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.YAML, `repo: "file:///srv/wal-g"`) {
		t.Errorf("expected file:// repo; got\n%s", res.YAML)
	}
}

func TestTranslate_RelativeFilePrefix_FlaggedUnmapped(t *testing.T) {
	body := `WALG_FILE_PREFIX=relative/path
PGHOST=db.example.com
`
	env, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Translate(env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.YAML, "repo:") {
		t.Errorf("relative path should NOT emit a repo line; got\n%s", res.YAML)
	}
	found := false
	for _, u := range res.Unmapped {
		if strings.Contains(u, "WALG_FILE_PREFIX") && strings.Contains(u, "not absolute") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected unmapped entry flagging non-absolute path; got %v", res.Unmapped)
	}
}

func TestTranslate_DefaultDeploymentName(t *testing.T) {
	body := `WALG_S3_PREFIX=s3://x/y
` // no PGHOST, no PG_HARDSTORAGE_DEPLOYMENT
	env, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Translate(env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.YAML, "name: default") {
		t.Errorf("expected fallback name=default; got\n%s", res.YAML)
	}
}

func TestTranslate_ExplicitDeploymentName(t *testing.T) {
	body := `WALG_S3_PREFIX=s3://x/y
PGHOST=db.example.com
PG_HARDSTORAGE_DEPLOYMENT=prod-db
`
	env, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Translate(env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.YAML, "name: prod-db") {
		t.Errorf("explicit deployment name should win; got\n%s", res.YAML)
	}
}
