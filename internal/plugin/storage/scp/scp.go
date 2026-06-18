// Package scp implements storage.StoragePlugin over SSH using
// shell-mediated transfer — the way `scp` and `paramiko-scp`
// actually move bytes.
//
// URL form:
//
//	scp://[user@]host[:port]/<absolute-path>
//
// Examples:
//
//	scp://backup@nas.example.com/srv/pg-hardstorage
//	scp://nas.example.com:2222/data/backups
//
// Authentication (same Extras keys as the sftp plugin):
//
//	identity_file: /etc/pg_hardstorage/keys/scp_id_ed25519     # private key
//	identity_passphrase: ""                                    # if encrypted
//	known_hosts: /etc/pg_hardstorage/keys/known_hosts          # required
//	password: ""                                               # discouraged
//
// # Why a separate scp:// backend when sftp:// exists?
//
// Some hardened SSH deployments disable the SFTP subsystem
// (`Subsystem sftp` commented out in sshd_config) but still
// permit ssh-exec; some embedded / appliance SSH servers don't
// implement SFTP at all.  The scp:// backend talks to those
// servers using the same set of remote-shell primitives `scp`
// itself uses — `cat`, `stat`, `find`, `mv`, `rm`, `mkdir`.  No
// SFTP subsystem required.
//
// # What this backend is NOT
//
// We do not implement the legacy SCP wire protocol (the binary
// `C0644 size name` framing).  The scp wire format has a
// well-documented security history (CVE-2018-20685,
// CVE-2019-6111, CVE-2019-6109) and is being deprecated by
// OpenSSH itself in favour of SFTP.  The `scp` shipped on
// modern systems already uses SFTP under the hood by default.
//
// Instead, this plugin uses ssh.Session exec with stdin /
// stdout streaming for data (`cat > path` / `cat path`) and
// shell commands for filesystem ops.  That posture is what
// `paramiko-scp`, Ansible's `synchronize` module, and ad-hoc
// rsync wrappers all use.  It works against any SSH server
// that allows command execution.
//
// # Atomicity
//
// Same model as the sftp backend:
//
//  1. Stat the destination — if present, return ErrAlreadyExists.
//  2. Write to "<dst>.tmp.<rand>".
//  3. mv -T tmp dst (atomic via rename(2) on the same fs).
//
// `mv -T` is atomic on every POSIX system when source + dest are
// on the same filesystem — the standard guarantee for our
// content-addressed chunk store.  CAS chunks are content-keyed
// so a duplicate write is harmless; manifest commits use
// RenameIfNotExists which has the same posture.
package scp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	xknownhosts "golang.org/x/crypto/ssh/knownhosts"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// init registers the "scp" URL scheme.
func init() {
	storage.Register("scp", func() storage.StoragePlugin { return &Plugin{} })
}

// Plugin is the SSH-exec-backed StoragePlugin.
type Plugin struct {
	// NopBarrier: SCP has no remote-fsync mechanism, so Barrier is
	// a no-op. Capabilities reports neither InlineDurable nor
	// DurabilityBarrier — callers needing durability use
	// DurabilityInline (the default) here.
	storage.NopBarrier

	root string

	mu     sync.Mutex
	ssh    *ssh.Client
	closed bool
}

// Name implements storage.StoragePlugin.
func (p *Plugin) Name() string { return "scp" }

// Capabilities implements storage.StoragePlugin.
func (p *Plugin) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		ConditionalPut: true, // emulated; see top-of-file
	}
}

// Open establishes the SSH connection eagerly so an
// authentication error surfaces here rather than in the first
// Put.  Auth + known_hosts handling deliberately mirrors the
// sftp plugin so an operator switching backends only changes
// the URL scheme.
func (p *Plugin) Open(_ context.Context, cfg storage.StorageConfig) error {
	if cfg.URL == nil {
		return errors.New("scp: nil URL")
	}
	host := cfg.URL.Host
	if !strings.Contains(host, ":") {
		host += ":22"
	}
	if err := airgap.Default().EndpointAllowed(cfg.URL.String()); err != nil {
		return fmt.Errorf("scp: %w", err)
	}

	user := ""
	if cfg.URL.User != nil {
		user = cfg.URL.User.Username()
	}
	if user == "" {
		user = os.Getenv("USER")
	}
	if cfg.URL.Path == "" {
		return errors.New("scp: URL must include the absolute repo path (e.g. scp://host/srv/repo)")
	}
	root := path.Clean(cfg.URL.Path)

	identityFile := cfg.Extras["identity_file"]
	identityPassphrase := cfg.Extras["identity_passphrase"]
	password := cfg.Extras["password"]
	knownHosts := cfg.Extras["known_hosts"]

	if knownHosts == "" {
		return errors.New("scp: extras.known_hosts is required (refusing StrictHostKeyChecking=no posture)")
	}
	hk, err := xknownhosts.New(knownHosts)
	if err != nil {
		return fmt.Errorf("scp: load known_hosts %s: %w", knownHosts, err)
	}

	auth := []ssh.AuthMethod{}
	switch {
	case identityFile != "":
		body, err := os.ReadFile(identityFile)
		if err != nil {
			return fmt.Errorf("scp: read identity %s: %w", identityFile, err)
		}
		var signer ssh.Signer
		if identityPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(body, []byte(identityPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(body)
		}
		if err != nil {
			return fmt.Errorf("scp: parse identity: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	case password != "":
		auth = append(auth, ssh.Password(password))
	default:
		return errors.New("scp: extras.identity_file or extras.password is required")
	}

	cfgssh := &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: hk,
		Timeout:         15 * time.Second,
	}
	conn, err := ssh.Dial("tcp", host, cfgssh)
	if err != nil {
		return fmt.Errorf("scp: dial %s: %w", host, err)
	}
	p.ssh = conn
	p.root = root
	return nil
}

// Close idempotently tears down the SSH client.
func (p *Plugin) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.ssh != nil {
		err := p.ssh.Close()
		p.ssh = nil
		return err
	}
	return nil
}

// resolve maps a repo-relative key to an absolute remote path.
// Refuses paths that would escape the root via "..".
func (p *Plugin) resolve(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("scp: empty key (refused)")
	}
	if strings.Contains(key, "..") {
		return "", fmt.Errorf("scp: key %q contains '..' (refused)", key)
	}
	if path.IsAbs(key) {
		return "", fmt.Errorf("scp: absolute key %q (refused)", key)
	}
	return path.Join(p.root, key), nil
}

// Put writes r to key.  Atomicity model mirrors the sftp plugin:
// stat → tmp file → mv -T.
func (p *Plugin) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if err := p.assertOpen(); err != nil {
		return storage.PutResult{}, err
	}
	full, err := p.resolve(key)
	if err != nil {
		return storage.PutResult{}, err
	}
	if opts.IfNotExists {
		if exists, err := p.exists(ctx, full); err != nil {
			return storage.PutResult{}, err
		} else if exists {
			return storage.PutResult{}, storage.ErrAlreadyExists
		}
	}

	if err := p.mkdirAll(ctx, path.Dir(full)); err != nil {
		return storage.PutResult{}, err
	}

	tmp := full + ".tmp." + randomSuffix()
	written, err := p.uploadVia(ctx, "cat > "+shellQuote(tmp), r)
	if err != nil {
		_, _ = p.runShell(ctx, "rm -f "+shellQuote(tmp))
		return storage.PutResult{}, fmt.Errorf("scp: write tmp %s: %w", tmp, err)
	}

	if opts.IfNotExists {
		// Re-check race window: stat may have raced; if dst
		// exists, drop the tmp and surface ErrAlreadyExists.
		if exists, err := p.exists(ctx, full); err != nil {
			_, _ = p.runShell(ctx, "rm -f "+shellQuote(tmp))
			return storage.PutResult{}, err
		} else if exists {
			_, _ = p.runShell(ctx, "rm -f "+shellQuote(tmp))
			return storage.PutResult{}, storage.ErrAlreadyExists
		}
	}

	// `mv -T` (rename(2)) is atomic on the same filesystem.
	// We rely on this for both same-fs writes (the common case
	// when the repo is one mount) and refuse to operate across
	// filesystems silently — the rm-of-tmp on error covers it.
	if _, err := p.runShell(ctx, "mv -T "+shellQuote(tmp)+" "+shellQuote(full)); err != nil {
		_, _ = p.runShell(ctx, "rm -f "+shellQuote(tmp))
		return storage.PutResult{}, fmt.Errorf("scp: mv %s -> %s: %w", tmp, full, err)
	}
	return storage.PutResult{Key: key, Size: written}, nil
}

// Get streams the contents of key back to the caller via
// `cat <path>`.  The returned ReadCloser is the SSH session's
// stdout pipe; closing it tears down the session.
func (p *Plugin) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	full, err := p.resolve(key)
	if err != nil {
		return nil, err
	}
	if exists, err := p.exists(ctx, full); err != nil {
		return nil, err
	} else if !exists {
		return nil, storage.ErrNotFound
	}
	return p.streamRead(ctx, "cat "+shellQuote(full))
}

// Stat issues `stat -c '%s %Y' <path>` and parses size + mtime.
func (p *Plugin) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := p.assertOpen(); err != nil {
		return storage.ObjectInfo{}, err
	}
	full, err := p.resolve(key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	out, err := p.runShell(ctx, "stat -c '%s %Y' "+shellQuote(full)+" 2>/dev/null")
	if err != nil || strings.TrimSpace(out) == "" {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	parts := strings.Fields(strings.TrimSpace(out))
	if len(parts) != 2 {
		return storage.ObjectInfo{}, fmt.Errorf("scp: stat parse %q: want size mtime, got %d fields",
			out, len(parts))
	}
	size, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("scp: stat parse size %q: %w", parts[0], err)
	}
	mtime, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("scp: stat parse mtime %q: %w", parts[1], err)
	}
	return storage.ObjectInfo{
		Key:     key,
		Size:    size,
		ModTime: time.Unix(mtime, 0).UTC(),
	}, nil
}

// List walks prefix recursively via
// `find <root> -type f -printf '%P\t%s\t%T@\n'`.  Each line is
// one file: relative path, size, mtime-as-float.
func (p *Plugin) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {
		if err := p.assertOpen(); err != nil {
			yield(storage.ObjectInfo{}, err)
			return
		}
		full, err := p.resolve(prefix)
		if err != nil {
			yield(storage.ObjectInfo{}, err)
			return
		}
		// %P is path relative to the find arg, %s is size,
		// %T@ is mtime as seconds.fractional.  TAB-separated
		// so paths with spaces parse cleanly.
		out, err := p.runShell(ctx,
			"find "+shellQuote(full)+
				` -type f -printf '%P\t%s\t%T@\n' 2>/dev/null`)
		if err != nil {
			// find returns 1 when the root doesn't exist;
			// we treat "no entries" as "empty list".
			if strings.Contains(err.Error(), "exit status 1") {
				return
			}
			yield(storage.ObjectInfo{}, err)
			return
		}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) != 3 {
				continue
			}
			rel := parts[0]
			size, _ := strconv.ParseInt(parts[1], 10, 64)
			mtimeFloat, _ := strconv.ParseFloat(parts[2], 64)
			mtime := time.Unix(int64(mtimeFloat), 0).UTC()
			// Compose key from prefix + relative path so the
			// caller sees keys in the same shape S3 / FS use.
			key := path.Join(prefix, rel)
			if !yield(storage.ObjectInfo{
				Key:     strings.TrimPrefix(key, "/"),
				Size:    size,
				ModTime: mtime,
			}, nil) {
				return
			}
		}
	}
}

// Delete removes a single object.  Idempotent on absent.
func (p *Plugin) Delete(ctx context.Context, key string) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	_, err = p.runShell(ctx, "rm -f "+shellQuote(full))
	if err != nil {
		return fmt.Errorf("scp: rm %s: %w", full, err)
	}
	return nil
}

// RenameIfNotExists is the manifest-commit primitive.  Uses the
// same TOCTOU-aware stat-then-rename pattern as the sftp
// backend; the chunk store's content-addressing absorbs any
// duplicate writes.
func (p *Plugin) RenameIfNotExists(ctx context.Context, src, dst string) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	srcFull, err := p.resolve(src)
	if err != nil {
		return err
	}
	dstFull, err := p.resolve(dst)
	if err != nil {
		return err
	}
	if exists, err := p.exists(ctx, dstFull); err != nil {
		return err
	} else if exists {
		return storage.ErrAlreadyExists
	}
	if _, err := p.runShell(ctx, "mv -T "+shellQuote(srcFull)+" "+shellQuote(dstFull)); err != nil {
		return fmt.Errorf("scp: mv %s -> %s: %w", srcFull, dstFull, err)
	}
	return nil
}

// SetRetention is unsupported on plain SSH-exec — there's no
// regulatory-grade WORM concept to surface.
func (p *Plugin) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	return storage.ErrUnsupported
}

// --- ssh exec helpers -------------------------------------------------

// runShell executes a single shell command on the remote host
// and returns combined stdout (stderr lands in the error).
// Each call opens its own ssh.Session — short-lived, easy to
// reason about, and cleanly cancellable via ctx.
//
// Keep the command line as the sole argument; never interpolate
// user-controlled keys without `shellQuote()`.
func (p *Plugin) runShell(ctx context.Context, command string) (string, error) {
	sess, err := p.ssh.NewSession()
	if err != nil {
		return "", fmt.Errorf("scp: new session: %w", err)
	}
	defer sess.Close()
	var out, errBuf bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &errBuf
	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()
	select {
	case err := <-done:
		if err != nil {
			return out.String(), fmt.Errorf("%w (stderr: %s)", err,
				strings.TrimSpace(errBuf.String()))
		}
		return out.String(), nil
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		return "", ctx.Err()
	}
}

// uploadVia opens a session for command (typically `cat > path`)
// and streams r to its stdin.  Returns the byte count.
func (p *Plugin) uploadVia(ctx context.Context, command string, r io.Reader) (int64, error) {
	sess, err := p.ssh.NewSession()
	if err != nil {
		return 0, fmt.Errorf("scp: new session: %w", err)
	}
	defer sess.Close()
	stdin, err := sess.StdinPipe()
	if err != nil {
		return 0, fmt.Errorf("scp: stdin pipe: %w", err)
	}
	var errBuf bytes.Buffer
	sess.Stderr = &errBuf

	if err := sess.Start(command); err != nil {
		return 0, fmt.Errorf("scp: start command: %w", err)
	}

	type result struct {
		written int64
		err     error
	}
	res := make(chan result, 1)
	go func() {
		n, copyErr := io.Copy(stdin, r)
		closeErr := stdin.Close()
		if copyErr != nil {
			res <- result{written: n, err: copyErr}
			return
		}
		res <- result{written: n, err: closeErr}
	}()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		return 0, ctx.Err()
	case r := <-res:
		if r.err != nil {
			return r.written, r.err
		}
		// Wait for the remote command to complete so we can
		// surface its exit status.
		if err := sess.Wait(); err != nil {
			return r.written, fmt.Errorf("%w (stderr: %s)", err,
				strings.TrimSpace(errBuf.String()))
		}
		return r.written, nil
	}
}

// streamRead opens a session for command (typically `cat path`)
// and returns a ReadCloser that drains stdout.  Closing the
// reader closes the session.
func (p *Plugin) streamRead(ctx context.Context, command string) (io.ReadCloser, error) {
	sess, err := p.ssh.NewSession()
	if err != nil {
		return nil, fmt.Errorf("scp: new session: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("scp: stdout pipe: %w", err)
	}
	if err := sess.Start(command); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("scp: start command: %w", err)
	}
	return &sessionReader{sess: sess, stdout: stdout, ctx: ctx}, nil
}

// sessionReader pairs an ssh.Session with its stdout pipe so
// closing the reader tears down the session.
type sessionReader struct {
	sess   *ssh.Session
	stdout io.Reader
	ctx    context.Context
}

// Read implements io.Reader. Returns the context's error before
// reading so a cancellation surfaces promptly rather than waiting
// for the SSH stream to time out.
func (s *sessionReader) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	return s.stdout.Read(p)
}

// Close implements io.Closer. Waits on the remote command first so
// the exit status surfaces, then tears down the SSH session.
func (s *sessionReader) Close() error {
	// Wait first so we surface the remote exit status; ignore
	// EOF-after-Wait paths.
	werr := s.sess.Wait()
	cerr := s.sess.Close()
	if werr != nil && werr != io.EOF {
		// Squelch the noisy "Process exited with status 0"
		// path some implementations report.
		if !strings.Contains(werr.Error(), "status 0") {
			return werr
		}
	}
	return cerr
}

// exists returns true when path is a present file or directory.
// Used by Put / RenameIfNotExists for the IfNotExists check.
func (p *Plugin) exists(ctx context.Context, fullPath string) (bool, error) {
	out, err := p.runShell(ctx,
		"sh -c '[ -e "+shellQuote(fullPath)+" ] && echo y || echo n'")
	if err != nil {
		return false, fmt.Errorf("scp: exists check %s: %w", fullPath, err)
	}
	switch strings.TrimSpace(out) {
	case "y":
		return true, nil
	case "n":
		return false, nil
	}
	return false, fmt.Errorf("scp: exists check %s: unexpected output %q", fullPath, out)
}

// mkdirAll creates dir + every parent.  Pure shell `mkdir -p`
// is atomic against concurrent creators on POSIX.
func (p *Plugin) mkdirAll(ctx context.Context, dir string) error {
	if dir == "" || dir == "/" {
		return nil
	}
	if _, err := p.runShell(ctx, "mkdir -p "+shellQuote(dir)); err != nil {
		return fmt.Errorf("scp: mkdir -p %s: %w", dir, err)
	}
	return nil
}

func (p *Plugin) assertOpen() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.ssh == nil {
		return errors.New("scp: plugin not open")
	}
	return nil
}

// shellQuote wraps s in single quotes, escaping any embedded
// single quotes via the standard `'\”` trick.  This is the
// only shell-injection defence between us and the remote
// command line; every key the caller hands us must pass
// through here.
//
// Why single quotes (not double): inside single quotes the
// shell does no expansion at all — no $vars, no backticks, no
// glob.  The escape trick `'\”` closes the literal, emits a
// single literal quote, then reopens.  POSIX-portable.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
