package scp

// inproc_server_test.go — an in-process SSH server that runs exec
// requests with the local /bin/sh, plus a proxy that can silently
// drop traffic. Lets the keepalive / reconnect / ctx / durability
// tests drive the real plugin end to end without docker or sshd.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

const execTestPassword = "scp-exec-test-pw"

// execServer records every command it ran.
type execServer struct {
	addr    string
	hostPub ssh.PublicKey
	mu      sync.Mutex
	cmds    []string

	// Stress wedge: when wedgeSuffix is set, a `cat` of a path ending in it
	// serves only the first wedgeChunk bytes, then parks (the data channel
	// makes no progress) while the SSH control channel / keepalive stay
	// live. Inert unless a test sets it; the parked write returns when the
	// client disconnects. wedgeSuffix + wedgeChunk are guarded by mu.
	wedgeSuffix string
	wedgeChunk  int
}

// wedge returns the current wedge suffix + chunk (mu-guarded).
func (s *execServer) wedge() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wedgeSuffix, s.wedgeChunk
}

// setWedge arms or disarms the stress wedge (mu-guarded). An empty suffix
// disarms it (normal exec for every command).
func (s *execServer) setWedge(suffix string, chunk int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wedgeSuffix = suffix
	s.wedgeChunk = chunk
}

func (s *execServer) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

func startExecServer(t *testing.T) *execServer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	hostPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == execTestPassword {
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
	s := &execServer{addr: ln.Addr().String(), hostPub: hostPub}

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
				go ssh.DiscardRequests(reqs)
				for newCh := range chans {
					if newCh.ChannelType() != "session" {
						_ = newCh.Reject(ssh.UnknownChannelType, "only session")
						continue
					}
					ch, chReqs, cerr := newCh.Accept()
					if cerr != nil {
						continue
					}
					go s.serveSession(ch, chReqs)
				}
				_ = sconn.Wait()
			}(raw)
		}
	}()
	return s
}

func (s *execServer) serveSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	var cmd *exec.Cmd
	for req := range reqs {
		switch req.Type {
		case "exec":
			if len(req.Payload) < 4 {
				_ = req.Reply(false, nil)
				return
			}
			n := binary.BigEndian.Uint32(req.Payload[:4])
			command := string(req.Payload[4 : 4+n])
			s.mu.Lock()
			s.cmds = append(s.cmds, command)
			s.mu.Unlock()
			_ = req.Reply(true, nil)
			if s.wedgeCat(ch, reqs, command) {
				return
			}
			cmd = exec.Command("/bin/sh", "-c", command)
			cmd.Stdin = ch
			cmd.Stdout = ch
			cmd.Stderr = ch.Stderr()
			status := uint32(0)
			if err := cmd.Start(); err != nil {
				status = 127
			} else {
				// A signal from the client (ctx cancel) kills the command.
				go func() {
					for r := range reqs {
						if r.Type == "signal" && cmd.Process != nil {
							_ = cmd.Process.Kill()
						}
						if r.WantReply {
							_ = r.Reply(false, nil)
						}
					}
				}()
				if err := cmd.Wait(); err != nil {
					var ee *exec.ExitError
					if errors.As(err, &ee) {
						status = uint32(ee.ExitCode())
					} else {
						status = 1
					}
				}
			}
			_ = ch.CloseWrite()
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], status)
			_, _ = ch.SendRequest("exit-status", false, b[:])
			return
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// wedgeCat handles a `cat` of a wedged path: it serves only the first
// wedgeChunk bytes of the (locally existing) file, then parks — the data
// channel makes no further progress (no more bytes, no EOF) while the SSH
// control channel and keepalive stay live. It returns true (handling the
// request) only when the stress wedge is armed for this command; otherwise
// false (normal exec). The parked goroutine blocks on ch.Read, which returns
// when the read guard tears the transport down (ch closes), releasing it.
func (s *execServer) wedgeCat(ch ssh.Channel, reqs <-chan *ssh.Request, command string) bool {
	wedge, wedgeChunk := s.wedge()
	if wedge == "" {
		return false
	}
	fields := strings.Fields(command)
	if len(fields) != 2 || fields[0] != "cat" {
		return false
	}
	arg := unshellQuote(fields[1])
	if !strings.HasSuffix(arg, wedge) {
		return false
	}
	data, err := os.ReadFile(arg)
	if err != nil {
		// Absent file: report nothing and close (the client's cat would exit
		// non-zero; the stress scenarios always put the file first).
		_ = ch.CloseWrite()
		return true
	}
	chunk := wedgeChunk
	if chunk <= 0 || chunk > len(data) {
		chunk = len(data)
	}
	// Serve the first chunk, then park WITHOUT sending any more bytes or an
	// EOF: the client's stdout Read then makes no progress. This goroutine is
	// the per-session goroutine (NOT the mux/reqs loop), so parking here
	// blocks only the server-side channel; the client's transport teardown
	// (or process exit) ends it. It outlives the read guard's idle window,
	// which is exactly what makes the client's read stall.
	if _, werr := ch.Write(data[:chunk]); werr != nil {
		return true
	}
	select {}
}

// unshellQuote strips one layer of shellQuote's single quotes ("'...'" with
// the '\” escape) so the in-proc server can read the real path the plugin
// passed to `cat`.
func unshellQuote(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, `'\''`, "'")
	}
	return s
}

// dropProxy forwards until blackhole is set, then reads and discards.
type dropProxy struct {
	addr      string
	blackhole atomic.Bool
}

func startDropProxy(t *testing.T, upstream string) *dropProxy {
	t.Helper()
	p := &dropProxy{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})
	go func() {
		for {
			cli, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			up, derr := net.Dial("tcp", upstream)
			if derr != nil {
				_ = cli.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, cli, up)
			mu.Unlock()
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, rerr := src.Read(buf)
					if n > 0 && !p.blackhole.Load() {
						if _, werr := dst.Write(buf[:n]); werr != nil {
							return
						}
					}
					if rerr != nil {
						return
					}
				}
			}
			go pipe(up, cli)
			go pipe(cli, up)
		}
	}()
	return p
}

// openExecPlugin opens the plugin against addr (server or proxy) with
// a fresh repo root.
func openExecPlugin(t *testing.T, addr string, hostPub ssh.PublicKey) *Plugin {
	t.Helper()
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte(knownhosts.Line([]string{addr}, hostPub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse("scp://tester@" + addr + filepath.Join(dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{
		URL:    u,
		Extras: map[string]string{"known_hosts": kh, "password": execTestPassword},
	}); err != nil {
		t.Fatalf("Open: %v", err)
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

// shrinkKeepalive makes dead-peer detection take milliseconds.
func shrinkKeepalive(t *testing.T) {
	oi, ot, om, orl := keepaliveInterval, keepaliveTimeout, keepaliveMisses, redialMinInterval
	keepaliveInterval, keepaliveTimeout, keepaliveMisses = 150*time.Millisecond, 150*time.Millisecond, 2
	redialMinInterval = 100 * time.Millisecond
	t.Cleanup(func() {
		keepaliveInterval, keepaliveTimeout, keepaliveMisses, redialMinInterval = oi, ot, om, orl
	})
}
