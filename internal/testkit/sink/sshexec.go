// sshexec.go — OpenSSH sink that permits ssh-exec, for the scp plugin.
//
// The scp backend moves bytes with remote shell commands (`cat`,
// `stat`, `find`, `ln -T`, `mv -T`), so it needs a server that allows
// command execution. atmoz/sftp — the fixture the sftp plugin uses —
// deliberately forbids exec, so scp cannot reuse it.
//
// Until this existed, scp's only real-server coverage ran against the
// HOST's /usr/sbin/sshd. That silently skips wherever sshd isn't
// installed (most containers, most CI images, macOS dev boxes), and it
// couples the suite to whatever sshd_config and OpenSSH version the
// host happens to ship. This runtime gives scp the same
// container-pinned, reproducible footing every other backend has.
//
// The image is built locally from alpine rather than pulled
// pre-baked: it needs a host key, an unlocked account, and an
// authorized_keys entry generated per-instance, and a throwaway
// keypair must never be baked into a published image.
package sink

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const (
	sshExecUser = "pghs"
	sshExecRoot = "/srv/repo"
	// sshExecImage is built on demand; the tag is stable so docker's
	// layer cache makes repeat runs ~instant.
	sshExecImage = "pg-hardstorage-testkit-sshexec:alpine-3.20"
)

var sshExecCounter atomic.Uint64

// sshExecRuntime is an OpenSSH container with exec enabled.
type sshExecRuntime struct {
	container string
	port      int
	dir       string // holds the client keypair + known_hosts

	identityFile string
	knownHosts   string
}

func newSSHExec() *sshExecRuntime { return &sshExecRuntime{} }

// Name implements Runtime.
func (s *sshExecRuntime) Name() string { return "ssh-exec" }

// Up generates a throwaway keypair, builds (or reuses) the sshd image
// with that public key authorised, starts it, and writes a
// per-instance known_hosts via ssh-keyscan.
func (s *sshExecRuntime) Up(ctx context.Context) error {
	if s.container != "" {
		return errors.New("sshExecRuntime: already up (call Down first)")
	}
	dir, err := os.MkdirTemp("", "pg-hs-sshexec-*")
	if err != nil {
		return fmt.Errorf("ssh-exec sink: tempdir: %w", err)
	}
	s.dir = dir

	key := filepath.Join(dir, "id_ed25519")
	if out, err := exec.CommandContext(ctx, "ssh-keygen",
		"-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		s.cleanupDir()
		return fmt.Errorf("ssh-exec sink: ssh-keygen: %w (%s)", err, truncate(out, 200))
	}
	s.identityFile = key

	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		s.cleanupDir()
		return fmt.Errorf("ssh-exec sink: read pubkey: %w", err)
	}
	if err := s.buildImage(ctx, dir, pub); err != nil {
		s.cleanupDir()
		return err
	}

	port, err := pickFreePort()
	if err != nil {
		s.cleanupDir()
		return fmt.Errorf("ssh-exec sink: pick port: %w", err)
	}
	s.port = port
	s.container = fmt.Sprintf("pg-hs-sshexec-%d-%d-%d",
		time.Now().UnixNano(), os.Getpid(), sshExecCounter.Add(1))

	out, err := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", s.container,
		"-p", fmt.Sprintf("127.0.0.1:%d:22", s.port),
		sshExecImage).CombinedOutput()
	if err != nil {
		name := s.container
		s.container = ""
		s.cleanupDir()
		return fmt.Errorf("ssh-exec sink: docker run %s: %w (%s)", name, err, truncate(out, 256))
	}

	if err := waitTCPReady(ctx, s.port, 30*time.Second); err != nil {
		_ = s.Down(context.Background())
		return err
	}
	// ssh-keyscan is the real readiness probe: it only succeeds once
	// sshd is speaking SSH, not merely once docker's port-proxy
	// accepts TCP.
	if err := s.writeKnownHosts(ctx, 60*time.Second); err != nil {
		_ = s.Down(context.Background())
		return err
	}
	return nil
}

// buildImage writes a Dockerfile authorising pub and builds it.
func (s *sshExecRuntime) buildImage(ctx context.Context, dir string, pub []byte) error {
	bctx := filepath.Join(dir, "img")
	if err := os.MkdirAll(bctx, 0o755); err != nil {
		return fmt.Errorf("ssh-exec sink: mkdir build ctx: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bctx, "authorized_keys"), pub, 0o644); err != nil {
		return fmt.Errorf("ssh-exec sink: write authorized_keys: %w", err)
	}
	// `passwd -u` matters: Alpine's `adduser -D` leaves the account
	// with a locked password, and OpenSSH refuses a locked account
	// even for publickey auth ("User ... not allowed because account
	// is locked"). Without it every connection fails at auth.
	dockerfile := "FROM " + SinkImages["ssh-exec"] + "\n" +
		"RUN apk add --no-cache openssh-server coreutils findutils && \\\n" +
		"    ssh-keygen -A && \\\n" +
		"    adduser -D -s /bin/sh " + sshExecUser + " && passwd -u " + sshExecUser + "\n" +
		"RUN mkdir -p /home/" + sshExecUser + "/.ssh " + sshExecRoot + " && \\\n" +
		"    chown -R " + sshExecUser + " /home/" + sshExecUser + " " + sshExecRoot + "\n" +
		"COPY authorized_keys /home/" + sshExecUser + "/.ssh/authorized_keys\n" +
		"RUN chown " + sshExecUser + " /home/" + sshExecUser + "/.ssh/authorized_keys && \\\n" +
		"    chmod 600 /home/" + sshExecUser + "/.ssh/authorized_keys\n" +
		"EXPOSE 22\n" +
		`CMD ["/usr/sbin/sshd","-D","-e"]` + "\n"
	if err := os.WriteFile(filepath.Join(bctx, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("ssh-exec sink: write Dockerfile: %w", err)
	}
	out, err := exec.CommandContext(ctx, "docker", "build", "-q",
		"-t", sshExecImage, bctx).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh-exec sink: docker build: %w (%s)", err, truncate(out, 400))
	}
	return nil
}

func (s *sshExecRuntime) writeKnownHosts(ctx context.Context, total time.Duration) error {
	deadline := time.Now().Add(total)
	for {
		out, err := exec.CommandContext(ctx, "ssh-keyscan",
			"-p", fmt.Sprint(s.port), "127.0.0.1").Output()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			kh := filepath.Join(s.dir, "known_hosts")
			if werr := os.WriteFile(kh, out, 0o600); werr != nil {
				return fmt.Errorf("ssh-exec sink: write known_hosts: %w", werr)
			}
			s.knownHosts = kh
			return nil
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", s.container).CombinedOutput()
			return fmt.Errorf("ssh-exec sink: sshd not answering SSH within %s; logs: %s",
				total, truncate(logs, 400))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Down implements Runtime. Idempotent.
func (s *sshExecRuntime) Down(ctx context.Context) error {
	if s.container != "" {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", s.container).Run()
		s.container = ""
	}
	s.cleanupDir()
	return nil
}

func (s *sshExecRuntime) cleanupDir() {
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
		s.dir = ""
	}
}

// URL implements Runtime.
func (s *sshExecRuntime) URL() string {
	return fmt.Sprintf("scp://%s@127.0.0.1:%d%s", sshExecUser, s.port, sshExecRoot)
}

// EnvForAgent implements Runtime. These are the ONLY channel by which
// the scp plugin can be configured in production — StorageConfig.Extras
// is never populated by storage.Open (see wiring_e2e_test.go).
func (s *sshExecRuntime) EnvForAgent() map[string]string {
	return map[string]string{
		"PG_HARDSTORAGE_SCP_KNOWN_HOSTS":   s.knownHosts,
		"PG_HARDSTORAGE_SCP_IDENTITY_FILE": s.identityFile,
	}
}

// ContainerName implements Runtime.
func (s *sshExecRuntime) ContainerName() string { return s.container }

// Extras implements Runtime. Returned for tests that drive
// plugin.Open directly; production never reads it.
func (s *sshExecRuntime) Extras() map[string]string {
	return map[string]string{
		"known_hosts":   s.knownHosts,
		"identity_file": s.identityFile,
	}
}
