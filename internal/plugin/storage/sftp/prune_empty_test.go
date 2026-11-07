//go:build !windows

package sftp

// prune_empty_test.go — PruneEmptyDirs and the writer-side retries that
// make it safe, against pkg/sftp's server on the REAL local filesystem
// (no SSH, no docker).
//
// The in-memory request server behind pipePlugin will not do here: it
// creates a file in a directory that does not exist, so it cannot show
// the one failure these tests are about. The local server maps the
// kernel's ENOENT / ENOTEMPTY to SFTP statuses the way OpenSSH does.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gosftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
)

// localFSPlugin returns a Plugin talking over pipes to an SFTP server
// that serves the local filesystem, rooted at a fresh directory (which
// does not exist yet, like a repo before its first Put).
func localFSPlugin(t *testing.T) (*Plugin, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	srv, err := gosftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{c2sR, s2cW})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	cli, err := gosftp.NewClientPipe(s2cR, c2sW)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Server side first: the client's Close waits for its reader
		// to see EOF on the server-to-client pipe.
		_ = srv.Close()
		_ = s2cW.Close()
		_ = c2sR.Close()
		_ = cli.Close()
	})
	return &Plugin{root: root, client: cli, cfgssh: &ssh.ClientConfig{}}, root
}

func TestPruneEmptyDirs_Contract(t *testing.T) {
	p, root := localFSPlugin(t)
	contract.PruneEmptyDirs(t, p)
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("repo root after pruning it: %v", err)
	}
}

// Writers racing the prune must never fail, or lose an object, because
// their directory vanished (issue #89).
func TestPruneEmptyDirs_RacesWriters(t *testing.T) {
	p, _ := localFSPlugin(t)
	contract.PruneRacesWriters(t, p, 4, 200)
}

// The race above in slow motion: the staging file's directory is gone
// by the time Put creates the file.
func TestCreateStaging_RecreatesVanishedDir(t *testing.T) {
	p, root := localFSPlugin(t)
	tmp := filepath.Join(root, "chunks/sha256/aa/bb/obj.hstmp-0011223344556677")
	f, err := p.createStaging(p.client, tmp)
	if err != nil {
		t.Fatalf("createStaging into a vanished directory: %v", err)
	}
	_ = f.Close()
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("staging file: %v", err)
	}
}

// A failure that is not a vanished directory is reported, not retried
// into a different error.
func TestCreateStaging_RealFailureSurfaces(t *testing.T) {
	p, root := localFSPlugin(t)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A regular file where the directory should be.
	if err := os.WriteFile(filepath.Join(root, "chunks"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.createStaging(p.client, filepath.Join(root, "chunks/aa/obj.hstmp-0")); err == nil {
		t.Fatal("createStaging under a regular file succeeded")
	}
}

func TestRenameIntoDir_RecreatesVanishedDir(t *testing.T) {
	p, root := localFSPlugin(t)
	ctx := context.Background()
	if _, err := p.Put(ctx, "manifests/db/src", strings.NewReader("m"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "manifests/db/src")

	// dst's directory does not exist — as after a prune that won the
	// race against RenameIfNotExists' mkdirAll.
	dst := filepath.Join(root, "manifests/other/deep/dst")
	if err := p.renameIntoDir(p.client, src, dst); err != nil {
		t.Fatalf("rename into a vanished directory: %v", err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "m" {
		t.Fatalf("dst after rename: %q, %v", b, err)
	}

	// A missing src is the other reading of "no such file". It must
	// come back as that, and must not leave directories behind for it.
	err := p.renameIntoDir(p.client, src, filepath.Join(root, "manifests/never/dst"))
	if !isNotExist(err) {
		t.Fatalf("rename of a missing src: %v, want not-exist", err)
	}
	if _, serr := os.Stat(filepath.Join(root, "manifests/never")); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("rename of a missing src created dst's directory (stat: %v)", serr)
	}
}

// A symlink is content, not a directory to descend into: its target may
// lie outside the repo.
func TestPruneEmptyDirs_DoesNotFollowSymlinks(t *testing.T) {
	p, root := localFSPlugin(t)
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(filepath.Join(outside, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "chunks/linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "chunks/linked/out")); err != nil {
		t.Fatal(err)
	}
	n, err := p.PruneEmptyDirs(ctx, "chunks", true)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0 and no error", n, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "empty")); err != nil {
		t.Fatalf("directory outside the repo was pruned through a symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "chunks/linked/out")); err != nil {
		t.Fatalf("symlink removed: %v", err)
	}
}

// No connection is an error — unlike a directory that could not be
// removed, which is a lost race.
func TestPruneEmptyDirs_ClosedPlugin(t *testing.T) {
	p, _ := localFSPlugin(t)
	// Not p.Close(): over these pipes the client's Close waits for a
	// server that is only torn down by the test's cleanup.
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	if _, err := p.PruneEmptyDirs(context.Background(), "chunks", false); !errors.Is(err, errClosed) {
		t.Fatalf("PruneEmptyDirs on a closed plugin: %v, want errClosed", err)
	}
}

func TestIsServerRefusal(t *testing.T) {
	if !isServerRefusal(&gosftp.StatusError{Code: 4}) { // SSH_FX_FAILURE: OpenSSH's "directory not empty"
		t.Error("an SFTP status is the server's answer")
	}
	if !isServerRefusal(os.ErrPermission) {
		t.Error("permission denied is the server's answer")
	}
	if isServerRefusal(io.ErrUnexpectedEOF) || isServerRefusal(gosftp.ErrSSHFxConnectionLost) {
		t.Error("a transport failure is not the server's answer")
	}
}
