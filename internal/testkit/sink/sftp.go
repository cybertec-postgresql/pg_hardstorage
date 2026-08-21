// sftp.go — SFTP sink (OpenSSH chroot-sftp, built locally from alpine).
package sink

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// sftpRuntime brings up the chroot-sftp container, an
// OpenSSH-based SFTP server with a tiny per-user setup line.
// The image is built locally (see buildImage) so the fixture
// exists on every architecture the host can build for.
//
// Image arg syntax (kept compatible with atmoz/sftp, the
// image this fixture replaced):
//
//	<user>:<password>:[uid]:[gid]:[absolute-or-relative-dir]
//
// We pass `testkit:testkit:1001::upload` which:
//   - creates OS user `testkit` with uid 1001
//   - sets password `testkit`
//   - makes /home/testkit/upload writable by the user
//
// The pg_hardstorage sftp plugin parses
// sftp://[user[:pass]@]host[:port]/<absolute-path>; we ship
// the credentials inline in the URL so EnvForAgent is empty
// (no SSH-key host plumbing needed in tests).
type sftpRuntime struct {
	scratchDir string // PGHS_SINK_SCRATCH bind-mount dir, "" when unset

	container string
	port      int
	// knownHosts is the path to a per-instance known_hosts
	// file built via ssh-keyscan against the freshly-started
	// container.  The SFTP plugin reads this via
	// cfg.Extras["known_hosts"]; without it, the plugin
	// (correctly) refuses to skip strict host-key checking.
	knownHosts string
}

const (
	sftpUser = "testkit"
	sftpPass = "testkit"
	sftpDir  = "upload" // created at /home/<user>/upload by the entrypoint
)

var sftpCounter atomic.Uint64

func newSFTP() *sftpRuntime { return &sftpRuntime{} }

// Name returns "sftp".
func (s *sftpRuntime) Name() string { return "sftp" }

// Up builds (or reuses) the fixture image, runs it with the testkit user
// pre-created, waits for the SSH banner (sshd takes longer
// than the TCP listener under daemon contention), and writes
// a per-instance known_hosts file via ssh-keyscan.
func (s *sftpRuntime) Up(ctx context.Context) error {
	if s.container != "" {
		return errors.New("sftpRuntime: already up")
	}
	image, err := s.buildImage(ctx)
	if err != nil {
		return err
	}
	port, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("sftp sink: pick port: %w", err)
	}
	s.port = port
	s.container = fmt.Sprintf("pg-hs-sftp-%d-%d",
		time.Now().UnixMilli(), sftpCounter.Add(1))

	args := []string{
		"run", "-d",
		"--name", s.container,
		"-p", fmt.Sprintf("127.0.0.1:%d:22", s.port),
	}
	// PGHS_SINK_SCRATCH relocates the server's upload directory onto a
	// host path via bind mount. Without it the data lives in the
	// container layer under docker's data-root — which sits on the
	// root filesystem on typical hosts, and the storage soaks write at
	// disk speed: the sftp fault soak filled a 7 GiB root reserve in
	// 68 seconds, and the resulting server-side SSH_FX_FAILURE on a
	// clean-path put is indistinguishable from a plugin bug until df
	// is checked. Announced at startup by the soak's own log line
	// (url=...); world-writable because the container writes as its
	// own uid (1001).
	if base := os.Getenv("PGHS_SINK_SCRATCH"); base != "" {
		dir := filepath.Join(base, s.container+"-upload")
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return fmt.Errorf("sftp sink: create scratch %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			return fmt.Errorf("sftp sink: chmod scratch %s: %w", dir, err)
		}
		s.scratchDir = dir
		args = append(args, "-v", dir+":/home/"+sftpUser+"/"+sftpDir)
	}
	args = append(args,
		image,
		fmt.Sprintf("%s:%s:1001::%s", sftpUser, sftpPass, sftpDir),
	)
	cmd := exec.CommandContext(ctx, "docker", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		s.container = ""
		return fmt.Errorf("sftp sink: docker run: %w (output: %s)",
			err, truncate(out, 256))
	}
	// docker run -d returns success when the container STARTS, not
	// when it survives: a fixture image that cannot execute on this
	// host (amd64-only image on an arm64 host without binfmt/qemu)
	// exits milliseconds after start. Fail fast with docker's verdict
	// instead of burning the banner-wait budget on a dead port.
	if err := ensureContainerRunning(ctx, s.container, 15*time.Second); err != nil {
		_ = s.Down(context.Background())
		return err
	}
	// SSH server opens its TCP socket as part of normal
	// startup; once we can dial it, sshd is ready.
	if err := waitTCPReady(ctx, s.port, 30*time.Second); err != nil {
		_ = s.Down(context.Background())
		return err
	}

	// TCP-ready isn't enough — docker's port-proxy listens
	// before sshd is actually serving SSH protocol, AND
	// the entrypoint generates host keys at first boot (the
	// container's startup script writes /etc/ssh/ssh_host_*
	// keys before exec'ing sshd; under docker-daemon
	// contention this can take >15s).  Wait for the SSH
	// banner before invoking ssh-keyscan — it's the only
	// proof sshd is actually responding to SSH protocol
	// and not just letting docker's proxy accept TCP.
	if err := s.waitSSHBanner(ctx, 60*time.Second); err != nil {
		_ = s.Down(context.Background())
		return fmt.Errorf("sftp sink: ssh banner: %w", err)
	}

	// Build per-instance known_hosts via ssh-keyscan.  The
	// SFTP plugin reads this file at Open time; without it
	// the plugin refuses to start (correctly — silent
	// strict-host-key-skip is exactly the kind of test-
	// only escape hatch you don't want shipping into
	// production code).  Each test instance has a unique
	// host key (generated on first start) so the file is
	// per-Up.
	if err := s.writeKnownHosts(ctx); err != nil {
		_ = s.Down(context.Background())
		return fmt.Errorf("sftp sink: known_hosts: %w", err)
	}
	return nil
}

// writeKnownHosts shells out to ssh-keyscan against the
// fresh container's port-mapped sshd, writes the result
// to a per-instance temp file, and stashes the path on
// s.knownHosts for Extras().  ssh-keyscan must be on PATH
// — it ships in openssh-client which any host doing SFTP
// testing already has.
//
// Retry: TCP-ready isn't the same as SSH-protocol-ready.
// sshd opens the listening socket early in startup but
// may not respond to keyscan probes for another second.
// Under concurrent Docker load (the `go test ./...` shape
// where 8+ packages spin up containers in parallel), the
// pre-2026-05 budget of 8×500ms=4s wasn't enough — sshd
// took up to ~7s to be keyscan-ready and the test ran
// into a 4s wall on Docker Desktop.  Bumped to 30×500ms
// = 15s, which covers the observed worst case with margin
// while still failing fast on a truly-stuck sshd.
// waitSSHBanner dials the port-mapped sshd, reads up to one
// line, and confirms it starts with "SSH-" — RFC 4253 §4.2
// requires sshd to send "SSH-protoversion-softwareversion\r\n"
// as the very first bytes on a new connection.  This proves
// sshd is up AND in protocol-ready state, not just docker's
// port proxy accepting TCP while sshd is still generating
// host keys.
//
// Retry: per-attempt 2s read budget, with up to ~total wall
// clock.  On a fast warm host this resolves on the first
// attempt (<100ms); under docker-daemon contention it can
// take 20-30s for the container's startup script to finish
// writing /etc/ssh/ssh_host_* and exec sshd.
func (s *sftpRuntime) waitSSHBanner(ctx context.Context, total time.Duration) error {
	deadline := time.Now().Add(total)
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			line, err := bufio.NewReader(conn).ReadString('\n')
			_ = conn.Close()
			if err == nil && strings.HasPrefix(line, "SSH-") {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("ssh banner not received from %s within %s (sshd may still be generating host keys)",
		addr, total)
}

func (s *sftpRuntime) writeKnownHosts(ctx context.Context) error {
	if _, err := exec.LookPath("ssh-keyscan"); err != nil {
		return fmt.Errorf("ssh-keyscan not on PATH (install openssh-client): %w", err)
	}
	var (
		out     []byte
		lastErr error
	)
	const maxAttempts = 30
	for i := 0; i < maxAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// -T 5: 5-second per-host timeout (sshd is up;
		// this is a sanity bound).  -t = key types.
		// -p N: the port-mapped sshd.  127.0.0.1 host is
		// what the agent will connect to.
		cmd := exec.CommandContext(ctx, "ssh-keyscan",
			"-T", "5",
			"-t", "rsa,ecdsa,ed25519",
			"-p", fmt.Sprintf("%d", s.port),
			"127.0.0.1")
		var err error
		out, err = cmd.Output()
		if err == nil && len(out) > 0 {
			break
		}
		lastErr = err
		out = nil
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if len(out) == 0 {
		if lastErr == nil {
			lastErr = errors.New("returned no host keys")
		}
		return fmt.Errorf("ssh-keyscan after %d retries: %w", maxAttempts, lastErr)
	}
	dir, err := os.MkdirTemp("", "pg-hs-sftp-knownhosts-*")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return err
	}
	s.knownHosts = path
	return nil
}

// Down removes the container, deletes the per-instance
// known_hosts tempdir, and clears the recorded port.
// Idempotent.
func (s *sftpRuntime) Down(ctx context.Context) error {
	if s.container != "" {
		if s.scratchDir != "" {
			// The container wrote as uid 1001; the host user cannot
			// delete those files. Wipe from INSIDE (sshd runs as
			// root there) while the container still exists, so the
			// host-side RemoveAll below only meets an empty dir.
			_ = exec.CommandContext(ctx, "docker", "exec", s.container,
				"sh", "-c", "rm -rf /home/"+sftpUser+"/"+sftpDir+"/* /home/"+sftpUser+"/"+sftpDir+"/.[!.]*").Run()
		}
		_ = exec.CommandContext(ctx, "docker", "rm", "-fv", s.container).Run()
		s.container = ""
	}
	if s.scratchDir != "" {
		_ = os.RemoveAll(s.scratchDir)
		s.scratchDir = ""
	}
	if s.knownHosts != "" {
		// The dir we created is the parent of the
		// known_hosts file; remove it whole so the
		// per-test temp tree doesn't leak.
		_ = os.RemoveAll(filepath.Dir(s.knownHosts))
		s.knownHosts = ""
	}
	s.port = 0
	return nil
}

// URL embeds user:password directly per the sftp plugin's
// documented format.  The path is absolute (/upload would
// resolve against the SFTP server's root, which is /home/
// <user>/) — the plugin uses chroot-aware paths, so we
// reference the directory from the user's home.
func (s *sftpRuntime) URL() string {
	return fmt.Sprintf("sftp://%s:%s@127.0.0.1:%d/%s",
		sftpUser, sftpPass, s.port, sftpDir)
}

// EnvForAgent publishes the SFTP creds via env vars the
// plugin recognises as a fallback to cfg.Extras.  This is
// the only path the scenario runner has to ferry per-test
// connection details to the agent across a shell-out: the
// agent's CLI doesn't take SFTP-specific flags, and Extras
// isn't reachable from outside the agent process.
//
// In-process callers (the contract suite) skip this and
// pass the values via storage.StorageConfig.Extras
// directly; the plugin's resolution chain prefers Extras
// over env, so both paths end up at the same effective
// config.
func (s *sftpRuntime) EnvForAgent() map[string]string {
	if s.knownHosts == "" {
		return nil
	}
	return map[string]string{
		"PG_HARDSTORAGE_SFTP_KNOWN_HOSTS": s.knownHosts,
		"PG_HARDSTORAGE_SFTP_PASSWORD":    sftpPass,
	}
}

// ContainerName implements Runtime.
func (s *sftpRuntime) ContainerName() string { return s.container }

// Extras implements Runtime.  Publishes the per-instance
// known_hosts path so the SFTP plugin can verify the
// freshly-spawned container's host key.  Also publishes
// the password as Extras (the plugin accepts password
// either in the URL or via Extras; we pass via Extras to
// keep the URL form consistent with other sinks that
// don't embed creds).
func (s *sftpRuntime) Extras() map[string]string {
	if s.knownHosts == "" {
		return nil
	}
	return map[string]string{
		"known_hosts": s.knownHosts,
		"password":    sftpPass,
	}
}

// --- fixture image -------------------------------------------------
//
// The SFTP server is built locally from alpine rather than pulled
// pre-baked. The old fixture, atmoz/sftp:alpine-3.7, publishes an
// amd64-only manifest; on an arm64 host the container exited
// immediately with "exec /entrypoint: exec format error", so every
// sftp-backed test failed and the backend had no real-server coverage
// at all on that architecture. Building from a multi-arch base means
// the fixture exists wherever docker can build, which is the property
// a test fixture actually needs.
//
// The image is deliberately exec-LESS: `ForceCommand internal-sftp`
// plus a chroot. That preserves the distinction the ssh-exec fixture
// documents — scp needs a server that permits remote commands, sftp
// must be proven against one that does not — so this cannot silently
// become a second ssh-exec and let an scp-shaped bug pass as sftp
// coverage.

// sftpEntrypoint reproduces the atmoz/sftp user-spec contract that
// Up already speaks: `<user>:<pass>:[uid]:[gid]:[dir]`. Keeping the
// argument shape means the swap is an image change, not a rewrite of
// every caller.
//
// Host keys are generated HERE, at container start, not baked into the
// image at build time: instances share one image tag, and a shared
// host key would make the per-instance known_hosts file meaningless.
//
// The chroot layout is what OpenSSH demands and is easy to get wrong:
// ChrootDirectory and every component above it must be owned by root
// and not group- or world-writable, or sshd refuses the session with
// "bad ownership or modes for chroot directory". So /home/<user> stays
// root-owned 0755 and only the upload dir beneath it belongs to the
// user.
const sftpEntrypoint = `#!/bin/sh
set -eu

spec="${1:-}"
if [ -z "$spec" ]; then
	echo "usage: <user>:<pass>:[uid]:[gid]:[dir]" >&2
	exit 2
fi

user=$(printf '%s' "$spec" | cut -d: -f1)
pass=$(printf '%s' "$spec" | cut -d: -f2)
uid=$(printf '%s' "$spec" | cut -d: -f3)
gid=$(printf '%s' "$spec" | cut -d: -f4)
dir=$(printf '%s' "$spec" | cut -d: -f5)

[ -n "$user" ] || { echo "empty user in spec" >&2; exit 2; }
[ -n "$dir" ] || dir=upload

if [ -n "$gid" ]; then
	addgroup -g "$gid" "$user" 2>/dev/null || true
	grp="$user"
else
	grp=""
fi

if [ -n "$uid" ]; then
	adduser -D -H -u "$uid" ${grp:+-G "$grp"} -h "/home/$user" -s /sbin/nologin "$user"
else
	adduser -D -H -h "/home/$user" -s /sbin/nologin "$user"
fi

if [ -n "$pass" ]; then
	printf '%s:%s\n' "$user" "$pass" | chpasswd
else
	passwd -u "$user"
fi

# Chroot root: root-owned, not group/world writable (sshd requirement).
mkdir -p "/home/$user"
chown root:root "/home/$user"
chmod 0755 "/home/$user"

# The writable area beneath it. When PGHS_SINK_SCRATCH bind-mounts a
# host directory here, this chown is what makes it usable by the
# container's uid.
mkdir -p "/home/$user/$dir"
chown "$user" "/home/$user/$dir"
chmod 0755 "/home/$user/$dir"

# Per-instance host keys — see the comment on sftpEntrypoint.
ssh-keygen -A >/dev/null

exec /usr/sbin/sshd -D -e
`

// sftpSSHDConfig forbids everything except SFTP. internal-sftp is
// built into sshd, so the chrooted session needs no binaries, no
// libraries and no /dev inside the chroot.
const sftpSSHDConfig = `Port 22
PermitRootLogin no
PasswordAuthentication yes
KbdInteractiveAuthentication no
UsePAM no
AllowTcpForwarding no
X11Forwarding no
PrintMotd no
Subsystem sftp internal-sftp
ForceCommand internal-sftp
ChrootDirectory %h
`

// sftpDockerfile is the image definition. It is a function rather than
// a constant so a test can hash it without invoking docker.
func sftpDockerfile() string {
	return "FROM " + SinkImages["sftp"] + "\n" +
		"RUN apk add --no-cache openssh-server openssh-keygen shadow\n" +
		"COPY sshd_config /etc/ssh/sshd_config\n" +
		"COPY entrypoint.sh /usr/local/bin/entrypoint.sh\n" +
		"EXPOSE 22\n" +
		`ENTRYPOINT ["/bin/sh","/usr/local/bin/entrypoint.sh"]` + "\n"
}

// sftpImageTag derives the tag from the content that defines the
// image, so concurrent builds of identical content converge on one tag
// (full cache hit, no tag-repointing races between packages) while any
// edit here lands on a fresh tag instead of silently reusing a stale
// image. Every input file is hashed: the Dockerfile text alone would
// not change when only the entrypoint or sshd_config did.
func sftpImageTag(dockerfile, entrypoint, sshdConfig string) string {
	sum := sha256.Sum256([]byte(dockerfile + "\x00" + entrypoint + "\x00" + sshdConfig))
	return "pg-hardstorage-testkit-sftp:" + hex.EncodeToString(sum[:6])
}

// buildImage builds the chroot-sftp image and returns its tag.
func (s *sftpRuntime) buildImage(ctx context.Context) (string, error) {
	bctx, err := os.MkdirTemp("", "pg-hs-sftp-img-*")
	if err != nil {
		return "", fmt.Errorf("sftp sink: mkdir build ctx: %w", err)
	}
	defer func() { _ = os.RemoveAll(bctx) }()

	dockerfile := sftpDockerfile()
	for name, body := range map[string]string{
		"Dockerfile":    dockerfile,
		"entrypoint.sh": sftpEntrypoint,
		"sshd_config":   sftpSSHDConfig,
	} {
		if err := os.WriteFile(filepath.Join(bctx, name), []byte(body), 0o644); err != nil {
			return "", fmt.Errorf("sftp sink: write %s: %w", name, err)
		}
	}

	tag := sftpImageTag(dockerfile, sftpEntrypoint, sftpSSHDConfig)
	if out, err := exec.CommandContext(ctx, "docker", "build", "-q",
		"-t", tag, bctx).CombinedOutput(); err != nil {
		return "", fmt.Errorf("sftp sink: docker build: %w (%s)", err, truncate(out, 400))
	}
	return tag, nil
}
