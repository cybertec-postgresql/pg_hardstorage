package sftp

// stress_test.go — the read-guard against a REAL in-process SSH+SFTP server.
//
// readguard_test.go proves the guard machinery in isolation (a parked
// io.ReadCloser). This file proves the shape it was built for: a LIVE peer
// whose SSH control channel keeps answering keepalives while one SFTP
// DATA-channel read makes no progress. The in-proc server below wedges at
// the data-channel layer — the SSH global-request loop (the keepalive probe
// path, answered by ssh.DiscardRequests) keeps responding, while one file's
// SFTP read parks in the handler's ReadAt. Only readIdleTimeout's teardown
// can unblock that; the keepalive provably cannot (its probes keep
// succeeding), which is exactly why the campaign's 10–14m stalls outlasted
// the keepalive.
//
// This is the one shape the pre-fix suites never exercised end to end: every
// earlier repro was a DEAD peer (blackhole proxy), which the keepalive alone
// catches.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gosftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// ---------------------------------------------------------------------------
// in-memory SFTP filesystem (Handlers impl) — shared by the scenarios, so a
// reconnect re-dials to the same world (the wedged file is still there).
// ---------------------------------------------------------------------------

type sftpStressFS struct {
	mu    sync.Mutex
	files map[string]*sftpStressFile
}

func newSftpStressFS() *sftpStressFS {
	return &sftpStressFS{files: make(map[string]*sftpStressFile)}
}

func normSftpPath(p string) string {
	c := path.Clean(p)
	if c == "." {
		return "/"
	}
	if !strings.HasPrefix(c, "/") {
		c = "/" + c
	}
	return c
}

type sftpStressFile struct {
	name  string
	data  []byte
	isDir bool
}

func (s *sftpStressFile) Truncate(size int64) error {
	if size < 0 {
		return errors.New("negative size")
	}
	if size == 0 {
		s.data = nil
		return nil
	}
	if int64(len(s.data)) < size {
		grown := make([]byte, size)
		copy(grown, s.data)
		s.data = grown
	} else {
		s.data = s.data[:size]
	}
	return nil
}

// FileCmd implements gosftp.FileCmder (the Put write path: create/truncate/
// write/rename).
func (f *sftpStressFS) Filecmd(r *gosftp.Request) error {
	p := normSftpPath(r.Filepath)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case "Setstat":
		if r.AttrFlags().Size {
			if _, ok := f.files[p]; !ok {
				f.files[p] = &sftpStressFile{name: path.Base(p)}
			}
			return f.files[p].Truncate(int64(r.Attributes().Size))
		}
		return nil
	case "Remove":
		delete(f.files, p)
		return nil
	case "Rmdir":
		delete(f.files, p)
		return nil
	case "Mkdir":
		if _, ok := f.files[p]; !ok {
			f.files[p] = &sftpStressFile{name: path.Base(p), isDir: true}
		}
		return nil
	case "Rename", "PosixRename":
		// The server advertises posix-rename@openssh.com, so Put's commit
		// (atomicRename) issues PosixRename, not plain Rename.
		src, dst := normSftpPath(r.Filepath), normSftpPath(r.Target)
		if sf, ok := f.files[src]; ok {
			sf.name = path.Base(dst)
			delete(f.files, src)
			f.files[dst] = sf
		}
		return nil
	default:
		// Link / Symlink / StatVFS: not issued by the stress scenarios
		// (IfNotExists is never set, so the hardlink path is not taken).
		return nil
	}
}

// memReader serves the file bytes (a plain, live data channel).
type memReader struct{ data []byte }

func (r *memReader) ReadAt(b []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(b, r.data[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

// FileReader returns a plain memReader; the stress handlers wrap it for the
// wedging scenarios.
func (f *sftpStressFS) Fileread(r *gosftp.Request) (io.ReaderAt, error) {
	p := normSftpPath(r.Filepath)
	f.mu.Lock()
	sf, ok := f.files[p]
	f.mu.Unlock()
	if !ok {
		return nil, &os.PathError{Op: "open", Path: r.Filepath, Err: os.ErrNotExist}
	}
	if sf.isDir {
		return nil, errors.New("is a directory")
	}
	return &memReader{data: sf.data}, nil
}

// memWriter implements the Put write path.
type memWriter struct {
	file *sftpStressFile
	fs   *sftpStressFS
}

func (w *memWriter) WriteAt(b []byte, off int64) (int, error) {
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	end := off + int64(len(b))
	if end > int64(len(w.file.data)) {
		grown := make([]byte, end)
		copy(grown, w.file.data)
		w.file.data = grown
	}
	copy(w.file.data[off:off+int64(len(b))], b)
	return len(b), nil
}

func (f *sftpStressFS) Filewrite(r *gosftp.Request) (io.WriterAt, error) {
	p := normSftpPath(r.Filepath)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[p]; !ok {
		f.files[p] = &sftpStressFile{name: path.Base(p)}
	}
	return &memWriter{file: f.files[p], fs: f}, nil
}

// memLister implements gosftp.ListerAt for a batch of entries.
type memLister struct {
	infos []os.FileInfo
	pos   int
}

func (l *memLister) ListAt(ls []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l.infos)) {
		return 0, io.EOF
	}
	n := 0
	for i := int(off); i < len(l.infos) && n < len(ls); i, n = i+1, n+1 {
		ls[n] = l.infos[i]
	}
	if int(off)+n >= len(l.infos) {
		return n, io.EOF
	}
	return n, nil
}

type sftpFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	isDir   bool
}

func (f *sftpFileInfo) Name() string       { return f.name }
func (f *sftpFileInfo) Size() int64        { return f.size }
func (f *sftpFileInfo) Mode() os.FileMode  { return 0o644 }
func (f *sftpFileInfo) ModTime() time.Time { return f.modTime }
func (f *sftpFileInfo) IsDir() bool        { return f.isDir }
func (f *sftpFileInfo) Sys() any           { return nil }

// FileList implements gosftp.FileLister (Stat + List).
func (f *sftpStressFS) Filelist(r *gosftp.Request) (gosftp.ListerAt, error) {
	p := normSftpPath(r.Filepath)
	f.mu.Lock()
	defer f.mu.Unlock()
	if sf, ok := f.files[p]; ok {
		return &memLister{infos: []os.FileInfo{&sftpFileInfo{
			name: sf.name, size: int64(len(sf.data)), modTime: time.Now(), isDir: sf.isDir,
		}}}, nil
	}
	// No exact entry: if p is a directory prefix, list its children.
	var infos []os.FileInfo
	for k, sf := range f.files {
		if sf.isDir && (k == p || strings.HasPrefix(k, p+"/")) {
			infos = append(infos, &sftpFileInfo{name: path.Base(k), size: int64(len(sf.data)),
				modTime: time.Now(), isDir: sf.isDir})
		}
	}
	if len(infos) == 0 {
		return nil, &os.PathError{Op: "stat", Path: r.Filepath, Err: os.ErrNotExist}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name() < infos[j].Name() })
	return &memLister{infos: infos}, nil
}

// ---------------------------------------------------------------------------
// the wedging layer — a LIVE peer whose data channel stalls.
// ---------------------------------------------------------------------------

// wedgingReader serves bytes for reads that START below wedgeOff, then parks
// forever on the first read at/ beyond it (the data-channel stall). Reads
// before the offset return, so the caller sees partial data first — the
// trickle-then-stall shape.
type wedgingReader struct {
	base     *memReader
	wedgeOff int64
}

func (w *wedgingReader) ReadAt(b []byte, off int64) (int, error) {
	if off >= w.wedgeOff {
		select {} // park: the SFTP data channel makes no progress
	}
	return w.base.ReadAt(b, off)
}

// slowReader serves each 32KiB-ish chunk after a fixed delay — a live but
// slow data channel (progress every chunk), which must NOT trip the guard.
type slowReader struct {
	base  *memReader
	delay time.Duration
}

func (s *slowReader) ReadAt(b []byte, off int64) (int, error) {
	time.Sleep(s.delay)
	return s.base.ReadAt(b, off)
}

// wedgingLister parks on the first ListAt (the Stat/List round-trip wedge).
type wedgingLister struct{}

func (wedgingLister) ListAt([]os.FileInfo, int64) (int, error) {
	select {}
}

// atomicString is a thread-safe string slot the wedging layer flips at runtime.
type atomicString struct {
	mu sync.Mutex
	v  string
}

func (a *atomicString) Load() string   { a.mu.Lock(); defer a.mu.Unlock(); return a.v }
func (a *atomicString) Store(s string) { a.mu.Lock(); a.v = s; a.mu.Unlock() }

// sftpStressHandler wraps the in-mem FS and wedges one key's data channel
// while everything else stays fully live. The key is matched by SUFFIX
// because the plugin prepends its configured repo root.
type sftpStressHandler struct {
	fs        *sftpStressFS
	wedgeKey  atomicString // suffix whose READ data channel parks
	wedgeOff  int64        // wedge parks on the first read AT/BEYOND this offset
	statWedge atomicString // suffix whose STAT round-trip parks
	slowKey   atomicString // suffix served by slowReader (live but slow)
}

func (h *sftpStressHandler) matches(s, suffix string) bool {
	return s != "" && (s == suffix || strings.HasSuffix(s, "/"+suffix))
}

func (h *sftpStressHandler) Fileread(r *gosftp.Request) (io.ReaderAt, error) {
	rd, err := h.fs.Fileread(r)
	if err != nil {
		return rd, err
	}
	if w := h.wedgeKey.Load(); h.matches(r.Filepath, w) {
		return &wedgingReader{base: rd.(*memReader), wedgeOff: h.wedgeOff}, nil
	}
	if s := h.slowKey.Load(); h.matches(r.Filepath, s) {
		return &slowReader{base: rd.(*memReader), delay: 150 * time.Millisecond}, nil
	}
	return rd, nil
}

func (h *sftpStressHandler) Filewrite(r *gosftp.Request) (io.WriterAt, error) {
	return h.fs.Filewrite(r)
}

func (h *sftpStressHandler) Filecmd(r *gosftp.Request) error { return h.fs.Filecmd(r) }

func (h *sftpStressHandler) Filelist(r *gosftp.Request) (gosftp.ListerAt, error) {
	if w := h.statWedge.Load(); h.matches(r.Filepath, w) {
		return wedgingLister{}, nil
	}
	return h.fs.Filelist(r)
}

// startSftpStressServer runs an in-proc SSH server serving the SFTP subsystem
// over the wedging handler. It answers the keepalive global request
// (ssh.DiscardRequests), so the peer is LIVE even while a data read is wedged.
func startSftpStressServer(t *testing.T) (addr string, hostPub ssh.PublicKey, h *sftpStressHandler) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	hostPub, err = ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == kaTestPassword {
				return nil, nil
			}
			return nil, errors.New("bad password")
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	h = &sftpStressHandler{fs: newSftpStressFS()}
	go func() {
		for {
			raw, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(raw net.Conn) {
				sconn, chans, reqs, herr := ssh.NewServerConn(raw, cfg)
				if herr != nil {
					_ = raw.Close()
					return
				}
				go ssh.DiscardRequests(reqs) // keepalive global request → reply (live)
				for newCh := range chans {
					if newCh.ChannelType() != "session" {
						_ = newCh.Reject(ssh.UnknownChannelType, "only session")
						continue
					}
					ch, chReqs, cerr := newCh.Accept()
					if cerr != nil {
						continue
					}
					go func(ch ssh.Channel, chReqs <-chan *ssh.Request) {
						defer func() { _ = ch.Close() }()
						for req := range chReqs {
							ok := req.Type == "subsystem" && len(req.Payload) >= 4 &&
								string(req.Payload[4:]) == "sftp"
							_ = req.Reply(ok, nil)
							if ok {
								srv := gosftp.NewRequestServer(ch, gosftp.Handlers{
									FileGet:  h,
									FilePut:  h,
									FileCmd:  h,
									FileList: h,
								})
								_ = srv.Serve()
								return
							}
						}
					}(ch, chReqs)
				}
				_ = sconn.Wait()
			}(raw)
		}
	}()
	return ln.Addr().String(), hostPub, h
}

// openStressPlugin opens the plugin against the stress server with a fresh
// known_hosts, and installs a bounded Close.
func openStressPlugin(t *testing.T, addr string, hostPub ssh.PublicKey) *Plugin {
	t.Helper()
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(knownhosts.Line([]string{addr}, hostPub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse("sftp://tester@" + addr + "/repo")
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{
		URL:    u,
		Extras: map[string]string{"known_hosts": kh, "password": kaTestPassword},
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { _ = p.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return p
}

// shrinkReadGuard makes the data-plane read guard fire in milliseconds;
// restored after the test.
func shrinkReadGuard(t *testing.T, idle time.Duration) {
	t.Helper()
	old := readIdleTimeout
	readIdleTimeout = idle
	t.Cleanup(func() { readIdleTimeout = old })
}

// countingReader counts delivered bytes and surfaces the underlying error.
type countingReader struct {
	rc io.Reader
	n  *int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	*c.n += n
	return n, err
}

// ---------------------------------------------------------------------------
// scenario 1 — the finding: a LIVE peer (keepalive answering) wedges one
// data-channel read. It must become an ERROR within readIdleTimeout, not a
// hang, and the transport of that generation must tear down.
// ---------------------------------------------------------------------------

func TestStress_WedgedDataRead_LivePeer(t *testing.T) {
	addr, hostPub, h := startSftpStressServer(t)
	shrinkReadGuard(t, 300*time.Millisecond)
	oi, ot, om := keepaliveInterval, keepaliveTimeout, keepaliveMisses
	keepaliveInterval, keepaliveTimeout, keepaliveMisses = 60*time.Millisecond, 40*time.Millisecond, 3
	t.Cleanup(func() { keepaliveInterval, keepaliveTimeout, keepaliveMisses = oi, ot, om })

	p := openStressPlugin(t, addr, hostPub)
	body := bytes.Repeat([]byte("0123456789abcdef"), 16) // 256 bytes
	if _, err := p.Put(context.Background(), "wedged", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put wedged: %v", err)
	}

	// The peer goes live-wedged: keepalives keep being answered (the global
	// request path is untouched), the data channel parks. The keepalive
	// therefore keeps the connection ALIVE — only the read guard can bound
	// the read.
	h.wedgeKey.Store("wedged")
	h.wedgeOff = 0

	rc, gerr := p.Get(context.Background(), "wedged")
	if gerr != nil {
		t.Fatalf("Get open: %v", gerr)
	}
	start := time.Now()
	var got int
	_, rerr := io.Copy(io.Discard, &countingReader{rc: rc, n: &got})
	_ = rc.Close()
	elapsed := time.Since(start)
	if rerr == nil {
		t.Fatalf("read over a wedged data channel succeeded — the read guard is " +
			"not bounding the no-progress read")
	}
	if !errors.Is(rerr, errSFTPReadStalled) {
		t.Fatalf("wedged read surfaced %q; want the stall error (the data channel "+
			"must be bounded by the read guard, not the keepalive)", rerr)
	}
	if got != 0 {
		t.Fatalf("wedged read delivered %d bytes before stalling; the wedge parks on "+
			"the first read", got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("wedged read took %v; the guard fires at %v (bounded, not hung)",
			elapsed, readIdleTimeout)
	}
	if !p.genDead() {
		t.Fatalf("wedged read did not tear down its own generation — the handle would " +
			"stay wedged forever")
	}
	t.Logf("live peer, wedged data read → stall error after %v, generation torn down", elapsed)
}

// The Stat/List round-trip is the same class (idleRoundTrip), not just Get:
// a wedged Stat must bound, not hang.
func TestStress_WedgedStat_LivePeer(t *testing.T) {
	addr, hostPub, h := startSftpStressServer(t)
	shrinkReadGuard(t, 300*time.Millisecond)
	p := openStressPlugin(t, addr, hostPub)
	if _, err := p.Put(context.Background(), "statwedged", strings.NewReader("x"),
		storage.PutOptions{ContentLength: 1}); err != nil {
		t.Fatalf("put: %v", err)
	}
	h.statWedge.Store("statwedged")

	start := time.Now()
	_, serr := p.Stat(context.Background(), "statwedged")
	elapsed := time.Since(start)
	if serr == nil {
		t.Fatalf("Stat over a wedged round-trip succeeded — the read guard must bound it")
	}
	if !errors.Is(serr, errSFTPReadStalled) {
		t.Fatalf("wedged Stat surfaced %q; want the stall error", serr)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("wedged Stat took %v; must be bounded", elapsed)
	}
	if !p.genDead() {
		t.Fatalf("wedged Stat did not tear down its generation")
	}
	t.Logf("live peer, wedged Stat → stall error after %v", elapsed)
}

// scenario 2 — trickle-then-stall: the peer delivers the first chunk, then
// parks mid-stream. The stall is detected on the NO-PROGRESS window (not on
// total time), and the partial bytes are visible before the error.
func TestStress_TrickleThenStall(t *testing.T) {
	addr, hostPub, h := startSftpStressServer(t)
	shrinkReadGuard(t, 300*time.Millisecond)
	p := openStressPlugin(t, addr, hostPub)
	body := bytes.Repeat([]byte("wal-segment"), 4096) // 51200 bytes
	if _, err := p.Put(context.Background(), "trickle", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Serve the first 32KiB (one FREAD chunk), park on the second.
	h.wedgeKey.Store("trickle")
	h.wedgeOff = 32768

	rc, gerr := p.Get(context.Background(), "trickle")
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	var got int
	start := time.Now()
	_, rerr := io.Copy(io.Discard, &countingReader{rc: rc, n: &got})
	_ = rc.Close()
	elapsed := time.Since(start)
	if rerr == nil {
		t.Fatalf("trickle-then-stall completed cleanly — the mid-stream park must trip the guard")
	}
	if !errors.Is(rerr, errSFTPReadStalled) {
		t.Fatalf("trickle stall surfaced %q; want the stall error", rerr)
	}
	if got < 16384 {
		t.Fatalf("trickle stall delivered only %d bytes; the first 32KiB chunk must "+
			"arrive before the park", got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("trickle stall took %v; must be bounded", elapsed)
	}
	if !p.genDead() {
		t.Fatalf("trickle stall did not tear down its generation")
	}
	t.Logf("trickle-then-stall: %d bytes delivered before the park, error after %v", got, elapsed)
}

// scenario 3 — mid-io.Copy of a large Get: the guard is hit once per 32KiB
// chunk (the WAL-stream hot path). A park after the first chunk must bound
// the whole io.Copy, not hang it.
func TestStress_MidCopyStall_LargeGet(t *testing.T) {
	addr, hostPub, h := startSftpStressServer(t)
	shrinkReadGuard(t, 300*time.Millisecond)
	p := openStressPlugin(t, addr, hostPub)
	body := bytes.Repeat([]byte("0123456789abcdef"), 16384) // 256 KiB
	if _, err := p.Put(context.Background(), "large", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put: %v", err)
	}
	h.wedgeKey.Store("large")
	h.wedgeOff = 32768

	rc, gerr := p.Get(context.Background(), "large")
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	var got int
	start := time.Now()
	_, rerr := io.Copy(io.Discard, &countingReader{rc: rc, n: &got})
	_ = rc.Close()
	elapsed := time.Since(start)
	if rerr == nil {
		t.Fatalf("mid-copy stall completed cleanly — the guard must bound the copy")
	}
	if !errors.Is(rerr, errSFTPReadStalled) {
		t.Fatalf("mid-copy stall surfaced %q; want the stall error", rerr)
	}
	if got < 32768 {
		t.Fatalf("mid-copy stall delivered %d bytes; expected the first 32KiB chunk "+
			"before the park (the trickle of a real WAL fetch)", got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("mid-copy stall took %v; must be bounded", elapsed)
	}
	t.Logf("mid-copy stall: %d/%d bytes delivered before the park, error after %v",
		got, len(body), elapsed)
}

// scenario 4 — concurrent Get storm on a HEALTHY peer: the read guard must
// never false-positive on live, concurrent reads. No stall, full bytes, the
// generation stays alive.
func TestStress_ConcurrentStorm_NoFalsePositive(t *testing.T) {
	addr, hostPub, _ := startSftpStressServer(t)
	shrinkReadGuard(t, 300*time.Millisecond)
	oi, ot, om := keepaliveInterval, keepaliveTimeout, keepaliveMisses
	keepaliveInterval, keepaliveTimeout, keepaliveMisses = 40*time.Millisecond, 30*time.Millisecond, 3
	t.Cleanup(func() { keepaliveInterval, keepaliveTimeout, keepaliveMisses = oi, ot, om })

	p := openStressPlugin(t, addr, hostPub)
	body := bytes.Repeat([]byte("storm-bytes-"), 4096) // 57344 bytes
	if _, err := p.Put(context.Background(), "big", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put: %v", err)
	}

	const N = 16
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			rc, gerr := p.Get(context.Background(), "big")
			if gerr != nil {
				errs <- gerr
				return
			}
			buf, rerr := io.ReadAll(rc)
			_ = rc.Close()
			if rerr != nil {
				errs <- rerr
				return
			}
			if !bytes.Equal(buf, body) {
				errs <- errors.New("byte mismatch under concurrent storm")
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < N; i++ {
		if e := <-errs; e != nil {
			t.Fatalf("healthy concurrent Get #%d failed: %v — the read guard "+
				"false-positived on a live peer", i, e)
		}
	}
	if p.genDead() {
		t.Fatalf("healthy concurrent storm tore down a live generation (false positive)")
	}
	t.Logf("concurrent storm: %d parallel 56KiB Gets, zero stall, generation alive", N)
}

// scenario 5 — concurrent Gets on a WEDGED file: the first stall tears down
// the shared generation; the others must surface an ERROR (stall or
// connection-closed), never hang.
func TestStress_WedgedConcurrent_NoHang(t *testing.T) {
	addr, hostPub, h := startSftpStressServer(t)
	shrinkReadGuard(t, 300*time.Millisecond)
	orl := redialMinInterval
	redialMinInterval = 100 * time.Millisecond
	t.Cleanup(func() { redialMinInterval = orl })

	p := openStressPlugin(t, addr, hostPub)
	body := bytes.Repeat([]byte("wedged-concurrent"), 4096)
	if _, err := p.Put(context.Background(), "wcc", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := p.Put(context.Background(), "healthy", strings.NewReader("ok"),
		storage.PutOptions{ContentLength: 2}); err != nil {
		t.Fatalf("put healthy: %v", err)
	}
	h.wedgeKey.Store("wcc")
	h.wedgeOff = 0

	const N = 6
	results := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			rc, gerr := p.Get(context.Background(), "wcc")
			if gerr != nil {
				results <- gerr
				return
			}
			_, rerr := io.ReadAll(rc)
			_ = rc.Close()
			results <- rerr
		}()
	}
	sawStall := 0
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; i < N; i++ {
		select {
		case rerr := <-results:
			if rerr == nil {
				t.Fatalf("concurrent wedged Get #%d succeeded — it must error", i)
			}
			if errors.Is(rerr, errSFTPReadStalled) {
				sawStall++
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("concurrent wedged Get hung — a wedged read must never hang the " +
				"concurrent callers")
		}
	}
	if sawStall == 0 {
		t.Fatalf("no concurrent Get surfaced the stall error — the first stall must " +
			"be visible to at least one caller")
	}

	// The handle must heal: a fresh op on a HEALTHY key reconnects and works.
	deadline = time.Now().Add(5 * time.Second)
	for {
		rc, gerr := p.Get(context.Background(), "healthy")
		if gerr == nil {
			b, rerr := io.ReadAll(rc)
			_ = rc.Close()
			if rerr == nil && string(b) == "ok" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("handle never healed after the concurrent wedge: %v", gerr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("concurrent wedge: %d/%d callers saw the stall error, handle healed", sawStall, N)
}

// scenario 6 — slow-but-live: reads that progress steadily but take a large
// fraction of readIdleTimeout per chunk must NOT trip the guard. The guard
// bounds NO progress, not total time.
func TestStress_SlowButLive_NoFalsePositive(t *testing.T) {
	addr, hostPub, h := startSftpStressServer(t)
	shrinkReadGuard(t, 600*time.Millisecond)
	p := openStressPlugin(t, addr, hostPub)
	body := bytes.Repeat([]byte("slow-but-live-"), 4096) // 65536 bytes
	if _, err := p.Put(context.Background(), "slow", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Serve each 32KiB chunk after a 150ms pause: ~150ms per read vs a 600ms
	// no-progress window → live, not wedged.
	h.slowKey.Store("slow")

	rc, gerr := p.Get(context.Background(), "slow")
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	start := time.Now()
	buf, rerr := io.ReadAll(rc)
	_ = rc.Close()
	elapsed := time.Since(start)
	if rerr != nil {
		t.Fatalf("slow-but-live read errored: %v — reads that keep making progress "+
			"must not trip the no-progress guard", rerr)
	}
	if !bytes.Equal(buf, body) {
		t.Fatalf("slow-but-live delivered %d bytes, want %d", len(buf), len(body))
	}
	if p.genDead() {
		t.Fatalf("slow-but-live read tore down a live generation (false positive)")
	}
	t.Logf("slow-but-live: 64KiB at ~150ms/chunk read clean in %v, no false positive", elapsed)
}
