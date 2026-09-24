package email_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/email"
)

// startStalledSMTP accepts connections and never says anything — the
// shape of a hung relay or a black-holed middlebox that completed the
// TCP handshake. Accepted conns are held open until the test ends.
func startStalledSMTP(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { <-done; _ = c.Close() }()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// emitWithin runs Emit and fails the test if it has not returned
// within limit. Returns Emit's error.
func emitWithin(t *testing.T, s output.Sink, ctx context.Context, limit time.Duration) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- s.Emit(ctx, output.NewEvent(output.SeverityError, "x", "y")) }()
	select {
	case err := <-errc:
		return err
	case <-time.After(limit):
		t.Fatalf("Emit still blocked after %v against a stalled relay", limit)
		return nil
	}
}

// A relay that accepts TCP and then stalls must not pin Emit (and,
// through it, Dispatcher.Close) forever: cancelling ctx aborts the
// SMTP conversation.
func TestEmail_StalledRelay_CtxCancelAborts(t *testing.T) {
	host, port := startStalledSMTP(t)
	s := build(t, host, port, map[string]any{"timeout": "1h"})
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	if err := emitWithin(t, s, ctx, 5*time.Second); err == nil {
		t.Fatal("Emit against a stalled relay returned nil")
	}
}

// With a context that never ends (the dispatcher forwards the caller's
// ctx, which is often Background), the sink's own timeout bounds the
// whole conversation.
func TestEmail_StalledRelay_TimeoutBoundsConversation(t *testing.T) {
	host, port := startStalledSMTP(t)
	s := build(t, host, port, map[string]any{"timeout": "300ms"})
	defer s.Close()

	if err := emitWithin(t, s, context.Background(), 5*time.Second); err == nil {
		t.Fatal("Emit against a stalled relay returned nil")
	}
}

func TestEmail_Build_RejectsBadTimeout(t *testing.T) {
	for _, v := range []string{"soon", "0s", "-1s"} {
		cfg := map[string]any{
			"smtp_host": "127.0.0.1", "tls": "none", "auth": "none",
			"from": "a@b", "to": "c@d", "timeout": v,
		}
		if _, err := email.NewFromSpec(output.SinkSpec{Name: "e", Plugin: "email", Config: cfg}); err == nil {
			t.Errorf("timeout=%q accepted", v)
		}
	}
}

// A body line that begins with '.' must reach the relay's mailbox
// exactly as written. net/smtp's DATA writer (textproto.DotWriter)
// already dot-stuffs; stuffing again in frame() made the recipient
// see "..line".
func TestEmail_LeadingDotLine_NotDoubleStuffed(t *testing.T) {
	srv := &fakeSMTP{}
	host, port := startFakeSMTP(t, srv)
	s := build(t, host, port, nil)
	defer s.Close()

	ev := output.NewEvent(output.SeverityError, "x", "y").
		WithSuggestion(&output.Suggestion{Human: "see below", Command: "ls\n.hidden-file"})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.bodies) != 1 {
		t.Fatalf("expected 1 body; got %d", len(srv.bodies))
	}
	var found bool
	for _, line := range strings.Split(srv.bodies[0], "\r\n") {
		if line == ".hidden-file" {
			found = true
		}
		if line == "..hidden-file" {
			t.Errorf("leading-dot line double-stuffed:\n%s", srv.bodies[0])
		}
	}
	if !found {
		t.Errorf("leading-dot line missing:\n%s", srv.bodies[0])
	}
}

// airgapped: strict covers every outbound path, SMTP included.
func TestEmail_AirgapStrict_RefusesPublicRelay(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()

	cfg := map[string]any{
		"smtp_host": "smtp.example.com", "tls": "none", "auth": "none",
		"from": "a@b", "to": "c@d",
	}
	_, err := email.NewFromSpec(output.SinkSpec{Name: "e", Plugin: "email", Config: cfg})
	if !errors.Is(err, airgap.ErrEndpointNotAllowed) {
		t.Fatalf("public relay under strict airgap: err = %v, want ErrEndpointNotAllowed", err)
	}

	// A loopback / RFC1918 relay stays usable.
	for _, h := range []string{"127.0.0.1", "10.1.2.3", "::1"} {
		cfg["smtp_host"] = h
		if _, err := email.NewFromSpec(output.SinkSpec{Name: "e", Plugin: "email", Config: cfg}); err != nil {
			t.Errorf("relay %s under strict airgap: %v", h, err)
		}
	}
}
