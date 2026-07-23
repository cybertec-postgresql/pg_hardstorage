// contract_test.go — full storage-contract suite for the scp plugin
// against a REAL sshd (host OpenSSH, loopback, throwaway keys).
//
// Why host sshd instead of a container: the scp plugin needs ssh EXEC
// (shell commands), which the atmoz/sftp fixture forbids; a local
// unprivileged sshd on a high port exercises the exact remote-shell
// primitives production uses — including the atomic `ln -T` commit for
// IfNotExists (the single-winner guarantee the shared-DEK mint, backup
// lease and audit chain depend on; concurrency audit / issue #31 class).
//
// Skips when /usr/sbin/sshd is absent unless
// PG_HARDSTORAGE_DEMAND_SSHD=1 (CI sets it so the suite can never
// silently stop running).
package scp_test

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
	"golang.org/x/crypto/ssh"
)

const sshdBin = "/usr/sbin/sshd"

func requireSSHD(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(sshdBin); err != nil {
		if os.Getenv("PG_HARDSTORAGE_DEMAND_SSHD") == "1" {
			t.Fatalf("PG_HARDSTORAGE_DEMAND_SSHD=1 but %s is missing", sshdBin)
		}
		t.Skipf("%s not present; skipping scp-vs-real-sshd contract suite", sshdBin)
	}
}

// sshdFixture is one running loopback sshd + the client material to
// reach it.
type sshdFixture struct {
	port       int
	root       string
	identity   string
	knownHosts string
	user       string
}

func startSSHD(t *testing.T) *sshdFixture {
	t.Helper()
	requireSSHD(t)
	dir := t.TempDir()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	// Keys: one host key, one client key (authorized for ourselves).
	hostKey := filepath.Join(dir, "host_ed25519")
	clientKey := filepath.Join(dir, "client_ed25519")
	for _, k := range []string{hostKey, clientKey} {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", k).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen %s: %v\n%s", k, err, out)
		}
	}
	authKeys := filepath.Join(dir, "authorized_keys")
	pub, err := os.ReadFile(clientKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authKeys, pub, 0o600); err != nil {
		t.Fatal(err)
	}

	// Pick a free port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	// Repo root the plugin will operate under.
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := filepath.Join(dir, "sshd_config")
	conf := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s
PidFile %s/sshd.pid
AuthorizedKeysFile %s
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
Subsystem sftp internal-sftp
AllowUsers %s
LogLevel ERROR
`, port, hostKey, dir, authKeys, u.Username)
	if err := os.WriteFile(cfg, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(sshdBin, "-D", "-f", cfg, "-e")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sshd: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	// Wait for the port, then build known_hosts from the host pubkey
	// (no keyscan round-trip needed — we own the host key).
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sshd did not start listening within 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	hostPub, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey(hostPub)
	if err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(dir, "known_hosts")
	line := fmt.Sprintf("[127.0.0.1]:%d %s %s\n", port, pk.Type(), ssh.FingerprintSHA256(pk))
	// knownhosts wants the base64 key, not the fingerprint — write the
	// authorized_keys form re-addressed to the host:port.
	line = fmt.Sprintf("[127.0.0.1]:%d %s", port, string(hostPub))
	if err := os.WriteFile(kh, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	return &sshdFixture{port: port, root: root, identity: clientKey, knownHosts: kh, user: u.Username}
}

// openSCPOnFresh satisfies contract.PluginOpener: every call gets a
// fresh sub-root on the shared sshd so cases are isolated.
func openSCPOnFresh(t *testing.T) storage.StoragePlugin {
	t.Helper()
	fx := startSSHD(t)
	p := &scp.Plugin{}
	u := &url.URL{
		Scheme: "scp",
		User:   url.User(fx.user),
		Host:   fmt.Sprintf("127.0.0.1:%d", fx.port),
		Path:   fx.root,
	}
	if err := p.Open(t.Context(), storage.StorageConfig{
		URL: u,
		Extras: map[string]string{
			"identity_file": fx.identity,
			"known_hosts":   fx.knownHosts,
		},
	}); err != nil {
		t.Fatalf("scp.Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestSCP_Contract runs the FULL storage contract — including the
// mandatory ParallelPuts single-winner and ParallelOverwrites cases —
// against a real sshd. This is the regression gate for the scp
// `ln -T` atomic-commit fix: the old stat+`mv -T` emulation failed
// ParallelPuts (two winners), which is exactly how the #31 DEK-fork
// class reached scp repos.
func TestSCP_Contract(t *testing.T) {
	contract.Run(t, openSCPOnFresh)
}
