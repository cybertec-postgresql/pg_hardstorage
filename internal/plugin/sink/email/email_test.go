package email_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/email"
)

// fakeSMTP is a minimal RFC 5321 server. It implements just enough
// of EHLO / MAIL / RCPT / DATA / QUIT for our PLAIN-auth-over-
// no-TLS tests. Anything more elaborate (STARTTLS, LOGIN auth,
// implicit TLS) is exercised via dedicated tests below.
type fakeSMTP struct {
	mu          sync.Mutex
	mailFroms   []string
	rcptTos     []string
	bodies      []string
	requireAuth bool
	authed      bool
	authMechs   []string // EHLO advertises these via AUTH line
	// Per-connection script of optional behaviours.
	rejectMail bool // refuse MAIL FROM with 550
	rejectRcpt string
}

func (s *fakeSMTP) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	rd := bufio.NewReader(c)
	wr := bufio.NewWriter(c)
	defer wr.Flush()
	writeLine := func(line string) {
		_, _ = wr.WriteString(line + "\r\n")
		_ = wr.Flush()
	}

	writeLine("220 fakesmtp ESMTP")
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			writeLine("250-fakesmtp")
			writeLine("250-PIPELINING")
			writeLine("250-8BITMIME")
			if len(s.authMechs) > 0 {
				writeLine("250-AUTH " + strings.Join(s.authMechs, " "))
			}
			writeLine("250 SIZE 10485760")

		case strings.HasPrefix(upper, "AUTH"):
			s.mu.Lock()
			s.authed = true
			s.mu.Unlock()
			// Consume the credential line for PLAIN inline-auth.
			if strings.Contains(upper, "PLAIN") && !strings.Contains(upper, "PLAIN ") {
				// Server-prompted PLAIN — request the credentials.
				writeLine("334 ")
				_, _ = rd.ReadString('\n')
			}
			writeLine("235 Authentication successful")

		case strings.HasPrefix(upper, "MAIL FROM:"):
			if s.requireAuth && !s.authed {
				writeLine("530 Authentication required")
				continue
			}
			if s.rejectMail {
				writeLine("550 mailbox unavailable")
				continue
			}
			from := extractAddress(line[len("MAIL FROM:"):])
			s.mu.Lock()
			s.mailFroms = append(s.mailFroms, from)
			s.mu.Unlock()
			writeLine("250 ok")

		case strings.HasPrefix(upper, "RCPT TO:"):
			rcpt := extractAddress(line[len("RCPT TO:"):])
			if s.rejectRcpt != "" && rcpt == s.rejectRcpt {
				writeLine("550 no such user")
				continue
			}
			s.mu.Lock()
			s.rcptTos = append(s.rcptTos, rcpt)
			s.mu.Unlock()
			writeLine("250 ok")

		case strings.HasPrefix(upper, "DATA"):
			writeLine("354 send data ending with <CRLF>.<CRLF>")
			body := &strings.Builder{}
			for {
				dl, err := rd.ReadString('\n')
				if err != nil {
					return
				}
				if dl == ".\r\n" || dl == ".\n" {
					break
				}
				// Undo dot-stuffing.
				if strings.HasPrefix(dl, "..") {
					dl = dl[1:]
				}
				body.WriteString(dl)
			}
			s.mu.Lock()
			s.bodies = append(s.bodies, body.String())
			s.mu.Unlock()
			writeLine("250 ok")

		case strings.HasPrefix(upper, "QUIT"):
			writeLine("221 bye")
			return

		case strings.HasPrefix(upper, "RSET"), strings.HasPrefix(upper, "NOOP"):
			writeLine("250 ok")

		default:
			writeLine("502 unrecognised")
		}
	}
}

func startFakeSMTP(t *testing.T, s *fakeSMTP) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				return
			}
			go s.handle(c)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// build constructs a Sink against the given fake-server host:port.
// `tls=none, auth=none` lets us exercise the basic SMTP conversation
// without TLS / SASL infrastructure.
func build(t *testing.T, host string, port int, extra map[string]any) output.Sink {
	t.Helper()
	cfg := map[string]any{
		"smtp_host":    host,
		"smtp_port":    port,
		"tls":          "none",
		"auth":         "none",
		"from":         "pg-hardstorage@example.com",
		"to":           []string{"ops@example.com"},
		"min_severity": "debug",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	s, err := email.NewFromSpec(output.SinkSpec{Name: "test", Plugin: "email", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEmail_Build_RequiresHostFromTo(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"missing host", map[string]any{
			"from": "a@b", "to": []string{"c@d"},
		}},
		{"missing from", map[string]any{
			"smtp_host": "x", "to": []string{"c@d"}, "tls": "none", "auth": "none",
		}},
		{"missing to", map[string]any{
			"smtp_host": "x", "from": "a@b", "tls": "none", "auth": "none",
		}},
		{"empty to slice", map[string]any{
			"smtp_host": "x", "from": "a@b", "to": []string{}, "tls": "none", "auth": "none",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := email.NewFromSpec(output.SinkSpec{Name: "e", Plugin: "email", Config: c.cfg})
			if err == nil {
				t.Errorf("expected error for cfg %v", c.cfg)
			}
		})
	}
}

func TestEmail_Build_RejectsBadModes(t *testing.T) {
	for _, mode := range []map[string]any{
		{"smtp_host": "x", "from": "a@b", "to": []string{"c@d"}, "tls": "rugby"},
		{"smtp_host": "x", "from": "a@b", "to": []string{"c@d"}, "auth": "rugby"},
	} {
		_, err := email.NewFromSpec(output.SinkSpec{Name: "e", Plugin: "email", Config: mode})
		if err == nil {
			t.Errorf("expected error for cfg %v", mode)
		}
	}
}

func TestEmail_Build_AuthRequiresCredentials(t *testing.T) {
	_, err := email.NewFromSpec(output.SinkSpec{Name: "e", Plugin: "email", Config: map[string]any{
		"smtp_host": "x", "from": "a@b", "to": []string{"c@d"},
		"tls": "none", "auth": "plain",
		// missing username + password
	}})
	if err == nil {
		t.Error("auth=plain without credentials should fail")
	}
}

func TestEmail_Build_PortMustBeInRange(t *testing.T) {
	_, err := email.NewFromSpec(output.SinkSpec{Name: "e", Plugin: "email", Config: map[string]any{
		"smtp_host": "x", "from": "a@b", "to": []string{"c@d"},
		"tls": "none", "auth": "none",
		"smtp_port": 99999,
	}})
	if err == nil {
		t.Error("port out of range should fail")
	}
}

func TestEmail_Emit_HappyPath_NoAuth_NoTLS(t *testing.T) {
	srv := &fakeSMTP{}
	host, port := startFakeSMTP(t, srv)

	s := build(t, host, port, map[string]any{
		"to": []string{"ops@example.com", "oncall@example.com"},
		"cc": []string{"audit@example.com"},
	})
	defer s.Close()

	ev := output.NewEvent(output.SeverityError, "backup", "manifest.replica_failed").
		WithSubject(output.Subject{Deployment: "db1"})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Verify the conversation we observed on the server side.
	srv.mu.Lock()
	defer srv.mu.Unlock()

	if len(srv.mailFroms) != 1 || srv.mailFroms[0] != "pg-hardstorage@example.com" {
		t.Errorf("MAIL FROM = %v, want [pg-hardstorage@example.com]", srv.mailFroms)
	}
	wantRcpts := []string{"ops@example.com", "oncall@example.com", "audit@example.com"}
	if !equalUnordered(srv.rcptTos, wantRcpts) {
		t.Errorf("RCPT TO = %v, want %v (any order)", srv.rcptTos, wantRcpts)
	}
	if len(srv.bodies) != 1 {
		t.Fatalf("expected 1 DATA body; got %d", len(srv.bodies))
	}
	body := srv.bodies[0]
	for _, want := range []string{
		"From: pg-hardstorage@example.com",
		"To: ops@example.com, oncall@example.com",
		"Cc: audit@example.com",
		"Subject: [pg_hardstorage] ERROR backup/manifest.replica_failed deployment=db1",
		"Severity: ERROR",
		"Component: backup",
		"Op: manifest.replica_failed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestEmail_Emit_PLAINAuth(t *testing.T) {
	srv := &fakeSMTP{requireAuth: true, authMechs: []string{"PLAIN"}}
	host, port := startFakeSMTP(t, srv)

	s := build(t, host, port, map[string]any{
		"auth":     "plain",
		"username": "ops@example.com",
		"password": "hunter2",
	})
	defer s.Close()

	ev := output.NewEvent(output.SeverityError, "x", "y")
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.authed {
		t.Error("server should have observed AUTH PLAIN")
	}
	if len(srv.bodies) != 1 {
		t.Errorf("expected 1 body delivered; got %d", len(srv.bodies))
	}
}

func TestEmail_FiltersBelowMinSeverity(t *testing.T) {
	srv := &fakeSMTP{}
	host, port := startFakeSMTP(t, srv)

	s := build(t, host, port, map[string]any{"min_severity": "critical"})
	defer s.Close()

	// Error is BELOW critical — sink should drop it without sending.
	if err := s.Emit(context.Background(),
		output.NewEvent(output.SeverityError, "x", "y")); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.bodies) != 0 {
		t.Errorf("error event below 'critical' threshold should be dropped; got %d delivered",
			len(srv.bodies))
	}
}

func TestEmail_PreCancelledCtx_RefusesEmit(t *testing.T) {
	// A cancelled ctx should bail BEFORE we open a TCP connection.
	// Use an unroutable address — if the sink dials, the test hangs.
	cfg := map[string]any{
		"smtp_host":    "127.0.0.1",
		"smtp_port":    1, // assume nothing listens on port 1
		"tls":          "none",
		"auth":         "none",
		"from":         "a@b",
		"to":           []string{"c@d"},
		"min_severity": "debug",
	}
	s, err := email.NewFromSpec(output.SinkSpec{Name: "e", Plugin: "email", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Emit(ctx, output.NewEvent(output.SeverityError, "x", "y")); err == nil {
		t.Error("Emit should have honoured pre-cancelled ctx")
	}
}

func TestEmail_RegistersWithDefaultRegistry(t *testing.T) {
	found := false
	for _, p := range output.DefaultSinkRegistry.Plugins() {
		if p == "email" {
			found = true
		}
	}
	if !found {
		t.Errorf("email should self-register")
	}
}

func TestEmail_DotStuffing_PreservedThroughSMTP(t *testing.T) {
	// A line beginning with `.` would be interpreted as end-of-data
	// without dot-stuffing per RFC 5321 §4.5.2. The sink prefixes
	// such lines with an extra `.`; the fake server undoes the
	// prefix on receive. Round-trip a body whose suggestion line
	// starts with `.` — we use a custom Body field via WithBody.
	srv := &fakeSMTP{}
	host, port := startFakeSMTP(t, srv)
	s := build(t, host, port, nil)
	defer s.Close()

	ev := output.NewEvent(output.SeverityError, "x", "y").
		WithBody(map[string]any{"note": "leading-dot lines are tricky"}).
		WithSuggestion(&output.Suggestion{
			Human: ".hidden-file is the issue",
		})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.bodies) != 1 {
		t.Fatalf("expected 1 body; got %d", len(srv.bodies))
	}
	if !strings.Contains(srv.bodies[0], ".hidden-file is the issue") {
		t.Errorf("dot-stuffed line not round-tripped:\n%s", srv.bodies[0])
	}
}

// extractAddress pulls just the address out of a MAIL FROM / RCPT TO
// argument. ESMTP appends extensions like ` BODY=8BITMIME ` after the
// address; we extract the value between `<` and `>` if present, else
// take everything up to the first space.
func extractAddress(s string) string {
	s = strings.TrimSpace(s)
	if open := strings.IndexByte(s, '<'); open >= 0 {
		if close := strings.IndexByte(s[open+1:], '>'); close >= 0 {
			return s[open+1 : open+1+close]
		}
	}
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}

// equalUnordered tests slice equality ignoring element order.
func equalUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := map[string]int{}
	for _, s := range a {
		count[s]++
	}
	for _, s := range b {
		count[s]--
		if count[s] < 0 {
			return false
		}
	}
	return true
}
