// Package email implements an output.Sink that delivers events via SMTP.
//
// Configuration (YAML keys):
//
//	plugin: email
//	config:
//	  smtp_host: smtp.example.com           # required
//	  smtp_port: 587                        # default: 587
//	  tls: starttls                         # starttls | implicit | none — default: starttls
//	  auth: plain                           # plain | login | none      — default: plain
//	  username: ops@acme.com                # required if auth != none
//	  password: <secret>                    # required if auth != none
//	  from: pg-hardstorage@acme.com         # required
//	  to: ["ops@acme.com"]                  # required, ≥1 recipient
//	  cc: []                                # optional
//	  subject_prefix: "[pg_hardstorage]"    # optional
//	  min_severity: error                   # default: error (email is for waking people)
//
// Why a focused TLS / auth model rather than try-everything? Operators
// typically know which combination their SMTP relay accepts; an
// auto-detect protocol negotiation hides failures behind retries and
// makes debugging hard. Explicit > clever.
//
// What's deliberately NOT here for:
//
//   - HTML bodies (the markdown renderer's job; pipe through it
//     externally if HTML mail is required)
//   - Per-recipient routing rules (use multiple sinks with different
//     `to` lists + min_severity filters instead)
//   - DKIM signing / bounce handling (the SMTP relay's job)
package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func init() {
	output.DefaultSinkRegistry.Register("email", NewFromSpec)
}

// TLSMode controls how the SMTP connection is wrapped in TLS.
//
//	"starttls" — connect plaintext on smtp_port, then upgrade via
//	             STARTTLS (the modern default; matches port 587).
//	"implicit" — connect with TLS from the first byte (port 465).
//	"none"     — no TLS at all (lab / loopback only).
type TLSMode string

const (
	// TLSStartTLS connects plaintext on smtp_port then upgrades via
	// STARTTLS — the modern default, matches port 587.
	TLSStartTLS TLSMode = "starttls"
	// TLSImplicit connects with TLS from the first byte — matches
	// the legacy SMTPS port 465.
	TLSImplicit TLSMode = "implicit"
	// TLSNone disables TLS entirely; only safe for loopback or
	// lab relays.
	TLSNone TLSMode = "none"
)

// AuthMode picks the SMTP authentication shape.
//
//	"plain" — RFC 4616 PLAIN over the TLS-wrapped channel
//	"login" — Microsoft's LOGIN mechanism (still common at hosters)
//	"none"  — anonymous SMTP (typically loopback or LAN relays)
type AuthMode string

const (
	// AuthPlain selects RFC 4616 PLAIN over the TLS-wrapped channel.
	AuthPlain AuthMode = "plain"
	// AuthLogin selects Microsoft's LOGIN mechanism, still common at
	// shared-hosting relays.
	AuthLogin AuthMode = "login"
	// AuthNone selects anonymous SMTP — typically loopback / LAN
	// relays with IP-level allowlists.
	AuthNone AuthMode = "none"
)

// Sink delivers events as RFC 5322-flavoured plain-text emails.
type Sink struct {
	name        string
	host        string
	port        int
	tlsMode     TLSMode
	authMode    AuthMode
	username    string
	password    string
	from        string
	to          []string
	cc          []string
	subjectPfx  string
	minSeverity output.Severity

	dialTimeout time.Duration

	// dialFn is the connection establisher; tests substitute via the
	// OverrideDialer hook (test-only file). Production reads
	// (&net.Dialer{Timeout: dialTimeout}).Dial.
	dialFn func(network, address string) (net.Conn, error)

	mu     sync.Mutex
	closed bool
}

// NewFromSpec is the SinkBuilder.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	host, err := output.SinkConfigString(spec.Config, "smtp_host")
	if err != nil {
		return nil, err
	}
	if host == "" {
		return nil, errors.New("email: config.smtp_host is required")
	}

	// smtp_port is permissive: YAML can produce int / int64 / float64
	// / string depending on how the operator wrote it. Resolve all
	// shapes here BEFORE consulting any string-typed helper (which
	// would error out on non-string).
	port := 587
	if v, ok := spec.Config["smtp_port"]; ok {
		switch n := v.(type) {
		case int:
			port = n
		case int64:
			port = int(n)
		case float64:
			port = int(n)
		case string:
			if n != "" {
				p, perr := strconv.Atoi(n)
				if perr != nil {
					return nil, fmt.Errorf("email: smtp_port %q: %w", n, perr)
				}
				port = p
			}
		default:
			return nil, fmt.Errorf("email: smtp_port: expected int or string, got %T", v)
		}
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("email: smtp_port %d out of range", port)
	}

	tlsStr, err := output.SinkConfigStringDefault(spec.Config, "tls", string(TLSStartTLS))
	if err != nil {
		return nil, err
	}
	var tlsMode TLSMode
	switch TLSMode(tlsStr) {
	case TLSStartTLS, TLSImplicit, TLSNone:
		tlsMode = TLSMode(tlsStr)
	default:
		return nil, fmt.Errorf("email: unknown tls mode %q (allowed: starttls, implicit, none)", tlsStr)
	}

	authStr, err := output.SinkConfigStringDefault(spec.Config, "auth", string(AuthPlain))
	if err != nil {
		return nil, err
	}
	var authMode AuthMode
	switch AuthMode(authStr) {
	case AuthPlain, AuthLogin, AuthNone:
		authMode = AuthMode(authStr)
	default:
		return nil, fmt.Errorf("email: unknown auth mode %q (allowed: plain, login, none)", authStr)
	}

	username, err := output.SinkConfigString(spec.Config, "username")
	if err != nil {
		return nil, err
	}
	password, err := output.SinkConfigString(spec.Config, "password")
	if err != nil {
		return nil, err
	}
	if authMode != AuthNone {
		if username == "" || password == "" {
			return nil, fmt.Errorf("email: auth=%s requires username and password", authMode)
		}
	}

	from, err := output.SinkConfigString(spec.Config, "from")
	if err != nil {
		return nil, err
	}
	if from == "" {
		return nil, errors.New("email: config.from is required")
	}

	to, err := readStringList(spec.Config, "to")
	if err != nil {
		return nil, err
	}
	if len(to) == 0 {
		return nil, errors.New("email: config.to must list ≥1 recipient")
	}
	cc, err := readStringList(spec.Config, "cc")
	if err != nil {
		return nil, err
	}

	pfx, err := output.SinkConfigStringDefault(spec.Config, "subject_prefix", "[pg_hardstorage]")
	if err != nil {
		return nil, err
	}

	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "error")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("email: %w", perr)
	}

	dialTimeout := 15 * time.Second
	dialer := &net.Dialer{Timeout: dialTimeout}
	return &Sink{
		name:        spec.Name,
		host:        host,
		port:        port,
		tlsMode:     tlsMode,
		authMode:    authMode,
		username:    username,
		password:    password,
		from:        from,
		to:          to,
		cc:          cc,
		subjectPfx:  pfx,
		minSeverity: minSev,
		dialTimeout: dialTimeout,
		dialFn:      dialer.Dial,
	}, nil
}

// readStringList parses a YAML list-of-strings (or a single string,
// for ergonomic single-recipient configs).
func readStringList(cfg map[string]any, key string) ([]string, error) {
	v, ok := cfg[key]
	if !ok {
		return nil, nil
	}
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		// YAML lets `to: ops@acme.com` mean a single recipient.
		if x == "" {
			return nil, nil
		}
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("email: config.%s contains non-string item %T", key, item)
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return append([]string(nil), x...), nil
	}
	return nil, fmt.Errorf("email: config.%s: expected list of strings, got %T", key, v)
}

// Name implements output.Sink.
func (s *Sink) Name() string { return s.name }

// Open implements output.Sink. SMTP connections are per-Emit (mail
// servers expect quick conversations and cap idle), so Open is a no-op.
func (s *Sink) Open(_ context.Context, _ map[string]any) error { return nil }

// Close implements output.Sink.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Emit implements output.Sink.
func (s *Sink) Emit(ctx context.Context, ev *output.Event) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("email: sink closed")
	}
	s.mu.Unlock()

	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	// Honour ctx cancellation BEFORE we open a TCP connection — the
	// dialer's Timeout is not the same as ctx (per the syslog and
	// HTTP sink reviews).
	if err := ctx.Err(); err != nil {
		return err
	}

	subject := s.formatSubject(ev)
	body := formatBody(ev)
	frame := s.frame(subject, body)

	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	conn, err := s.openConn(addr)
	if err != nil {
		return fmt.Errorf("email: dial %s: %w", addr, err)
	}
	// net/smtp.NewClient takes the conn; on success the client owns it.
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("email: smtp handshake: %w", err)
	}
	defer func() { _ = c.Close() }()

	if s.tlsMode == TLSStartTLS {
		// EHLO sent implicitly by NewClient, but we re-EHLO after
		// STARTTLS to pick up the post-TLS extensions.
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("email: STARTTLS requested but server doesn't advertise it")
		}
		if err := c.StartTLS(&tls.Config{ServerName: s.host}); err != nil {
			return fmt.Errorf("email: STARTTLS: %w", err)
		}
	}

	if err := s.authenticate(c); err != nil {
		return err
	}

	if err := c.Mail(s.from); err != nil {
		return fmt.Errorf("email: MAIL FROM: %w", err)
	}
	for _, rcpt := range s.to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("email: RCPT TO %s: %w", rcpt, err)
		}
	}
	for _, rcpt := range s.cc {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("email: RCPT TO (cc) %s: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("email: DATA: %w", err)
	}
	if _, err := w.Write([]byte(frame)); err != nil {
		_ = w.Close()
		return fmt.Errorf("email: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: close DATA: %w", err)
	}
	return c.Quit()
}

// openConn dials the SMTP server, wrapping in implicit TLS when
// configured. STARTTLS upgrades happen AFTER NewClient, so this
// function returns a plaintext conn for that mode.
func (s *Sink) openConn(addr string) (net.Conn, error) {
	if s.tlsMode == TLSImplicit {
		// Dial then wrap in TLS. We use net.Dialer (or test
		// override) for the underlying dial so dial-timeout
		// semantics match the other modes.
		raw, err := s.dialFn("tcp", addr)
		if err != nil {
			return nil, err
		}
		tconn := tls.Client(raw, &tls.Config{ServerName: s.host})
		if err := tconn.Handshake(); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		return tconn, nil
	}
	return s.dialFn("tcp", addr)
}

// authenticate runs the operator-chosen auth mechanism. AuthNone is a
// no-op (some relays trust loopback or peer IP).
func (s *Sink) authenticate(c *smtp.Client) error {
	switch s.authMode {
	case AuthNone:
		return nil
	case AuthPlain:
		auth := smtp.PlainAuth("", s.username, s.password, s.host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("email: PLAIN auth: %w", err)
		}
	case AuthLogin:
		// stdlib net/smtp doesn't ship a LOGIN auth; provide one
		// inline. LOGIN is the same exchange as PLAIN with two
		// challenge prompts — minimal, unencrypted-in-flight (so
		// only safe over a TLS-wrapped channel).
		auth := loginAuth{username: s.username, password: s.password, host: s.host}
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("email: LOGIN auth: %w", err)
		}
	}
	return nil
}

// loginAuth implements smtp.Auth for the LOGIN SASL mechanism.
type loginAuth struct{ username, password, host string }

// Start implements smtp.Auth. Refuses to advertise LOGIN unless the
// connection is TLS-wrapped — LOGIN sends credentials in cleartext
// over the SASL channel.
func (a loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	// LOGIN is dangerous over plaintext — refuse unless the connection
	// is TLS-wrapped (server.TLS reflects the active state of the
	// underlying conn). The server.TLS field comes from the smtp client
	// after a successful StartTLS or implicit-TLS dial.
	if !server.TLS {
		return "", nil, errors.New("email: LOGIN auth requires TLS (starttls or implicit)")
	}
	return "LOGIN", nil, nil
}

// Next implements smtp.Auth by replying to the server's "Username:"
// and "Password:" challenges in the LOGIN exchange.
func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:":
		return []byte(a.username), nil
	case "password:":
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("email: LOGIN auth: unexpected challenge %q", fromServer)
}

// formatSubject builds the RFC 5322 Subject line. Truncate at a
// reasonable length so mail clients don't elide the operator-relevant
// suffix when the body is most useful.
func (s *Sink) formatSubject(ev *output.Event) string {
	parts := []string{strings.ToUpper(ev.SeverityName), ev.Component + "/" + ev.Op}
	if ev.Subject.Deployment != "" {
		parts = append(parts, "deployment="+ev.Subject.Deployment)
	}
	subj := strings.Join(parts, " ")
	if s.subjectPfx != "" {
		subj = s.subjectPfx + " " + subj
	}
	const maxLen = 200
	if len(subj) > maxLen {
		subj = subj[:maxLen]
	}
	return subj
}

// frame assembles the RFC 5322 message: headers + blank line + body.
// CRLFs everywhere because the SMTP DATA command is line-oriented.
func (s *Sink) frame(subject, body string) string {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "From: %s\r\n", s.from)
	fmt.Fprintf(bw, "To: %s\r\n", strings.Join(s.to, ", "))
	if len(s.cc) > 0 {
		fmt.Fprintf(bw, "Cc: %s\r\n", strings.Join(s.cc, ", "))
	}
	fmt.Fprintf(bw, "Subject: %s\r\n", subject)
	fmt.Fprintf(bw, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprint(bw, "MIME-Version: 1.0\r\n")
	fmt.Fprint(bw, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprint(bw, "Content-Transfer-Encoding: 8bit\r\n")
	fmt.Fprint(bw, "\r\n") // header / body separator
	// Body — same vocabulary as the JIRA / syslog shapes so an
	// operator reading email + Slack about the same incident sees
	// matching content.
	for _, line := range strings.Split(body, "\n") {
		// SMTP "dot stuffing" — lines beginning with `.` need a
		// leading dot per RFC 5321 §4.5.2 to avoid being
		// interpreted as the end-of-data marker. net/textproto's
		// Data() Writer handles this for us, but applying it here
		// keeps `frame` self-contained for tests.
		if strings.HasPrefix(line, ".") {
			line = "." + line
		}
		fmt.Fprintf(bw, "%s\r\n", line)
	}
	return bw.String()
}

// formatBody renders the event into a plain-text body. Same shape as
// the jira sink — operators reading the same event across surfaces
// see matching content.
func formatBody(ev *output.Event) string {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "Severity: %s\n", strings.ToUpper(ev.SeverityName))
	fmt.Fprintf(bw, "Component: %s\n", ev.Component)
	fmt.Fprintf(bw, "Op: %s\n", ev.Op)
	fmt.Fprintf(bw, "At: %s\n", ev.GeneratedAt.UTC().Format(time.RFC3339))
	if ev.Subject.Deployment != "" {
		fmt.Fprintf(bw, "Deployment: %s\n", ev.Subject.Deployment)
	}
	if ev.Subject.BackupID != "" {
		fmt.Fprintf(bw, "Backup: %s\n", ev.Subject.BackupID)
	}
	if ev.Subject.Timeline != 0 {
		fmt.Fprintf(bw, "Timeline: %d\n", ev.Subject.Timeline)
	}
	if ev.Subject.LSN != "" {
		fmt.Fprintf(bw, "LSN: %s\n", ev.Subject.LSN)
	}
	if ev.Body != nil {
		fmt.Fprintf(bw, "Body: %v\n", ev.Body)
	}
	if ev.Suggestion != nil {
		if ev.Suggestion.Human != "" {
			fmt.Fprintf(bw, "\nSuggestion: %s\n", ev.Suggestion.Human)
		}
		if ev.Suggestion.Command != "" {
			fmt.Fprintf(bw, "Command: %s\n", ev.Suggestion.Command)
		}
		if ev.Suggestion.DocURL != "" {
			fmt.Fprintf(bw, "Runbook: %s\n", ev.Suggestion.DocURL)
		}
	}
	return strings.TrimRight(bw.String(), "\n")
}

// Compile-time check: Sink implements output.Sink.
var _ output.Sink = (*Sink)(nil)
