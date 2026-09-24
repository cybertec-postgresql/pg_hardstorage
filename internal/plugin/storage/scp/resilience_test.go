package scp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// TestKeepalive_DeadConnectionErrorsInsteadOfHanging pins the first
// half of M33: scp had no keepalive, so a peer that went silently away
// (NAT expiry, power loss) left every operation blocked forever — the
// failure the sftp plugin's keepalive was built for.
func TestKeepalive_DeadConnectionErrorsInsteadOfHanging(t *testing.T) {
	shrinkKeepalive(t)
	srv := startExecServer(t)
	proxy := startDropProxy(t, srv.addr)
	p := openExecPlugin(t, proxy.addr, srv.hostPub)

	if _, err := p.Put(context.Background(), "probe", strings.NewReader("alive"), storage.PutOptions{}); err != nil {
		t.Fatalf("put through live proxy: %v", err)
	}
	proxy.blackhole.Store(true)

	done := make(chan error, 1)
	go func() {
		_, err := p.Stat(context.Background(), "probe")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Stat succeeded through a blackholed connection")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Stat still blocked 15s after the peer went silent: no keepalive teardown")
	}
}

// TestReconnect_AfterTeardown_OpsRecover pins the second half: once
// the dead connection is torn down, the handle must heal when the
// network returns instead of failing every later call until restart.
func TestReconnect_AfterTeardown_OpsRecover(t *testing.T) {
	shrinkKeepalive(t)
	srv := startExecServer(t)
	proxy := startDropProxy(t, srv.addr)
	p := openExecPlugin(t, proxy.addr, srv.hostPub)

	if _, err := p.Put(context.Background(), "probe", strings.NewReader("v1"), storage.PutOptions{}); err != nil {
		t.Fatalf("baseline put: %v", err)
	}
	proxy.blackhole.Store(true)
	done := make(chan error, 1)
	go func() { _, err := p.Stat(context.Background(), "probe"); done <- err }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Stat never returned on the dead connection")
	}
	proxy.blackhole.Store(false)

	deadline := time.Now().Add(20 * time.Second)
	for {
		rc, err := p.Get(context.Background(), "probe")
		if err == nil {
			b, rerr := io.ReadAll(rc)
			_ = rc.Close()
			if rerr == nil && string(b) == "v1" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("handle never recovered after the network came back: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestReconnect_ClosedTransportHeals: a transport that died any other
// way (server restart, RST) must also be re-dialled on next use.
func TestReconnect_ClosedTransportHeals(t *testing.T) {
	// Production keepalive timing on purpose: recovery must come from
	// the session-open path noticing the dead transport, not from the
	// prober (which would take ~30s here).
	orl := redialMinInterval
	redialMinInterval = 100 * time.Millisecond
	t.Cleanup(func() { redialMinInterval = orl })
	srv := startExecServer(t)
	p := openExecPlugin(t, srv.addr, srv.hostPub)

	p.mu.Lock()
	_ = p.ssh.Close()
	p.mu.Unlock()

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := p.Put(context.Background(), "after", strings.NewReader("x"), storage.PutOptions{})
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Put never recovered after the transport closed: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// TestStreamRead_HonoursContext pins the ctx half of M33: a blocked
// remote read ignored cancellation — Read checked ctx only BEFORE
// blocking in the SSH stdout pipe.
func TestStreamRead_HonoursContext(t *testing.T) {
	srv := startExecServer(t)
	p := openExecPlugin(t, srv.addr, srv.hostPub)

	ctx, cancel := context.WithCancel(context.Background())
	rc, err := p.streamRead(ctx, "sleep 30", nil)
	if err != nil {
		t.Fatalf("streamRead: %v", err)
	}
	defer rc.Close()
	time.AfterFunc(200*time.Millisecond, cancel)

	done := make(chan error, 1)
	go func() {
		_, rerr := io.ReadAll(rc)
		done <- rerr
	}()
	select {
	case rerr := <-done:
		if !errors.Is(rerr, context.Canceled) {
			t.Errorf("read after cancel = %v, want context.Canceled", rerr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Read still blocked 10s after its context was cancelled")
	}
}

// TestReapStaging_RemovesLeakedTemps pins M32 for scp: a Put that died
// between upload and commit left "<key>.hstmp-*" behind, hidden from
// List and never removed.
func TestReapStaging_RemovesLeakedTemps(t *testing.T) {
	srv := startExecServer(t)
	p := openExecPlugin(t, srv.addr, srv.hostPub)
	ctx := context.Background()
	if _, err := p.Put(ctx, "chunks/aa/real", strings.NewReader("keep"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	stale := filepath.Join(p.root, "chunks/aa/bb.hstmp-0123456789abcdef")
	fresh := filepath.Join(p.root, "chunks/aa/cc.hstmp-fedcba9876543210")
	for _, f := range []string{stale, fresh} {
		if err := os.WriteFile(f, []byte("leaked"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	st, err := storage.ReapStagingOf(ctx, p, storage.DefaultStagingReapAge)
	if err != nil {
		t.Fatalf("ReapStagingOf: %v", err)
	}
	if st.Removed != 1 || st.Bytes != int64(len("leaked")) {
		t.Errorf("stats = %+v, want 1 removed / 6 bytes", st)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale staging temp survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh staging temp (a live writer's) was reaped: %v", err)
	}
	if _, err := p.Stat(ctx, "chunks/aa/real"); err != nil {
		t.Errorf("real object reaped: %v", err)
	}
}

// TestPut_SyncsBeforeCommit pins M29 for scp: Put never flushed the
// staged file or the directory entry. The staged file must be synced
// before it is linked/renamed into place, and the directory after.
func TestPut_SyncsBeforeCommit(t *testing.T) {
	srv := startExecServer(t)
	p := openExecPlugin(t, srv.addr, srv.hostPub)
	for _, ifNot := range []bool{true, false} {
		before := len(srv.Commands())
		if _, err := p.Put(context.Background(), "chunks/aa/bb", strings.NewReader("x"),
			storage.PutOptions{IfNotExists: ifNot}); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
			t.Fatalf("Put: %v", err)
		}
		cmds := srv.Commands()[before:]
		fileSync, commit, dirSync := -1, -1, -1
		for i, c := range cmds {
			switch {
			case strings.Contains(c, "cat >") && strings.Contains(c, "sync"):
				fileSync = i
			case strings.HasPrefix(c, "ln -T") || strings.HasPrefix(c, "mv -T"):
				commit = i
				if strings.Contains(c, "sync") {
					dirSync = i
				}
			}
		}
		if fileSync < 0 || commit < 0 || fileSync > commit {
			t.Errorf("IfNotExists=%v: staged file not synced before commit; commands: %q", ifNot, cmds)
		}
		if dirSync < 0 {
			t.Errorf("IfNotExists=%v: directory entry not synced after commit; commands: %q", ifNot, cmds)
		}
	}
}
