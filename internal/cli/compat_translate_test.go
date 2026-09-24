package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// captureStderr runs fn with os.Stderr redirected and returns what it
// wrote. The translators print their notes straight to os.Stderr.
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	ferr := fn()
	os.Stderr = prev
	w.Close()
	return <-done, ferr
}

// Unmapped settings are echoed to stderr for review — which lands in
// terminals, CI logs and journald. Secret values (AWS_SECRET_ACCESS_KEY,
// repo1-s3-key-secret, passwords in conninfo) must never be echoed;
// the key alone tells the operator what to review.
func TestCompatTranslate_UnmappedSecretsNotEchoed(t *testing.T) {
	dir := t.TempDir()
	const secret = "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"
	cases := []struct {
		from, file, body string
	}{
		{"walg", "walg.env", "WALG_S3_PREFIX=s3://b/p\nAWS_SECRET_ACCESS_KEY=" + secret + "\nPGPASSWORD=" + secret + "\n"},
		{"pgbackrest", "pgbackrest.conf", "[global]\nrepo1-path=/r\nrepo1-s3-key-secret=" + secret +
			"\n[db1]\npg1-host=h\nrepo1-s3-key-secret=" + secret + "\n"},
		{"barman", "barman.conf", "[barman]\nbarman_home=/var/lib/barman\n[db1]\nconninfo=host=h user=u dbname=postgres\n" +
			"backup_directory=/var/lib/barman/db1\nstreaming_password=" + secret + "\n" +
			"ssh_command=ssh postgres@h -o 'Password=x' password=" + secret + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.from, func(t *testing.T) {
			in := filepath.Join(dir, tc.file)
			if err := os.WriteFile(in, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, tc.from+".yaml")
			stderr, err := captureStderr(t, func() error { return runCompatTranslate(tc.from, in, out, true) })
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if strings.Contains(stderr, secret) {
				t.Fatalf("secret echoed to stderr:\n%s", stderr)
			}
		})
	}
}

// An unknown --from is a usage error (exit 2), not a runtime failure.
func TestCompatTranslate_BadFromIsUsage(t *testing.T) {
	err := runCompatTranslate("bacula", "/nonexistent", "", false)
	if err == nil || !errors.Is(err, output.ErrUsage) {
		t.Fatalf("want ErrUsage, got %v", err)
	}
}
