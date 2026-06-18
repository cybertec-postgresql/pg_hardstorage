package sftp_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/sftp"
)

// The SFTP plugin's full integration test would need a live
// SSH server (testcontainers).  We can't bring a container up
// in unit tests reliably, so the SFTP plugin's unit-level
// tests focus on:
//
//   - Open() rejection paths (missing known_hosts, missing
//     auth, malformed URL).
//   - resolve() / path-traversal refusal.
//
// End-to-end coverage lives at the testkit L3+ level where a
// docker-backed openssh-server can be spun up.

func TestOpen_RequiresKnownHosts(t *testing.T) {
	u, _ := url.Parse("sftp://user@host:22/srv/repo")
	p := &sftp.Plugin{}
	err := p.Open(context.Background(), storage.StorageConfig{
		URL: u,
		Extras: map[string]string{
			"identity_file": "/dev/null",
		},
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
	u, _ := url.Parse("sftp://user@host:22/srv/repo")
	p := &sftp.Plugin{}
	err := p.Open(context.Background(), storage.StorageConfig{
		URL: u,
		Extras: map[string]string{
			"known_hosts": khPath,
		},
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
	os.WriteFile(khPath, []byte(""), 0o600)
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n-----END OPENSSH PRIVATE KEY-----"), 0o600); err != nil {
		t.Fatal(err)
	}

	u, _ := url.Parse("sftp://user@host")
	p := &sftp.Plugin{}
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
	os.WriteFile(khPath, []byte(""), 0o600)
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("sftp://user@host:22/srv/repo")
	p := &sftp.Plugin{}
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
	p := &sftp.Plugin{}
	if _, err := p.Get(context.Background(), "key"); err == nil {
		t.Error("Get should refuse on a not-yet-opened plugin")
	}
	if _, err := p.Stat(context.Background(), "key"); err == nil {
		t.Error("Stat should refuse on a not-yet-opened plugin")
	}
	if _, err := p.Put(context.Background(), "key", nil, storage.PutOptions{}); err == nil {
		t.Error("Put should refuse on a not-yet-opened plugin")
	}
}

func TestPlugin_Capabilities(t *testing.T) {
	p := &sftp.Plugin{}
	cap := p.Capabilities()
	if !cap.ConditionalPut {
		t.Error("SFTP plugin should advertise ConditionalPut (emulated)")
	}
}

func TestPlugin_Name(t *testing.T) {
	p := &sftp.Plugin{}
	if p.Name() != "sftp" {
		t.Errorf("Name = %q, want %q", p.Name(), "sftp")
	}
}

func TestPlugin_SetRetentionUnsupported(t *testing.T) {
	p := &sftp.Plugin{}
	err := p.SetRetention(context.Background(), "key", time.Time{}, storage.WORMCompliance)
	if !errors.Is(err, storage.ErrUnsupported) {
		t.Errorf("SetRetention should return ErrUnsupported; got %v", err)
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
