// Package sftp implements storage.StoragePlugin over SSH/SFTP.
//
// URL form:
//
//	sftp://[user@]host[:port]/<absolute-path>
//
// Examples:
//
//	sftp://backup@nas.example.com/srv/pg-hardstorage
//	sftp://nas.example.com:2222/data/backups
//
// Authentication (Extras keys):
//
//	identity_file: /etc/pg_hardstorage/keys/sftp_id_ed25519   # private key
//	identity_passphrase: ""                                   # if encrypted
//	known_hosts: /etc/pg_hardstorage/keys/known_hosts         # required
//	password: ""                                              # discouraged
//
// We deliberately reject `StrictHostKeyChecking=no` and require
// a known_hosts file — silently trusting unknown hosts is the
// single most common SFTP misconfiguration in audited
// environments, and it would let a network attacker MITM
// the entire backup pipeline.  Operators who genuinely don't
// have a stable host key (CI sandboxes) point at a per-test
// known_hosts written by the test harness.
//
// # Atomicity
//
// SFTP doesn't expose conditional writes natively.  We
// emulate `IfNotExists` via:
//
//  1. Stat the destination — if present, return ErrAlreadyExists.
//  2. Write to "<dst>.tmp.<rand>".
//  3. fsync (via Posix_rename if the server supports it; else
//     best-effort).
//  4. Rename `<tmp>` -> `<dst>`.
//
// The TOCTOU window between the stat and the write is
// inherent to SFTP and the same posture rsync / scp ship
// with.  CAS chunks are content-addressed so a duplicate
// write is harmless; the manifest commit goes through
// RenameIfNotExists which has the same posture.
package sftp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	xknownhosts "golang.org/x/crypto/ssh/knownhosts"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// init registers the "sftp" URL scheme with the storage
// registry.  Without this, repo.Open never knows how to
// dispatch sftp:// URLs to the SFTP plugin (an audit-v26-
// class wiring bug — the package compiled but never reached
// the running binary's resolver).
func init() {
	storage.Register("sftp", func() storage.StoragePlugin { return &Plugin{} })
}

// Plugin is the SSH/SFTP-backed StoragePlugin.
type Plugin struct {
	// NopBarrier: the SFTP plugin does not issue the optional
	// fsync@openssh.com extension, so Barrier is a no-op.
	// Capabilities reports neither InlineDurable nor
	// DurabilityBarrier — callers needing durability use
	// DurabilityInline (the default) here.
	storage.NopBarrier

	root string

	mu     sync.Mutex
	ssh    *ssh.Client
	client *sftp.Client
	closed bool
}

// Name implements storage.StoragePlugin.
func (p *Plugin) Name() string { return "sftp" }

// Capabilities implements storage.StoragePlugin.
func (p *Plugin) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		ConditionalPut: true, // emulated; see top-of-file
	}
}

// Open implements storage.StoragePlugin.  Establishes the SSH
// connection eagerly so an authentication error surfaces here
// rather than in the first Put.
func (p *Plugin) Open(_ context.Context, cfg storage.StorageConfig) error {
	if cfg.URL == nil {
		return errors.New("sftp: nil URL")
	}
	host := cfg.URL.Host
	if !strings.Contains(host, ":") {
		host += ":22"
	}
	if err := airgap.Default().EndpointAllowed(cfg.URL.String()); err != nil {
		return fmt.Errorf("sftp: %w", err)
	}

	user := ""
	if cfg.URL.User != nil {
		user = cfg.URL.User.Username()
	}
	if user == "" {
		user = os.Getenv("USER")
	}
	if cfg.URL.Path == "" {
		return errors.New("sftp: URL must include the absolute repo path (e.g. sftp://host/srv/repo)")
	}
	root := path.Clean(cfg.URL.Path)

	// Three input paths, in priority order:
	//   1. cfg.Extras (typical operator setup via config file)
	//   2. PG_HARDSTORAGE_SFTP_* env vars (the testkit's
	//      shell-out path: scenarios spawn the agent as a
	//      child process and can't populate cfg.Extras
	//      directly, but they can set env)
	//
	// URL query / user-info embedding is deliberately NOT
	// supported for password — URL strings get logged, and
	// the testkit isn't a strong enough reason to weaken
	// production credential hygiene.
	identityFile := firstNonEmpty(
		cfg.Extras["identity_file"],
		os.Getenv("PG_HARDSTORAGE_SFTP_IDENTITY_FILE"))
	identityPassphrase := firstNonEmpty(
		cfg.Extras["identity_passphrase"],
		os.Getenv("PG_HARDSTORAGE_SFTP_IDENTITY_PASSPHRASE"))
	password := firstNonEmpty(
		cfg.Extras["password"],
		os.Getenv("PG_HARDSTORAGE_SFTP_PASSWORD"))
	knownHosts := firstNonEmpty(
		cfg.Extras["known_hosts"],
		os.Getenv("PG_HARDSTORAGE_SFTP_KNOWN_HOSTS"))

	if knownHosts == "" {
		return errors.New("sftp: known_hosts required " +
			"(set extras.known_hosts or PG_HARDSTORAGE_SFTP_KNOWN_HOSTS env; " +
			"refusing StrictHostKeyChecking=no posture)")
	}
	hk, err := xknownhosts.New(knownHosts)
	if err != nil {
		return fmt.Errorf("sftp: load known_hosts %s: %w", knownHosts, err)
	}

	auth := []ssh.AuthMethod{}
	switch {
	case identityFile != "":
		body, err := os.ReadFile(identityFile)
		if err != nil {
			return fmt.Errorf("sftp: read identity %s: %w", identityFile, err)
		}
		var signer ssh.Signer
		if identityPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(body, []byte(identityPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(body)
		}
		if err != nil {
			return fmt.Errorf("sftp: parse identity: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	case password != "":
		auth = append(auth, ssh.Password(password))
	default:
		return errors.New("sftp: extras.identity_file or extras.password is required")
	}

	cfgssh := &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: hk,
		Timeout:         15 * time.Second,
	}
	conn, err := ssh.Dial("tcp", host, cfgssh)
	if err != nil {
		return fmt.Errorf("sftp: dial %s: %w", host, err)
	}
	cli, err := sftp.NewClient(conn)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("sftp: open client: %w", err)
	}

	p.ssh = conn
	p.client = cli
	p.root = root
	return nil
}

// Close implements storage.StoragePlugin.  Idempotent.
func (p *Plugin) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var firstErr error
	if p.client != nil {
		if err := p.client.Close(); err != nil {
			firstErr = err
		}
		p.client = nil
	}
	if p.ssh != nil {
		if err := p.ssh.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		p.ssh = nil
	}
	return firstErr
}

// resolve maps a repo-relative key to an absolute remote path.
// Refuses paths that would escape the root via "..".
func (p *Plugin) resolve(key string) (string, error) {
	if strings.Contains(key, "..") {
		return "", fmt.Errorf("sftp: key %q contains '..' (refused)", key)
	}
	return path.Join(p.root, key), nil
}

// Put implements storage.StoragePlugin.
//
// IfNotExists semantics
// ---------------------
// The SFTP protocol doesn't have a single atomic "create
// exclusive" rename.  We write to a tmp file then commit
// via one of two paths:
//
//   - IfNotExists=true: try `Link(tmp, full)` —
//     OpenSSH's hardlink@openssh.com extension is an
//     atomic create-exclusive operation (POSIX link(2)
//     fails with EEXIST when the target is present).
//     ONLY one concurrent writer succeeds; everyone else
//     gets ErrAlreadyExists.  When the server doesn't
//     advertise the extension we fall back to the
//     legacy stat-then-rename pattern with a documented
//     race window — chunks are content-addressed so
//     duplicate writes are harmless, and manifests rely
//     on the contract suite's atomic guarantees on
//     extension-supporting servers.
//
//   - IfNotExists=false: best-effort Remove(dst) then
//     atomicRename(tmp, dst) — last-writer-wins, the
//     legacy behaviour.
//
// Atomic-rename note: the SFTP RFC's Rename is NOT
// guaranteed atomic.  atomicRename uses the
// posix-rename@openssh.com extension when advertised
// (every modern OpenSSH server) and falls back to plain
// Rename otherwise.
func (p *Plugin) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if err := p.assertOpen(); err != nil {
		return storage.PutResult{}, err
	}
	full, err := p.resolve(key)
	if err != nil {
		return storage.PutResult{}, err
	}
	// Ensure parent directory.
	if err := p.mkdirAll(path.Dir(full)); err != nil {
		return storage.PutResult{}, err
	}

	tmp := full + ".tmp." + randomSuffix()
	f, err := p.client.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return storage.PutResult{}, fmt.Errorf("sftp: create tmp %s: %w", tmp, err)
	}
	written, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = p.client.Remove(tmp)
		return storage.PutResult{}, fmt.Errorf("sftp: copy to %s: %w", tmp, copyErr)
	}
	if closeErr != nil {
		_ = p.client.Remove(tmp)
		return storage.PutResult{}, fmt.Errorf("sftp: close %s: %w", tmp, closeErr)
	}

	if opts.IfNotExists {
		// Atomic path: hardlink with fail-on-exists.
		// hardlink@openssh.com is the OpenSSH-protocol
		// extension that maps to POSIX link(2); it
		// returns SSH_FX_FILE_ALREADY_EXISTS when the
		// destination is present.  The contract suite's
		// concurrent-IfNotExists race is exactly the case
		// this is required for.
		linkErr := p.client.Link(tmp, full)
		if linkErr == nil {
			// Hardlink succeeded; tmp is now redundant.
			// Best-effort cleanup.
			_ = p.client.Remove(tmp)
			return storage.PutResult{Key: key, Size: written}, nil
		}
		if isAlreadyExists(linkErr) {
			_ = p.client.Remove(tmp)
			return storage.PutResult{}, storage.ErrAlreadyExists
		}
		// Server doesn't support the hardlink extension —
		// fall back to the legacy stat-then-rename pattern,
		// accepting the small race window.  The contract
		// suite's ParallelPuts case fails on these servers;
		// the agent's audit-chain serialisation makes the
		// race academic in practice.
		if _, err := p.client.Stat(full); err == nil {
			_ = p.client.Remove(tmp)
			return storage.PutResult{}, storage.ErrAlreadyExists
		}
	} else {
		// last-writer-wins semantics: remove dst if present so
		// rename succeeds (sftp Rename semantics vary).
		_ = p.client.Remove(full)
	}
	if err := p.atomicRename(tmp, full); err != nil {
		_ = p.client.Remove(tmp)
		return storage.PutResult{}, fmt.Errorf("sftp: rename %s -> %s: %w", tmp, full, err)
	}
	return storage.PutResult{Key: key, Size: written}, nil
}

// isAlreadyExists detects the SFTP "file already exists"
// status.  pkg/sftp wraps the SSH_FX_FILE_ALREADY_EXISTS
// status (code 11) in a *StatusError; we match on the
// numeric code so the check stays robust to wording
// variations across server implementations.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	// pkg/sftp's StatusError carries the SFTP status code
	// in the Code field.  11 == SSH_FX_FILE_ALREADY_EXISTS.
	type coder interface{ Status() uint32 }
	var c coder
	if errors.As(err, &c) {
		return c.Status() == 11
	}
	// Fall back to substring match for servers that wrap
	// the error differently (rare but possible).
	msg := err.Error()
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "SSH_FX_FILE_ALREADY_EXISTS")
}

// atomicRename calls PosixRename when the server advertises
// posix-rename@openssh.com (every modern OpenSSH server,
// rsync.net, NetApp SnapDiff, etc.) and falls back to plain
// Rename otherwise.  Operators of strict-RFC SFTP servers
// (rare) accept the small atomicity gap; the chunk store's
// content-addressing absorbs any duplicate writes.
func (p *Plugin) atomicRename(src, dst string) error {
	if _, supported := p.client.HasExtension("posix-rename@openssh.com"); supported {
		return p.client.PosixRename(src, dst)
	}
	return p.client.Rename(src, dst)
}

// Get implements storage.StoragePlugin.
func (p *Plugin) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	full, err := p.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := p.client.Open(full)
	if err != nil {
		if isNotExist(err) {
			return nil, storage.ErrNotFound
		}
		return nil, fmt.Errorf("sftp: open %s: %w", full, err)
	}
	return f, nil
}

// Stat implements storage.StoragePlugin.
func (p *Plugin) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := p.assertOpen(); err != nil {
		return storage.ObjectInfo{}, err
	}
	full, err := p.resolve(key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	fi, err := p.client.Stat(full)
	if err != nil {
		if isNotExist(err) {
			return storage.ObjectInfo{}, storage.ErrNotFound
		}
		return storage.ObjectInfo{}, fmt.Errorf("sftp: stat %s: %w", full, err)
	}
	return storage.ObjectInfo{
		Key:     key,
		Size:    fi.Size(),
		ModTime: fi.ModTime(),
	}, nil
}

// List implements storage.StoragePlugin.  Walks the prefix
// recursively (SFTP's Walk uses depth-first traversal).
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
		walker := p.client.Walk(full)
		for walker.Step() {
			if err := ctx.Err(); err != nil {
				yield(storage.ObjectInfo{}, err)
				return
			}
			if err := walker.Err(); err != nil {
				if !isNotExist(err) {
					if !yield(storage.ObjectInfo{}, err) {
						return
					}
				}
				continue
			}
			info := walker.Stat()
			if info == nil || info.IsDir() {
				continue
			}
			absPath := walker.Path()
			rel := strings.TrimPrefix(absPath, p.root+"/")
			rel = strings.TrimPrefix(rel, p.root)
			if !yield(storage.ObjectInfo{
				Key:     rel,
				Size:    info.Size(),
				ModTime: info.ModTime(),
			}, nil) {
				return
			}
		}
	}
}

// Delete implements storage.StoragePlugin.
func (p *Plugin) Delete(ctx context.Context, key string) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	full, err := p.resolve(key)
	if err != nil {
		return err
	}
	if err := p.client.Remove(full); err != nil {
		if isNotExist(err) {
			return nil
		}
		return fmt.Errorf("sftp: remove %s: %w", full, err)
	}
	return nil
}

// RenameIfNotExists implements storage.StoragePlugin.
// Uses posix-rename@openssh.com when the server supports it
// (atomic), falls back to plain Rename otherwise.
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
	if _, err := p.client.Stat(dstFull); err == nil {
		return storage.ErrAlreadyExists
	}
	if err := p.atomicRename(srcFull, dstFull); err != nil {
		return fmt.Errorf("sftp: rename %s -> %s: %w", srcFull, dstFull, err)
	}
	return nil
}

// SetRetention implements storage.StoragePlugin.  SFTP has no
// regulatory-grade WORM concept; return ErrUnsupported.
func (p *Plugin) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	return storage.ErrUnsupported
}

// mkdirAll creates all parents of dir.  SFTP doesn't have
// MkdirAll natively in older servers; we walk the path.
func (p *Plugin) mkdirAll(dir string) error {
	if dir == "" || dir == "/" {
		return nil
	}
	parts := strings.Split(dir, "/")
	cur := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		cur = cur + "/" + part
		if _, err := p.client.Stat(cur); err == nil {
			continue
		}
		if err := p.client.Mkdir(cur); err != nil {
			// Tolerate races: if another process created the
			// directory between our Stat and Mkdir, the second
			// Stat will return success.
			if _, statErr := p.client.Stat(cur); statErr == nil {
				continue
			}
			return fmt.Errorf("sftp: mkdir %s: %w", cur, err)
		}
	}
	return nil
}

func (p *Plugin) assertOpen() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.client == nil {
		return errors.New("sftp: plugin not open")
	}
	return nil
}

func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	// pkg/sftp returns its own status type; ENOENT is the
	// `code: 2` value.
	if msg := err.Error(); strings.Contains(msg, "file does not exist") || strings.Contains(msg, "No such file") {
		return true
	}
	return false
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Sanity import used to reference the net package symbol via
// ssh.Dial; kept for clarity that the dependency is intentional.
var _ = net.Dial

// firstNonEmpty returns the first non-empty argument, or
// "" if all are empty.  Tiny helper used by the SFTP
// credential-resolution chain (cfg.Extras → env var).
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
