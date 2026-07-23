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
// Same model as the sftp backend's hardlink path:
//
//  1. Stat the destination — if present, return ErrAlreadyExists
//     (cheap fast path only; NOT the correctness gate).
//  2. Write to "<dst>.hstmp-<rand>".
//  3. IfNotExists commit: `ln -T tmp dst` — link(2) fails with
//     EEXIST when dst is present, so exactly one concurrent
//     writer wins (single-winner consumers — the shared-DEK
//     object, the backup lease, audit-chain slots — depend on
//     this). Plain overwrite puts use `mv -T` (rename(2)),
//     which is last-writer-wins by design.
//
// Both `ln -T` and `mv -T` are atomic on every POSIX system when
// source + dest are on the same filesystem; manifest commits use
// RenameIfNotExists, which commits via the same `ln -T`.
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
		ConditionalPut: true, // atomic: ln -T (link(2) EEXIST) commits IfNotExists puts
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
	return p.resolvePrefix(key)
}

// resolvePrefix maps a repo-relative prefix to an absolute remote
// path. Unlike resolve it accepts the empty prefix, which maps to the
// repo root — List(ctx, "") must enumerate the whole repository
// (repo.Wipe relies on it). The ".."/absolute refusals still apply so
// a listing can never escape the root.
func (p *Plugin) resolvePrefix(prefix string) (string, error) {
	if strings.Contains(prefix, "..") {
		return "", fmt.Errorf("scp: key %q contains '..' (refused)", prefix)
	}
	if path.IsAbs(prefix) {
		return "", fmt.Errorf("scp: absolute key %q (refused)", prefix)
	}
	return path.Join(p.root, prefix), nil
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

	tmp := full + ".hstmp-" + randomSuffix()
	written, err := p.uploadVia(ctx, "cat > "+shellQuote(tmp), r)
	if err != nil {
		_, _ = p.runShell(ctx, "rm -f "+shellQuote(tmp))
		return storage.PutResult{}, fmt.Errorf("scp: write tmp %s: %w", tmp, err)
	}

	if opts.IfNotExists {
		// ATOMIC commit via link(2): `ln -T tmp full` fails with
		// EEXIST when full is already present, so exactly ONE of
		// several concurrent writers wins — the loser is told so.
		//
		// The previous stat → mv -T pattern was a TOCTOU: rename(2)
		// silently REPLACES an existing destination, so two writers
		// that both passed the stat re-check both "succeeded" and the
		// second overwrote the first. For content-addressed chunks
		// that is harmless, but for mutable single-key objects that
		// rely on single-winner semantics — the shared-DEK object
		// (issue #31's fix), the backup lease, audit-chain event
		// slots — it silently broke the contract this backend
		// advertises via ConditionalPut (concurrency audit).
		if _, lerr := p.runShell(ctx, "ln -T "+shellQuote(tmp)+" "+shellQuote(full)); lerr != nil {
			_, _ = p.runShell(ctx, "rm -f "+shellQuote(tmp))
			if strings.Contains(lerr.Error(), "File exists") {
				return storage.PutResult{}, storage.ErrAlreadyExists
			}
			return storage.PutResult{}, fmt.Errorf("scp: ln %s -> %s: %w", tmp, full, lerr)
		}
		_, _ = p.runShell(ctx, "rm -f "+shellQuote(tmp))
		return storage.PutResult{Key: key, Size: written}, nil
	}

	// `mv -T` (rename(2)) is atomic on the same filesystem — correct
	// for the last-writer-wins (IfNotExists=false) path only.
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
	// Test for existence first, then stat. If the file is absent the
	// remote command prints a sentinel and exits 0, so a genuine
	// "not found" is a nil-error empty-marker case. Only that maps to
	// ErrNotFound; any runShell error (SSH drop, exec failure, host
	// unreachable) now propagates instead of being masked as a
	// missing object — which would let a caller treat a transient
	// failure as proof the object doesn't exist.
	out, err := p.runShell(ctx, statCommand(full))
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("scp: stat %s: %w", full, err)
	}
	if strings.TrimSpace(out) == statNotFoundMarker || strings.TrimSpace(out) == "" {
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
		full, err := p.resolvePrefix(prefix)
		if err != nil {
			yield(storage.ObjectInfo{}, err)
			return
		}
		// %P is path relative to the find arg, %s is size,
		// %T@ is mtime as seconds.fractional.  TAB-separated
		// so paths with spaces parse cleanly.
		//
		// We must NOT collapse every find exit-1 to "empty list":
		// find also exits 1 on permission-denied / file-vanished
		// errors and may have emitted a PARTIAL listing on stdout
		// before failing. Silently returning would truncate the
		// listing — a caller wiping the repo would then leave the
		// unlisted tail behind. So we (a) guard the absent-root case
		// explicitly with a test-then-find (absent root => exit 0,
		// no output), and (b) keep find's stderr so a real failure
		// propagates, but only AFTER yielding whatever find produced.
		out, err := p.runShell(ctx, listCommand(full))
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
			// Hide scp-internal staging temps ("<key>.hstmp-<rand>",
			// written by Put between upload and mv -T). Caller keys
			// that merely contain ".tmp." (the repo layer's manifest
			// commit temps, which GC must be able to reap) are NOT
			// filtered — ".hstmp-" is reserved for backend staging.
			if strings.Contains(rel, ".hstmp-") {
				continue
			}
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
		// A find failure (permission denied, entry vanished mid-walk,
		// SSH drop) is surfaced only after emitting the partial
		// listing above, so the caller sees both what find found and
		// that the listing is incomplete — never a silent truncation.
		if err != nil {
			yield(storage.ObjectInfo{}, fmt.Errorf("scp: list %s: %w", full, err))
			return
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

// RenameIfNotExists is the manifest-commit primitive.  Commits via
// atomic link(2) (`ln -T` fails EEXIST) + unlink of src, so exactly
// one concurrent committer wins — the previous stat-then-`mv -T`
// pattern let a second committer silently replace the first
// (rename(2) overwrites an existing destination).
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
	// Cheap fast path — the atomic gate is the ln below.
	if exists, err := p.exists(ctx, dstFull); err != nil {
		return err
	} else if exists {
		return storage.ErrAlreadyExists
	}
	if _, lerr := p.runShell(ctx, "ln -T "+shellQuote(srcFull)+" "+shellQuote(dstFull)); lerr != nil {
		if strings.Contains(lerr.Error(), "File exists") {
			return storage.ErrAlreadyExists
		}
		return fmt.Errorf("scp: ln %s -> %s: %w", srcFull, dstFull, lerr)
	}
	_, _ = p.runShell(ctx, "rm -f "+shellQuote(srcFull))
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
	// runShell already runs the command through the remote login
	// shell (ssh exec), so the test expression goes there directly.
	// Wrapping it in an inner `sh -c '...'` would double-quote the
	// path: the inner single quotes emitted by shellQuote collide
	// with the single quotes delimiting the `sh -c` argument, so
	// the quoting cancels out — spaces break the check and
	// $()/backticks in a key would be executed by the outer shell.
	out, err := p.runShell(ctx, existsCommand(fullPath))
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

// statNotFoundMarker is printed by statCommand when the path is
// absent, so Stat can distinguish "the file isn't there" (nil error,
// this marker) from a transport/exec failure (non-nil runShell error).
const statNotFoundMarker = "__PGHS_NOTFOUND__"

// existsCommand builds the remote test-expression for exists(). The
// path is shellQuote'd exactly once and handed straight to the remote
// login shell (runShell exec's it) — NOT re-wrapped in an inner
// `sh -c '...'`, which would collide with shellQuote's single quotes
// and let $()/backticks in a key execute.
func existsCommand(fullPath string) string {
	return "[ -e " + shellQuote(fullPath) + " ] && echo y || echo n"
}

// statCommand builds the remote command for Stat. An absent path
// prints statNotFoundMarker and exits 0, so a genuine "not found" is a
// nil-error case and any runShell error is a real transport failure to
// propagate.
func statCommand(fullPath string) string {
	q := shellQuote(fullPath)
	return "if [ -e " + q + " ]; then stat -c '%s %Y' " + q +
		"; else echo " + statNotFoundMarker + "; fi"
}

// listCommand builds the remote command for List. An absent root exits
// 0 with no output (a legitimately empty listing); otherwise find runs
// with stderr intact so a permission/vanish error surfaces to the
// caller after the partial listing is emitted, never silently
// truncated.
func listCommand(fullPath string) string {
	q := shellQuote(fullPath)
	return "if [ ! -e " + q + " ]; then exit 0; fi; " +
		"find " + q + ` -type f -printf '%P\t%s\t%T@\n'`
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
