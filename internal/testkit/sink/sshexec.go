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
// pre-baked: it needs a host key and an unlocked account, and a
// throwaway keypair must never be baked into a published image.
//
// The authorised key is injected at `docker run` time, NOT baked into
// the image, and that distinction is load-bearing. The image tag is
// shared by every instance so docker's layer cache makes repeat runs
// instant — but `go test` runs packages CONCURRENTLY, and two packages
// use this sink (internal/plugin/storage's wiring test and
// .../storage/scp's contract test). When each baked its own key into a
// build under that one shared tag, the later build re-pointed the tag
// and the earlier instance's `docker run` started a container
// authorising the OTHER package's key. Every operation then failed
// with "unable to authenticate, attempted methods [none publickey]" —
// intermittently, on whichever package lost the race.
//
// So the image holds nothing instance-specific, and its tag carries a
// hash of the Dockerfile that produced it: concurrent builds converge
// on identical content, and an edit here can never silently reuse a
// stale image built by an older revision.
package sink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// sshExecAuthKeyEnv carries the per-instance public key into the
	// container at run time.
	sshExecAuthKeyEnv = "PGHS_AUTHORIZED_KEY"
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
	image, err := s.buildImage(ctx, dir)
	if err != nil {
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
		"-e", sshExecAuthKeyEnv+"="+strings.TrimSpace(string(pub)),
		image).CombinedOutput()
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
	// Prove the instance's own key is accepted before handing the URL
	// out. Speaking SSH and accepting THIS key are different things,
	// and when they came apart the symptom was ~20 opaque failures
	// inside the contract suite rather than one fixture error here.
	if err := s.probeAuth(ctx, 30*time.Second); err != nil {
		_ = s.Down(context.Background())
		return err
	}
	return nil
}

// probeAuth runs a trivial remote command with the instance's key.
func (s *sshExecRuntime) probeAuth(ctx context.Context, total time.Duration) error {
	deadline := time.Now().Add(total)
	var last []byte
	for {
		cmd := exec.CommandContext(ctx, "ssh",
			"-i", s.identityFile,
			"-o", "IdentitiesOnly=yes",
			"-o", "BatchMode=yes",
			"-o", "UserKnownHostsFile="+s.knownHosts,
			"-o", "StrictHostKeyChecking=yes",
			"-o", "ConnectTimeout=5",
			"-p", fmt.Sprint(s.port),
			sshExecUser+"@127.0.0.1", "true")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		last = out
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", s.container).CombinedOutput()
			return fmt.Errorf("ssh-exec sink: sshd rejected the instance's own key within %s "+
				"(%s); container logs: %s",
				total, truncate(last, 200), truncate(logs, 400))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// sshExecEntrypoint installs the run-time-supplied public key and then
// becomes sshd. Writing the key here rather than at build time is what
// keeps the image free of per-instance material; sshd only starts once
// the key is in place, so a container that answers SSH is a container
// that will accept its instance's key.
//
// StrictModes means sshd ignores an authorized_keys it does not
// consider safely owned, and does so silently from the client's side,
// so ownership and mode are set explicitly rather than inherited.
const sshExecEntrypoint = `set -e
if [ -z "${` + sshExecAuthKeyEnv + `:-}" ]; then
  echo "ssh-exec fixture: ` + sshExecAuthKeyEnv + ` is empty; no key would be authorised" >&2
  exit 1
fi
printf '%s\n' "$` + sshExecAuthKeyEnv + `" > /home/` + sshExecUser + `/.ssh/authorized_keys
chown ` + sshExecUser + ` /home/` + sshExecUser + `/.ssh/authorized_keys
chmod 600 /home/` + sshExecUser + `/.ssh/authorized_keys
exec /usr/sbin/sshd -D -e
`

// sshExecDockerfile is the image definition. It is a function rather
// than a constant so a test can hash it without invoking docker.
//
// `passwd -u` matters: Alpine's `adduser -D` leaves the account with a
// locked password, and OpenSSH refuses a locked account even for
// publickey auth ("User ... not allowed because account is locked").
// Without it every connection fails at auth.
func sshExecDockerfile() string {
	return "FROM " + SinkImages["ssh-exec"] + "\n" +
		"RUN apk add --no-cache openssh-server coreutils findutils && \\\n" +
		"    ssh-keygen -A && \\\n" +
		"    adduser -D -s /bin/sh " + sshExecUser + " && passwd -u " + sshExecUser + "\n" +
		"RUN mkdir -p /home/" + sshExecUser + "/.ssh " + sshExecRoot + " && \\\n" +
		"    chown -R " + sshExecUser + " /home/" + sshExecUser + " " + sshExecRoot + " && \\\n" +
		"    chmod 700 /home/" + sshExecUser + "/.ssh\n" +
		"COPY entrypoint.sh /usr/local/bin/entrypoint.sh\n" +
		"EXPOSE 22\n" +
		`CMD ["/bin/sh","/usr/local/bin/entrypoint.sh"]` + "\n"
}

// sshExecImageTag derives the image tag from the content that defines
// the image. This is what makes concurrent builds safe: instances that
// would produce identical images converge on one tag, so no instance
// can re-point a tag another is about to run.
//
// Both inputs are hashed. The entrypoint is a separate file in the
// build context, so a change to it does not alter the Dockerfile text
// — hash only the Dockerfile and an entrypoint edit would silently
// reuse the previous image.
func sshExecImageTag(dockerfile, entrypoint string) string {
	sum := sha256.Sum256([]byte(dockerfile + "\x00" + entrypoint))
	return "pg-hardstorage-testkit-sshexec:" + hex.EncodeToString(sum[:6])
}

// buildImage builds the key-free sshd image and returns the tag it was
// built under. The tag embeds a hash of the Dockerfile, so every
// instance that would produce identical content converges on one tag
// (full cache hit, no races) while any edit here lands on a fresh one.
func (s *sshExecRuntime) buildImage(ctx context.Context, dir string) (string, error) {
	bctx := filepath.Join(dir, "img")
	if err := os.MkdirAll(bctx, 0o755); err != nil {
		return "", fmt.Errorf("ssh-exec sink: mkdir build ctx: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bctx, "entrypoint.sh"),
		[]byte(sshExecEntrypoint), 0o755); err != nil {
		return "", fmt.Errorf("ssh-exec sink: write entrypoint: %w", err)
	}
	dockerfile := sshExecDockerfile()
	if err := os.WriteFile(filepath.Join(bctx, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", fmt.Errorf("ssh-exec sink: write Dockerfile: %w", err)
	}

	tag := sshExecImageTag(dockerfile, sshExecEntrypoint)

	out, err := exec.CommandContext(ctx, "docker", "build", "-q",
		"-t", tag, bctx).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh-exec sink: docker build: %w (%s)", err, truncate(out, 400))
	}
	return tag, nil
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
