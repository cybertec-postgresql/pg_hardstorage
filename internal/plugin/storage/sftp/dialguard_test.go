package sftp

// dialguard_test.go — the SFTP subsystem handshake is bounded (issue #73).
//
// A peer that completes SSH auth and accepts the "sftp" subsystem but never
// sends its version packet parks sftp.NewClient in recvVersion. Before the
// fix that hung the first Open and every reconnect forever.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// startMuteSubsystemServer runs an SSH server that authenticates, accepts
// the sftp subsystem request, then never writes a byte.
func startMuteSubsystemServer(t *testing.T) (addr string, hostPub ssh.PublicKey) {
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
					ch, chReqs, cerr := newCh.Accept()
					if cerr != nil {
						continue
					}
					go func(ch ssh.Channel, chReqs <-chan *ssh.Request) {
						for req := range chReqs {
							_ = req.Reply(req.Type == "subsystem", nil)
						}
						_ = ch.Close()
					}(ch, chReqs)
				}
				_ = sconn.Wait()
			}(raw)
		}
	}()
	return ln.Addr().String(), hostPub
}

func TestDial_StalledSubsystemHandshakeFailsFast(t *testing.T) {
	shrinkReadGuard(t, 300*time.Millisecond)
	addr, hostPub := startMuteSubsystemServer(t)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(knownhosts.Line([]string{addr}, hostPub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse("sftp://tester@" + addr + "/repo")
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{}
	done := make(chan error, 1)
	go func() {
		done <- p.Open(context.Background(), storage.StorageConfig{
			URL:    u,
			Extras: map[string]string{"known_hosts": kh, "password": kaTestPassword},
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errSFTPReadStalled) {
			t.Fatalf("Open = %v, want errSFTPReadStalled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Open hung on a stalled sftp subsystem handshake")
	}
}
