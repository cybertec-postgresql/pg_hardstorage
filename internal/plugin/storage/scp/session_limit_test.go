package scp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// A stock sshd allows 10 sessions per connection (MaxSessions). Backup
// keeps 16 chunk uploads in flight and restore 16 reads (issues #90,
// #97), each of them a session, so the plugin itself has to stay under
// the server's limit: beyond it sshd refuses the channel, and a refusal
// that outlasts the short retry failed the Put or the Get — and with it
// the backup or the restore.
func TestSessions_StayUnderTheServersLimit(t *testing.T) {
	srv := startExecServer(t)
	srv.maxSessions.Store(10)
	srv.sessionHold.Store(int64(40 * time.Millisecond))
	p := openExecPlugin(t, srv.addr, srv.hostPub)
	ctx := context.Background()

	const workers, perWorker = 32, 6
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				key := fmt.Sprintf("chunks/w%02d/k%03d", w, i)
				body := []byte(strings.Repeat(key, 50))
				if _, err := p.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{IfNotExists: true}); err != nil {
					errs <- fmt.Errorf("put %s: %w", key, err)
					return
				}
				rc, err := p.Get(ctx, key)
				if err != nil {
					errs <- fmt.Errorf("get %s: %w", key, err)
					return
				}
				got, rerr := io.ReadAll(rc)
				_ = rc.Close()
				if rerr != nil || !bytes.Equal(got, body) {
					errs <- fmt.Errorf("read %s: %d bytes, %v", key, len(got), rerr)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if peak := srv.peakSessions.Load(); peak > 10 {
		t.Errorf("server saw %d sessions at once", peak)
	}
	t.Logf("peak sessions=%d, refused opens=%d", srv.peakSessions.Load(), srv.rejected.Load())
}

// The slots are a bound, not a lock: a caller that holds sessions open
// while it opens another one (a listing whose consumer reads each
// object, readers kept open side by side) is let through after a wait
// instead of waiting on itself forever.
func TestSessions_HeldSlotsDoNotDeadlock(t *testing.T) {
	old := sessionSlotWait
	sessionSlotWait = 100 * time.Millisecond
	t.Cleanup(func() { sessionSlotWait = old })

	srv := startExecServer(t)
	p := openExecPlugin(t, srv.addr, srv.hostPub)
	ctx := context.Background()
	if _, err := p.Put(ctx, "held/obj", strings.NewReader("held"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	var open []io.ReadCloser
	t.Cleanup(func() {
		for _, rc := range open {
			_ = rc.Close()
		}
	})
	for i := 0; i < maxOpenSessions+3; i++ {
		rc, err := p.Get(ctx, "held/obj")
		if err != nil {
			t.Fatalf("reader %d (all earlier ones still open): %v", i, err)
		}
		open = append(open, rc)
	}

	// A cancelled caller stops waiting for a slot at once.
	sessionSlotWait = time.Minute
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.Get(cctx, "held/obj"); err == nil {
		t.Fatal("Get succeeded with every slot held and its context expired")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("a cancelled caller waited %v for a slot", d)
	}

	// Closing the readers gives the slots back.
	for _, rc := range open {
		_ = rc.Close()
	}
	open = nil
	if got := len(p.slots); got != 0 {
		t.Fatalf("%d slots still held after every session was closed", got)
	}
}
