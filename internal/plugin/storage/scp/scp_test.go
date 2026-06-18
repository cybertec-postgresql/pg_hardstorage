package scp_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
)

// The scp backend's full integration test would need a live
// SSH server (testcontainers).  Like the sftp plugin, we focus
// the unit suite on:
//
//   - Open() rejection paths (missing known_hosts, missing
//     auth, malformed URL).
//   - resolve() / path-traversal refusal.
//   - shell-quote correctness (the only shell-injection
//     defence).
//   - method refusals on a not-yet-opened plugin.
//
// End-to-end coverage lives at the testkit L3+ tier where a
// docker-backed openssh-server gets spun up.

func TestOpen_RequiresKnownHosts(t *testing.T) {
	u, _ := url.Parse("scp://user@host:22/srv/repo")
	p := &scp.Plugin{}
	err := p.Open(context.Background(), storage.StorageConfig{
		URL:    u,
		Extras: map[string]string{"identity_file": "/dev/null"},
	})
	if err == nil {
		t.Fatal("expected refusal without known_hosts")
	}
	if !errContains(err, "known_hosts") {
		t.Errorf("error should mention known_hosts: %v", err)
	}
}

func TestOpen_RequiresAuthMethod(t *testing.T) {
	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(khPath, []byte("# empty known_hosts\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("scp://user@host:22/srv/repo")
	p := &scp.Plugin{}
	err := p.Open(context.Background(), storage.StorageConfig{
		URL:    u,
		Extras: map[string]string{"known_hosts": khPath},
	})
	if err == nil {
		t.Fatal("expected refusal without identity / password")
	}
	if !errContains(err, "identity") {
		t.Errorf("error should mention identity_file: %v", err)
	}
}

func TestOpen_RequiresAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	_ = os.WriteFile(khPath, []byte(""), 0o600)
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath,
		[]byte("-----BEGIN OPENSSH PRIVATE KEY-----\n-----END OPENSSH PRIVATE KEY-----"),
		0o600); err != nil {
		t.Fatal(err)
	}

	u, _ := url.Parse("scp://user@host")
	p := &scp.Plugin{}
	err := p.Open(context.Background(), storage.StorageConfig{
		URL: u,
		Extras: map[string]string{
			"known_hosts":   khPath,
			"identity_file": keyPath,
		},
	})
	if err == nil {
		t.Fatal("expected refusal without URL path")
	}
	if !errContains(err, "absolute repo path") {
		t.Errorf("error should mention absolute path: %v", err)
	}
}

func TestOpen_RejectsMalformedIdentity(t *testing.T) {
	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	_ = os.WriteFile(khPath, []byte(""), 0o600)
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("scp://user@host:22/srv/repo")
	p := &scp.Plugin{}
	err := p.Open(context.Background(), storage.StorageConfig{
		URL: u,
		Extras: map[string]string{
			"known_hosts":   khPath,
			"identity_file": keyPath,
		},
	})
	if err == nil {
		t.Fatal("expected refusal for malformed key")
	}
}

func TestPlugin_NotOpenRefuses(t *testing.T) {
	p := &scp.Plugin{}
	if _, err := p.Get(context.Background(), "key"); err == nil {
		t.Error("Get should refuse on a not-yet-opened plugin")
	}
	if _, err := p.Stat(context.Background(), "key"); err == nil {
		t.Error("Stat should refuse on a not-yet-opened plugin")
	}
	if _, err := p.Put(context.Background(), "key", nil, storage.PutOptions{}); err == nil {
		t.Error("Put should refuse on a not-yet-opened plugin")
	}
	if err := p.Delete(context.Background(), "key"); err == nil {
		t.Error("Delete should refuse on a not-yet-opened plugin")
	}
	if err := p.RenameIfNotExists(context.Background(), "src", "dst"); err == nil {
		t.Error("RenameIfNotExists should refuse on a not-yet-opened plugin")
	}
}

func TestPlugin_Capabilities(t *testing.T) {
	p := &scp.Plugin{}
	c := p.Capabilities()
	if !c.ConditionalPut {
		t.Error("scp plugin should advertise ConditionalPut (emulated)")
	}
}

func TestPlugin_Name(t *testing.T) {
	p := &scp.Plugin{}
	if p.Name() != "scp" {
		t.Errorf("Name = %q, want %q", p.Name(), "scp")
	}
}

func TestPlugin_SetRetentionUnsupported(t *testing.T) {
	p := &scp.Plugin{}
	err := p.SetRetention(context.Background(), "key", time.Time{}, storage.WORMCompliance)
	if !errors.Is(err, storage.ErrUnsupported) {
		t.Errorf("SetRetention should return ErrUnsupported; got %v", err)
	}
}

func TestSchemeRegistered(t *testing.T) {
	// Importing this _test package causes the scp package's
	// init() to fire registration via the underscore import
	// in scp.go.  Schemes() should now report "scp" alongside
	// the other backends.
	hasSCP := false
	for _, s := range storage.Schemes() {
		if s == "scp" {
			hasSCP = true
		}
	}
	if !hasSCP {
		t.Errorf("storage.Schemes() did not include scp; got %v",
			storage.Schemes())
	}
}

// shell-quote correctness: the function is the only shell-
// injection defence between user-controlled keys and the
// remote command line.  The behaviour is well-defined POSIX so
// we can assert it directly.
func TestShellQuote_Roundtrip(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "''"},
		{"plain", "'plain'"},
		{"with space", "'with space'"},
		{"path/to/file.chk", "'path/to/file.chk'"},
		{"don't", `'don'\''t'`},
		// Adversarial: a key containing a single quote followed
		// by a shell metachar must still be quoted such that
		// `bash -c "echo CMD"` would print it literally.
		{"a'; rm -rf /", `'a'\''; rm -rf /'`},
	}
	for _, tt := range cases {
		t.Run(tt.in, func(t *testing.T) {
			got := scp.ShellQuoteForTest(tt.in)
			if got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

// errContains is a tiny helper since we don't pull in
// strings here.
func errContains(err error, sub string) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
