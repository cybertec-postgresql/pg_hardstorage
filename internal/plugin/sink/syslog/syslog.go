// Package syslog implements an output.Sink emitting RFC 5424
// syslog messages.
//
// Configuration (YAML keys):
//
//	plugin: syslog
//	config:
//	  protocol: udp                      # udp | tcp | tls (default udp)
//	  address: 127.0.0.1:514             # host:port
//	  facility: local6                   # syslog facility (default local6)
//	  app_name: pg_hardstorage           # APP-NAME field
//	  hostname: ""                       # default: os.Hostname()
//	  min_severity: notice               # default
//	  timeout: 5s                        # connect / write timeout
//	  tls:                               # required when protocol=tls
//	    ca_file: /etc/ssl/siem-ca.pem    # PEM bundle of trusted CAs (optional; falls back to system roots)
//	    cert_file: /etc/ssl/client.pem   # client cert for mTLS (optional, must come with key_file)
//	    key_file: /etc/ssl/client.key    # client key for mTLS
//	    server_name: siem.acme.internal  # SNI / cert-name override (optional, defaults to address host)
//	    min_version: tls1.2              # tls1.2 | tls1.3 (default tls1.2)
//	    insecure_skip_verify: false      # opt-out of cert verification (TEST ONLY)
//
// We deliberately use RFC 5424 (the "structured-data" format), not
// the older RFC 3164. 5424 is what every modern SIEM expects, and it
// preserves enough structure (PRI / hostname / app / proc / msgid)
// that downstream pipelines don't need to re-parse our JSON body.
//
// MSG-PART is the JSON-encoded Event. Operators wanting a different
// shape (CEF, LEEF) configure a downstream transformer; the syslog
// sink itself stays neutral.
package syslog

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func init() {
	output.DefaultSinkRegistry.Register("syslog", NewFromSpec)
}

// Facility encodes the syslog facility code (0..23).
type Facility int

// Common facilities the spec lists.
const (
	FacilityKern   Facility = 0
	FacilityUser   Facility = 1
	FacilityLocal0 Facility = 16
	FacilityLocal1 Facility = 17
	FacilityLocal2 Facility = 18
	FacilityLocal3 Facility = 19
	FacilityLocal4 Facility = 20
	FacilityLocal5 Facility = 21
	FacilityLocal6 Facility = 22
	FacilityLocal7 Facility = 23
)

func parseFacility(name string) (Facility, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "local6":
		return FacilityLocal6, nil
	case "kern":
		return FacilityKern, nil
	case "user":
		return FacilityUser, nil
	case "local0":
		return FacilityLocal0, nil
	case "local1":
		return FacilityLocal1, nil
	case "local2":
		return FacilityLocal2, nil
	case "local3":
		return FacilityLocal3, nil
	case "local4":
		return FacilityLocal4, nil
	case "local5":
		return FacilityLocal5, nil
	case "local7":
		return FacilityLocal7, nil
	}
	return 0, fmt.Errorf("syslog: unknown facility %q", name)
}

// Sink emits 5424-formatted syslog messages over the configured
// transport. The connection is lazy and reconnects on transient
// failure.
type Sink struct {
	name        string
	protocol    string // "udp" | "tcp" | "tls"
	address     string
	facility    Facility
	appName     string
	hostname    string
	timeout     time.Duration
	minSeverity output.Severity
	tlsCfg      *tls.Config // nil unless protocol=="tls"

	mu     sync.Mutex
	conn   net.Conn
	closed bool
}

// NewFromSpec is the SinkBuilder.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	address, err := output.SinkConfigString(spec.Config, "address")
	if err != nil {
		return nil, err
	}
	if address == "" {
		return nil, errors.New("syslog: config.address is required (host:port)")
	}
	protocol, err := output.SinkConfigStringDefault(spec.Config, "protocol", "udp")
	if err != nil {
		return nil, err
	}
	protocol = strings.ToLower(protocol)
	switch protocol {
	case "udp", "tcp", "tls":
	default:
		return nil, fmt.Errorf("syslog: unsupported protocol %q (allowed: udp, tcp, tls)", protocol)
	}

	facName, err := output.SinkConfigStringDefault(spec.Config, "facility", "local6")
	if err != nil {
		return nil, err
	}
	facility, err := parseFacility(facName)
	if err != nil {
		return nil, err
	}

	appName, err := output.SinkConfigStringDefault(spec.Config, "app_name", "pg_hardstorage")
	if err != nil {
		return nil, err
	}
	hostname, err := output.SinkConfigString(spec.Config, "hostname")
	if err != nil {
		return nil, err
	}
	if hostname == "" {
		hostname, _ = os.Hostname()
		if hostname == "" {
			hostname = "-"
		}
	}

	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "notice")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("syslog: %w", perr)
	}

	timeoutStr, err := output.SinkConfigStringDefault(spec.Config, "timeout", "5s")
	if err != nil {
		return nil, err
	}
	timeout, perr := time.ParseDuration(timeoutStr)
	if perr != nil {
		return nil, fmt.Errorf("syslog: parse timeout %q: %w", timeoutStr, perr)
	}

	var tlsCfg *tls.Config
	if protocol == "tls" {
		tlsCfg, err = buildTLSConfig(spec.Config, address)
		if err != nil {
			return nil, err
		}
	} else if _, hasTLS := spec.Config["tls"]; hasTLS {
		// Operator wrote a `tls:` block but picked a plaintext
		// protocol — almost certainly a mistake. Fail loudly
		// instead of silently sending audit events in clear text.
		return nil, fmt.Errorf("syslog: tls config provided but protocol is %q (set protocol: tls)", protocol)
	}

	return &Sink{
		name:        spec.Name,
		protocol:    protocol,
		address:     address,
		facility:    facility,
		appName:     appName,
		hostname:    hostname,
		timeout:     timeout,
		minSeverity: minSev,
		tlsCfg:      tlsCfg,
	}, nil
}

// buildTLSConfig parses the optional `tls:` submap from spec.Config
// into a *tls.Config. Files are read and parsed eagerly so a missing
// CA or malformed cert fails NewFromSpec rather than the first emit.
func buildTLSConfig(cfg map[string]any, address string) (*tls.Config, error) {
	host, _, splitErr := net.SplitHostPort(address)
	if splitErr != nil {
		return nil, fmt.Errorf("syslog: bad address %q: %w", address, splitErr)
	}

	t := &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}

	raw, hasTLS := cfg["tls"]
	if !hasTLS {
		return t, nil
	}
	tcfg, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("syslog: config.tls: expected mapping, got %T", raw)
	}

	caFile, _ := tcfg["ca_file"].(string)
	certFile, _ := tcfg["cert_file"].(string)
	keyFile, _ := tcfg["key_file"].(string)
	serverName, _ := tcfg["server_name"].(string)
	minVersion, _ := tcfg["min_version"].(string)
	insecure, _ := tcfg["insecure_skip_verify"].(bool)

	if serverName != "" {
		t.ServerName = serverName
	}

	switch strings.ToLower(strings.TrimSpace(minVersion)) {
	case "", "tls1.2", "1.2":
		t.MinVersion = tls.VersionTLS12
	case "tls1.3", "1.3":
		t.MinVersion = tls.VersionTLS13
	default:
		return nil, fmt.Errorf("syslog: tls.min_version: unsupported %q (allowed: tls1.2, tls1.3)", minVersion)
	}

	if insecure {
		// Loud opt-out for test-only deployments. Documented as
		// such; operators picking this in production are on their
		// own.
		t.InsecureSkipVerify = true
	}

	if caFile != "" {
		body, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("syslog: tls.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(body) {
			return nil, fmt.Errorf("syslog: tls.ca_file %q: no usable certs", caFile)
		}
		t.RootCAs = pool
	}

	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, errors.New("syslog: tls.cert_file and tls.key_file must both be set for mTLS")
		}
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("syslog: load client keypair: %w", err)
		}
		t.Certificates = []tls.Certificate{cert}
	}

	return t, nil
}

// Name implements output.Sink.
func (s *Sink) Name() string { return s.name }

// Open implements output.Sink. Establishes the network connection.
// For UDP this is connect-less; for TCP/TLS we dial here and reuse
// the connection across emits.
func (s *Sink) Open(ctx context.Context, _ map[string]any) error {
	return s.dial(ctx)
}

// dial (re-)establishes the network connection. Caller must hold s.mu.
func (s *Sink) dial(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("syslog: sink closed")
	}
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}

	dialer := &net.Dialer{Timeout: s.timeout}
	var conn net.Conn
	var err error
	switch s.protocol {
	case "udp":
		conn, err = dialer.DialContext(ctx, "udp", s.address)
	case "tcp":
		conn, err = dialer.DialContext(ctx, "tcp", s.address)
	case "tls":
		// tlsCfg was built eagerly at NewFromSpec time so any
		// CA / cert misconfiguration fails before Open. Clone
		// it per-dial — tls.DialWithDialer mutates ServerName
		// when it doesn't have one, and we want the original
		// preserved for the next dial attempt.
		conn, err = tls.DialWithDialer(dialer, "tcp", s.address, s.tlsCfg.Clone())
	}
	if err != nil {
		return fmt.Errorf("syslog: dial %s/%s: %w", s.protocol, s.address, err)
	}
	s.conn = conn
	return nil
}

// Emit implements output.Sink. Builds a 5424 frame and writes it.
// On transient write failure we re-dial once and retry — TCP
// connections drop silently on idle networks more often than is
// comfortable.
func (s *Sink) Emit(ctx context.Context, ev *output.Event) error {
	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	frame, err := s.formatFrame(ev)
	if err != nil {
		return err
	}

	if err := s.write(ctx, frame); err == nil {
		return nil
	}

	// Try a reconnect-and-retry once.
	if err := s.dial(ctx); err != nil {
		return err
	}
	return s.write(ctx, frame)
}

// write sends the formatted frame on the active conn, dialing if
// necessary.
func (s *Sink) write(ctx context.Context, frame []byte) error {
	// External review pass: SetWriteDeadline reads ctx.Deadline()
	// but plain ctx.Cancel() (no deadline, just cancellation) was
	// silently honoured only if we happened to have a deadline.
	// An explicit ctx.Err() check at the top covers the
	// cancellation-without-deadline case.
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	conn := s.conn
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("syslog: sink closed")
	}
	if conn == nil {
		if err := s.dial(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		conn = s.conn
		s.mu.Unlock()
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
	} else {
		_ = conn.SetWriteDeadline(time.Now().Add(s.timeout))
	}
	_, err := conn.Write(frame)
	return err
}

// Close implements output.Sink.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	return nil
}

// formatFrame produces an RFC 5424 message for ev:
//
//	<PRI>VERSION TIMESTAMP HOSTNAME APP-NAME PROCID MSGID STRUCTURED-DATA MSG
//
// VERSION is always 1. STRUCTURED-DATA is a single SD-ELEMENT carrying
// pg_hardstorage-specific fields ([pgh@<enterprise> component="..."
// op="..." …]). MSG is the JSON-encoded event.
//
// Per-protocol framing differs:
//   - UDP datagrams are self-delimiting; no extra framing needed.
//   - TCP can use either non-transparent (newline-delimited) or
//     octet-counted (`length SP frame`) framing per RFC 6587. We use
//     octet-counted, which is what every modern SIEM expects and is
//     unambiguous in the face of embedded newlines.
func (s *Sink) formatFrame(ev *output.Event) ([]byte, error) {
	pri := int(s.facility)*8 + int(ev.Severity)
	ts := ev.GeneratedAt.UTC()
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	timestamp := ts.Format(time.RFC3339Nano)

	component := nilDash(ev.Component)
	op := nilDash(ev.Op)
	procID := nilDash(fmt.Sprintf("%d", os.Getpid()))

	body, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("syslog: marshal event: %w", err)
	}

	sd := fmt.Sprintf(`[pgh@32473 component=%q op=%q]`, component, op)

	// MSGID per RFC 5424 §6.2.7 is at most 32 PRINTUSASCII chars and
	// is meant to identify the TYPE of message (TCPIN, TLSPIN, …),
	// not the specific payload. Our component/op are richer than
	// MSGID can carry and are already in the structured-data block,
	// so we emit NILVALUE here. SIEMs that pivot on op should look
	// at the SD-PARAM, not MSGID.
	msgID := "-"

	msg := fmt.Sprintf("<%d>1 %s %s %s %s %s %s %s",
		pri, timestamp, s.hostname, s.appName, procID, msgID, sd, body)

	switch s.protocol {
	case "udp":
		return []byte(msg), nil
	case "tcp", "tls":
		// Octet-counted framing per RFC 6587 §3.4.1.
		return []byte(fmt.Sprintf("%d %s", len(msg), msg)), nil
	}
	return nil, fmt.Errorf("syslog: unknown protocol %q", s.protocol)
}

// nilDash returns "-" for empty strings, per the 5424 NILVALUE rule.
func nilDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
