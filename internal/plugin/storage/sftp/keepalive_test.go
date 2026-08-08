package sftp

// keepalive_test.go — the soak-caught hang, reproduced deterministically.
//
// A real SSH+SFTP server runs in-process; the plugin connects through
// a proxy that can be flipped to BLACKHOLE mode: bytes from the client
// are still read (writes keep succeeding) but nothing is forwarded and
// no replies ever come back — the silent-drop shape of a NAT expiry or
// a peer that lost power. Without the keepalive the next operation
// blocks forever (the storage fault soak sat 14 minutes inside
// pkg/sftp's sendPacket until the 1h package timeout shot the binary);
// with it the connection is torn down after the miss budget and every
// parked call returns an error.

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gosftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"

	"context"
	"strings"
)

const kaTestPassword = "keepalive-test-pw"

// startInProcSFTPServer runs a minimal SSH server whose only job is to
// serve the sftp subsystem over an in-memory filesystem and to answer
// global requests (the keepalive probe) the way OpenSSH does.
func startInProcSFTPServer(t *testing.T) (addr string, hostPub ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	hostPubKey, err := ssh.NewPublicKey(pub)
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

	// ONE in-memory filesystem shared by every session: the reconnect
	// tests write through connection N and read through connection
	// N+1, and a per-session InMemHandler silently hands each session
	// its own empty world — the first version of the recovery test
	// "failed" with a not-found that was actually proof the reconnect
	// worked.
	handlers := gosftp.InMemHandler()

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
				// DiscardRequests REPLIES to want-reply requests
				// (with failure), which is exactly what OpenSSH does
				// for unknown request types — any reply proves the
				// round-trip, so the keepalive counts it as alive.
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
					go func(ch ssh.Channel, chReqs <-chan *ssh.Request) {
						// ch.Close is not optional: without the server's
						// CHANNEL_CLOSE the client's graceful shutdown
						// waits forever for an EOF that never comes —
						// real OpenSSH always sends it.
						defer func() { _ = ch.Close() }()
						for req := range chReqs {
							ok := req.Type == "subsystem" && len(req.Payload) >= 4 &&
								string(req.Payload[4:]) == "sftp"
							_ = req.Reply(ok, nil)
							if ok {
								srv := gosftp.NewRequestServer(ch, handlers)
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
	return ln.Addr().String(), hostPubKey
}

// blackholeProxy forwards client<->upstream until Blackhole is set;
// after that it keeps READING from both sides (so writes keep
// succeeding — the silent-drop shape) but forwards nothing.
type blackholeProxy struct {
	addr      string
	ln        net.Listener
	blackhole atomic.Bool
	conns     struct {
		sync.Mutex
		all []net.Conn
	}
}

func (p *blackholeProxy) close(t *testing.T) {
	t.Helper()
	if p.ln != nil {
		_ = p.ln.Close()
	}
	p.conns.Lock()
	for _, c := range p.conns.all {
		_ = c.Close()
	}
	p.conns.Unlock()
}

func startBlackholeProxy(t *testing.T, upstream string) *blackholeProxy {
	t.Helper()
	p := &blackholeProxy{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.ln = ln
	t.Cleanup(func() {
		_ = ln.Close()
		p.conns.Lock()
		for _, c := range p.conns.all {
			_ = c.Close()
		}
		p.conns.Unlock()
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
			p.conns.Lock()
			p.conns.all = append(p.conns.all, cli, up)
			p.conns.Unlock()
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

// TestKeepalive_DeadConnectionErrorsInsteadOfHanging is the finding.
func TestKeepalive_DeadConnectionErrorsInsteadOfHanging(t *testing.T) {
	// Milliseconds instead of production's tens of seconds; restored
	// afterwards so sibling tests see the real values.
	oi, ot, om := keepaliveInterval, keepaliveTimeout, keepaliveMisses
	keepaliveInterval, keepaliveTimeout, keepaliveMisses = 150*time.Millisecond, 150*time.Millisecond, 2
	t.Cleanup(func() { keepaliveInterval, keepaliveTimeout, keepaliveMisses = oi, ot, om })

	serverAddr, hostPub := startInProcSFTPServer(t)
	proxy := startBlackholeProxy(t, serverAddr)

	khPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(khPath,
		[]byte(knownhosts.Line([]string{proxy.addr}, hostPub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse("sftp://tester@" + proxy.addr + "/upload")
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{}
	if oerr := p.Open(context.Background(), storage.StorageConfig{
		URL: u,
		Extras: map[string]string{
			"known_hosts": khPath,
			"password":    kaTestPassword,
		},
	}); oerr != nil {
		t.Fatalf("open through live proxy: %v", oerr)
	}
	// Bounded close: WITHOUT the keepalive (the registered mutant), a
	// graceful Close on the blackholed connection hangs exactly like
	// the operation under test — an unbounded cleanup would turn the
	// mutant's clean 15s red into a full package timeout.
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() { _ = p.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
		}
	})

	// Prove the fixture: a real round-trip through the proxy.
	body := []byte("alive")
	if _, perr := p.Put(context.Background(), "probe", strings.NewReader(string(body)),
		storage.PutOptions{ContentLength: int64(len(body))}); perr != nil {
		t.Fatalf("put through live proxy: %v", perr)
	}

	// The peer goes silently away.
	proxy.blackhole.Store(true)

	done := make(chan error, 1)
	go func() {
		rc, gerr := p.Get(context.Background(), "probe")
		if gerr == nil {
			_, gerr = io.ReadAll(rc)
			_ = rc.Close()
		}
		done <- gerr
	}()

	start := time.Now()
	select {
	case gerr := <-done:
		if gerr == nil {
			t.Fatal("Get returned nil error through a blackholed connection — the read " +
				"must fail once the keepalive tears the connection down")
		}
		t.Logf("dead connection surfaced as error after %v: %v", time.Since(start), gerr)
	case <-time.After(15 * time.Second):
		t.Fatal("Get is still blocked 15s after the peer went silent.\n\n" +
			"This is the storage fault soak's 14-minute sendPacket stall: without the " +
			"keepalive teardown, a dead SFTP connection hangs every operation forever — " +
			"a wal fetch inside restore_command hangs recovery indefinitely, a backup " +
			"never finishes and never fails, an archiver stalls while pg_wal fills.")
	}
}

// TestKeepalive_LiveConnectionUntouched: a healthy connection must
// survive many keepalive cycles — a false-positive teardown would
// break every long-running backup over sftp.
func TestKeepalive_LiveConnectionUntouched(t *testing.T) {
	oi, ot, om := keepaliveInterval, keepaliveTimeout, keepaliveMisses
	keepaliveInterval, keepaliveTimeout, keepaliveMisses = 100*time.Millisecond, 200*time.Millisecond, 2
	t.Cleanup(func() { keepaliveInterval, keepaliveTimeout, keepaliveMisses = oi, ot, om })

	serverAddr, hostPub := startInProcSFTPServer(t)
	khPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(khPath,
		[]byte(knownhosts.Line([]string{serverAddr}, hostPub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("sftp://tester@" + serverAddr + "/upload")
	p := &Plugin{}
	if oerr := p.Open(context.Background(), storage.StorageConfig{
		URL: u,
		Extras: map[string]string{
			"known_hosts": khPath,
			"password":    kaTestPassword,
		},
	}); oerr != nil {
		t.Fatalf("open: %v", oerr)
	}
	t.Cleanup(func() { _ = p.Close() })

	// 20+ keepalive cycles with periodic traffic.
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		key := "probe"
		if _, perr := p.Put(context.Background(), key, strings.NewReader("x"),
			storage.PutOptions{ContentLength: 1}); perr != nil {
			t.Fatalf("healthy connection failed at cycle %d: %v — the keepalive must "+
				"never tear down a live peer", i, perr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestReconnect_AfterTeardown_OpsRecover is bug #25's regression test:
// the keepalive tearing down a dead connection must not brick the
// HANDLE. A `wal stream` holds one plugin for days and the CLI opens
// the repo once outside its retry loop — before reconnection existed,
// one 70-second stall meant every later operation failed on the same
// dead client until process restart, archiving stopped while the
// primary kept writing.
func TestReconnect_AfterTeardown_OpsRecover(t *testing.T) {
	oi, ot, om := keepaliveInterval, keepaliveTimeout, keepaliveMisses
	keepaliveInterval, keepaliveTimeout, keepaliveMisses = 150*time.Millisecond, 150*time.Millisecond, 2
	orl := redialMinInterval
	redialMinInterval = 100 * time.Millisecond
	t.Cleanup(func() {
		keepaliveInterval, keepaliveTimeout, keepaliveMisses = oi, ot, om
		redialMinInterval = orl
	})

	serverAddr, hostPub := startInProcSFTPServer(t)
	proxy := startBlackholeProxy(t, serverAddr)
	khPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(khPath,
		[]byte(knownhosts.Line([]string{proxy.addr}, hostPub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("sftp://tester@" + proxy.addr + "/upload")
	p := &Plugin{}
	if oerr := p.Open(context.Background(), storage.StorageConfig{
		URL:    u,
		Extras: map[string]string{"known_hosts": khPath, "password": kaTestPassword},
	}); oerr != nil {
		t.Fatalf("open: %v", oerr)
	}
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() { _ = p.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
		}
	})
	if _, perr := p.Put(context.Background(), "probe", strings.NewReader("v1"),
		storage.PutOptions{ContentLength: 2}); perr != nil {
		t.Fatalf("baseline put: %v", perr)
	}

	// Silent drop → keepalive teardown → the parked op errors.
	proxy.blackhole.Store(true)
	if _, gerr := p.Get(context.Background(), "probe"); gerr == nil {
		t.Fatal("Get succeeded through a blackholed connection")
	}

	// The network comes back. The handle must HEAL: new connection
	// through the proxy, operation succeeds, data intact.
	proxy.blackhole.Store(false)
	deadline := time.Now().Add(20 * time.Second)
	for {
		rc, gerr := p.Get(context.Background(), "probe")
		if gerr == nil {
			body, rerr := io.ReadAll(rc)
			_ = rc.Close()
			if rerr == nil && string(body) == "v1" {
				return // healed
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("handle never recovered after the network came back: %v\n\n"+
				"This is the brick: the keepalive rightly killed the dead connection, but "+
				"nothing re-dials — a days-long wal stream stops archiving after one "+
				"transient stall and stays stopped until an operator restarts it.", gerr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestReconnect_ServerGone_TypedErrorFast: when the peer is REALLY
// gone (listener closed), the reconnect path must fail with an error
// promptly — not hang, not spin.
func TestReconnect_ServerGone_TypedErrorFast(t *testing.T) {
	oi, ot, om := keepaliveInterval, keepaliveTimeout, keepaliveMisses
	keepaliveInterval, keepaliveTimeout, keepaliveMisses = 150*time.Millisecond, 150*time.Millisecond, 2
	orl := redialMinInterval
	redialMinInterval = 100 * time.Millisecond
	t.Cleanup(func() {
		keepaliveInterval, keepaliveTimeout, keepaliveMisses = oi, ot, om
		redialMinInterval = orl
	})

	serverAddr, hostPub := startInProcSFTPServer(t)
	proxy := startBlackholeProxy(t, serverAddr)
	khPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(khPath,
		[]byte(knownhosts.Line([]string{proxy.addr}, hostPub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("sftp://tester@" + proxy.addr + "/upload")
	p := &Plugin{}
	if oerr := p.Open(context.Background(), storage.StorageConfig{
		URL:    u,
		Extras: map[string]string{"known_hosts": khPath, "password": kaTestPassword},
	}); oerr != nil {
		t.Fatalf("open: %v", oerr)
	}
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() { _ = p.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
		}
	})

	proxy.blackhole.Store(true)
	_, _ = p.Get(context.Background(), "probe") // force teardown
	proxy.close(t)                              // peer truly gone: connects now refuse

	done := make(chan error, 1)
	go func() {
		time.Sleep(250 * time.Millisecond) // past the redial rate limit
		_, gerr := p.Get(context.Background(), "probe")
		done <- gerr
	}()
	select {
	case gerr := <-done:
		if gerr == nil {
			t.Fatal("Get succeeded against a gone server")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("reconnect against a gone server hung instead of failing fast")
	}
}
