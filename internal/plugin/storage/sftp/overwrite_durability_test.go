package sftp

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	gosftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// recordingCmds wraps the in-memory server's command handler and logs
// every Filecmd/PosixRename, so a test can see what Put sent.
type recordingCmds struct {
	inner gosftp.FileCmder
	mu    sync.Mutex
	log   []string
}

func (r *recordingCmds) record(s string) {
	r.mu.Lock()
	r.log = append(r.log, s)
	r.mu.Unlock()
}

func (r *recordingCmds) Filecmd(req *gosftp.Request) error {
	r.record(req.Method + " " + req.Filepath)
	return r.inner.Filecmd(req)
}

func (r *recordingCmds) PosixRename(req *gosftp.Request) error {
	r.record("PosixRename " + req.Filepath + " -> " + req.Target)
	return r.inner.(gosftp.PosixRenameFileCmder).PosixRename(req)
}

func (r *recordingCmds) Log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

// pipePlugin returns a Plugin talking to an in-process request server
// over pipes (no SSH, no network).
func pipePlugin(t *testing.T) (*Plugin, *recordingCmds) {
	t.Helper()
	h := gosftp.InMemHandler()
	rec := &recordingCmds{inner: h.FileCmd}
	h.FileCmd = rec

	c2s_r, c2s_w := io.Pipe()
	s2c_r, s2c_w := io.Pipe()
	srv := gosftp.NewRequestServer(struct {
		io.Reader
		io.WriteCloser
	}{c2s_r, s2c_w}, h)
	go func() { _ = srv.Serve() }()
	cli, err := gosftp.NewClientPipe(s2c_r, c2s_w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Server side first: the client's Close waits for its reader
		// to see EOF on the server-to-client pipe.
		_ = srv.Close()
		_ = s2c_w.Close()
		_ = c2s_r.Close()
		_ = cli.Close()
	})
	return &Plugin{root: "/repo", client: cli, cfgssh: &ssh.ClientConfig{}}, rec
}

func readAll(t *testing.T, p *Plugin, key string) string {
	t.Helper()
	rc, err := p.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// TestPut_OverwriteIsAtomicReplace pins H14: an overwrite Put removed
// the destination and THEN renamed the temp into place. Between the
// two the key did not exist — a racer could create it there (and then
// be silently overwritten, breaking lease exclusivity), and a dropped
// connection in the gap lost the object outright. posix-rename@openssh
// replaces atomically, so the remove must not happen when it is
// available.
func TestPut_OverwriteIsAtomicReplace(t *testing.T) {
	p, rec := pipePlugin(t)
	ctx := context.Background()
	if _, err := p.Put(ctx, "locks/lease", strings.NewReader("v1"), storage.PutOptions{}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	before := len(rec.Log())
	if _, err := p.Put(ctx, "locks/lease", strings.NewReader("v2"), storage.PutOptions{}); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	for _, op := range rec.Log()[before:] {
		if op == "Remove /repo/locks/lease" {
			t.Fatalf("overwrite removed the destination before renaming (ops: %v)", rec.Log()[before:])
		}
	}
	if got := readAll(t, p, "locks/lease"); got != "v2" {
		t.Errorf("content = %q, want v2", got)
	}
}

// TestReapStaging_RemovesLeakedTemps pins M32 for sftp: a Put that
// died between upload and commit left "<key>.hstmp-*" behind, hidden
// from List and never removed. (The in-memory server stamps files with
// the current time, so the cut-off here is zero.)
func TestReapStaging_RemovesLeakedTemps(t *testing.T) {
	p, _ := pipePlugin(t)
	ctx := context.Background()
	if _, err := p.Put(ctx, "chunks/aa/real", strings.NewReader("keep"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := p.client.Create("/repo/chunks/aa/bb.hstmp-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("leaked"))
	_ = f.Close()
	time.Sleep(10 * time.Millisecond)

	var _ storage.StagingReapAware = p
	st, err := p.ReapStaging(ctx, 0)
	if err != nil {
		t.Fatalf("ReapStaging: %v", err)
	}
	if st.Removed != 1 || st.Bytes != int64(len("leaked")) {
		t.Errorf("stats = %+v, want 1 removed / 6 bytes", st)
	}
	if _, err := p.client.Stat("/repo/chunks/aa/bb.hstmp-0123456789abcdef"); err == nil {
		t.Error("leaked staging temp survived")
	}
	if _, err := p.Stat(ctx, "chunks/aa/real"); err != nil {
		t.Errorf("real object reaped: %v", err)
	}
}

// TestPut_FsyncsStagedFileBeforeCommit pins M29: Put never fsynced the
// staged file although the package doc (and DurabilityInline) promised
// it. The staged bytes must be flushed before the rename publishes
// them, and a failed flush must fail the Put rather than publish bytes
// that may not survive a crash.
func TestPut_FsyncsStagedFileBeforeCommit(t *testing.T) {
	p, rec := pipePlugin(t)
	var synced []string
	orig := syncStaged
	t.Cleanup(func() { syncStaged = orig })
	syncStaged = func(_ *gosftp.Client, f *gosftp.File) error {
		synced = append(synced, f.Name())
		rec.record("Sync " + f.Name())
		return nil
	}
	if _, err := p.Put(context.Background(), "chunks/aa/bb", strings.NewReader("x"), storage.PutOptions{IfNotExists: true}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(synced) != 1 || !strings.Contains(synced[0], ".hstmp-") {
		t.Fatalf("staged file not fsynced before commit: %v", synced)
	}
	log := rec.Log()
	syncAt, commitAt := -1, -1
	for i, op := range log {
		if strings.HasPrefix(op, "Sync ") {
			syncAt = i
		}
		if strings.HasPrefix(op, "Link ") || strings.HasPrefix(op, "PosixRename ") {
			commitAt = i
		}
	}
	if syncAt < 0 || commitAt < 0 || syncAt > commitAt {
		t.Errorf("fsync must precede the commit; ops: %v", log)
	}

	syncStaged = func(*gosftp.Client, *gosftp.File) error { return errors.New("EIO") }
	if _, err := p.Put(context.Background(), "chunks/aa/cc", strings.NewReader("x"), storage.PutOptions{IfNotExists: true}); err == nil {
		t.Fatal("Put succeeded although the fsync failed")
	}
	if _, err := p.Stat(context.Background(), "chunks/aa/cc"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("object published despite a failed fsync (Stat err = %v)", err)
	}
}
