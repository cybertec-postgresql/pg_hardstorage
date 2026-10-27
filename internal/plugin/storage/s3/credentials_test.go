package s3

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func openS3(t *testing.T, raw string) (*Plugin, error) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{}
	return p, p.Open(context.Background(), storage.StorageConfig{URL: u})
}

func accessKey(t *testing.T, p *Plugin) string {
	t.Helper()
	c, err := p.client.Options().Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c.AccessKeyID
}

// Two s3:// URLs in one process authenticate differently (issue #76).
func TestOpen_PerURLCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AMBIENT")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")
	t.Setenv("SRC_ACCESS_KEY_ID", "SRCKEY")
	t.Setenv("SRC_SECRET_ACCESS_KEY", "src-secret")

	src, err := openS3(t, "s3://src/p?endpoint=http://127.0.0.1:9000&credentials_env=SRC")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := openS3(t, "s3://dst/p")
	if err != nil {
		t.Fatal(err)
	}
	if got := accessKey(t, src); got != "SRCKEY" {
		t.Errorf("src access key = %q, want SRCKEY", got)
	}
	if got := accessKey(t, dst); got != "AMBIENT" {
		t.Errorf("dst access key = %q, want AMBIENT", got)
	}
}

func TestOpen_ProfileCredentials(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials")
	if err := os.WriteFile(creds, []byte("[minio]\naws_access_key_id = MINIOKEY\naws_secret_access_key = s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	p, err := openS3(t, "s3://b/p?profile=minio")
	if err != nil {
		t.Fatal(err)
	}
	if got := accessKey(t, p); got != "MINIOKEY" {
		t.Errorf("access key = %q, want MINIOKEY", got)
	}
}

func TestOpen_CredentialsEnvErrors(t *testing.T) {
	if _, err := openS3(t, "s3://b/p?credentials_env=NOPE_UNSET"); err == nil || !strings.Contains(err.Error(), "NOPE_UNSET_ACCESS_KEY_ID") {
		t.Errorf("unset credentials_env: err = %v", err)
	}
	if _, err := openS3(t, "s3://b/p?profile=a&credentials_env=B"); err == nil {
		t.Error("profile + credentials_env must be rejected")
	}
}
