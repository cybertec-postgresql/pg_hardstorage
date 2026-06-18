package syslog_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/syslog"
)

// startTCPServer accepts one connection, reads all bytes until close,
// and returns them via a channel. We use TCP with octet-counted
// framing in the tests because UDP is racier on slow CI runners.
func startTCPServer(t *testing.T) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 1)
	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		// Octet-counted: <len> <space> <frame>. We read len, then frame.
		br := bufio.NewReader(conn)
		var sb strings.Builder
		for {
			line, err := br.ReadString(' ')
			if err != nil {
				break
			}
			lenStr := strings.TrimSpace(line)
			if lenStr == "" {
				continue
			}
			n := 0
			for _, c := range lenStr {
				if c < '0' || c > '9' {
					return
				}
				n = n*10 + int(c-'0')
			}
			buf := make([]byte, n)
			if _, err := readFull(br, buf); err != nil {
				return
			}
			sb.Write(buf)
			sb.WriteByte('\n')
		}
		out <- sb.String()
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), out
}

func readFull(r *bufio.Reader, p []byte) (int, error) {
	read := 0
	for read < len(p) {
		n, err := r.Read(p[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

func TestSyslog_TCP_OctetCountedRoundTrip(t *testing.T) {
	addr, recv := startTCPServer(t)

	s, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"protocol":     "tcp",
		"address":      addr,
		"facility":     "local6",
		"app_name":     "pg_hardstorage_test",
		"hostname":     "testhost",
		"min_severity": "info",
		"timeout":      "2s",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Open(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	ev := output.NewEvent(output.SeverityWarning, "backup", "manifest.replica_failed").
		WithSubject(output.Subject{Deployment: "db1"})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-recv:
		// Expected PRI: facility 22 (local6) << 3 + severity 4 (warning) = 180.
		if !strings.Contains(got, "<180>1 ") {
			t.Errorf("PRI/version missing or wrong; got: %s", got)
		}
		if !strings.Contains(got, "testhost pg_hardstorage_test") {
			t.Errorf("hostname / app-name not present; got: %s", got)
		}
		if !strings.Contains(got, "[pgh@32473 component=\"backup\" op=\"manifest.replica_failed\"]") {
			t.Errorf("structured-data block missing; got: %s", got)
		}
		// MSGID is NILVALUE per RFC 5424 — operator-meaningful op
		// goes in the SD block, not MSGID (where 32-char limit
		// would clip it).
		if !strings.Contains(got, " - [pgh@32473 ") {
			t.Errorf("MSGID should be NILVALUE (-) before SD block; got: %s", got)
		}
		if strings.Contains(got, " manifest.replica_failed [pgh@32473 ") {
			t.Errorf("op leaked into MSGID slot; got: %s", got)
		}
		// MSG portion is the JSON event — should round-trip.
		idx := strings.Index(got, "{")
		if idx < 0 {
			t.Fatalf("no JSON body in frame: %s", got)
		}
		jsonPart := got[idx:]
		// Strip trailing newline if any
		if nl := strings.IndexByte(jsonPart, '\n'); nl >= 0 {
			jsonPart = jsonPart[:nl]
		}
		var parsed output.Event
		if err := json.Unmarshal([]byte(jsonPart), &parsed); err != nil {
			t.Errorf("MSG body not valid Event JSON: %v\n%s", err, jsonPart)
		}
		if parsed.Op != "manifest.replica_failed" {
			t.Errorf("body Op = %q", parsed.Op)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server didn't receive a frame")
	}
}

func TestSyslog_FiltersBelowMinSeverity(t *testing.T) {
	addr, recv := startTCPServer(t)
	s, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"protocol":     "tcp",
		"address":      addr,
		"min_severity": "error",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Open(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Warning is below the error threshold; must be dropped.
	if err := s.Emit(context.Background(), output.NewEvent(output.SeverityWarning, "x", "y")); err != nil {
		t.Fatal(err)
	}
	// Force the server to flush by closing.
	s.Close()
	select {
	case got := <-recv:
		if got != "" {
			t.Errorf("warning event was sent; should have been dropped. got: %s", got)
		}
	case <-time.After(500 * time.Millisecond):
		// Server timed out reading — that's the expected outcome for "no data".
	}
}

func TestSyslog_RejectsBadFacility(t *testing.T) {
	_, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"address":  "1.2.3.4:514",
		"facility": "fortran",
	}})
	if err == nil || !strings.Contains(err.Error(), "fortran") {
		t.Errorf("expected facility error; got %v", err)
	}
}

func TestSyslog_RejectsBadProtocol(t *testing.T) {
	_, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"address":  "1.2.3.4:514",
		"protocol": "carrier-pigeon",
	}})
	if err == nil || !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Errorf("expected protocol error; got %v", err)
	}
}

func TestSyslog_RequiresAddress(t *testing.T) {
	_, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog"})
	if err == nil || !strings.Contains(err.Error(), "address") {
		t.Errorf("expected address-required error; got %v", err)
	}
}

// External review pass: syslog write read ctx.Deadline() but never
// checked ctx.Err(). A plain ctx.Cancel (no deadline, just
// cancellation) was silently honoured only when a deadline was
// also set. The fix adds an explicit ctx.Err() at the top of write.
func TestSyslog_PreCancelledCtx_RefusesEmit(t *testing.T) {
	addr, _ := startTCPServer(t)
	s, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"protocol":     "tcp",
		"address":      addr,
		"facility":     "local6",
		"app_name":     "pg_hardstorage_test",
		"min_severity": "debug",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled — no deadline involved.

	ev := output.NewEvent(output.SeverityInfo, "test", "ping")
	if err := s.Emit(ctx, ev); err == nil {
		t.Error("Emit should have honoured pre-cancelled ctx; got nil error")
	}
}

// startTLSServer mirrors startTCPServer but wraps the listener in
// TLS using the supplied cert. Caller can supply requireClientCert to
// exercise mTLS; the test rig validates against the same CA pool.
func startTLSServer(t *testing.T, serverCert tls.Certificate, clientCAs *x509.CertPool) (string, <-chan string) {
	t.Helper()
	cfg := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS12,
	}
	if clientCAs != nil {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = clientCAs
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 1)
	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		br := bufio.NewReader(conn)
		var sb strings.Builder
		for {
			line, err := br.ReadString(' ')
			if err != nil {
				break
			}
			lenStr := strings.TrimSpace(line)
			if lenStr == "" {
				continue
			}
			n := 0
			for _, c := range lenStr {
				if c < '0' || c > '9' {
					return
				}
				n = n*10 + int(c-'0')
			}
			buf := make([]byte, n)
			if _, err := readFull(br, buf); err != nil {
				return
			}
			sb.Write(buf)
			sb.WriteByte('\n')
		}
		out <- sb.String()
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), out
}

// genTestCA creates a self-signed CA + a server cert under it for
// 127.0.0.1.  Returns the CA PEM, the server tls.Certificate, and an
// optional client cert under the same CA for mTLS tests.
func genTestCA(t *testing.T, withClientCert bool) (caPEM []byte, serverCert tls.Certificate, clientCertPEM, clientKeyPEM []byte) {
	t.Helper()
	caPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pg_hardstorage syslog test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caPriv.PublicKey, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	srvPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTpl, caCert, &srvPriv.PublicKey, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	srvKeyDER, _ := x509.MarshalECPrivateKey(srvPriv)
	serverCert = tls.Certificate{
		Certificate: [][]byte{srvDER},
		PrivateKey:  srvPriv,
	}
	_ = srvKeyDER

	if withClientCert {
		cliPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cliTpl := &x509.Certificate{
			SerialNumber: big.NewInt(3),
			Subject:      pkix.Name{CommonName: "syslog-client"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		cliDER, err := x509.CreateCertificate(rand.Reader, cliTpl, caCert, &cliPriv.PublicKey, caPriv)
		if err != nil {
			t.Fatal(err)
		}
		cliKeyDER, err := x509.MarshalECPrivateKey(cliPriv)
		if err != nil {
			t.Fatal(err)
		}
		clientCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cliDER})
		clientKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: cliKeyDER})
	}
	return
}

func writeFile(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSyslog_TLS_RoundTripWithCustomCA(t *testing.T) {
	caPEM, srvCert, _, _ := genTestCA(t, false)
	addr, recv := startTLSServer(t, srvCert, nil)

	dir := t.TempDir()
	caPath := writeFile(t, dir, "ca.pem", caPEM)

	s, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"protocol":     "tls",
		"address":      addr,
		"app_name":     "pg_hardstorage_test",
		"hostname":     "testhost",
		"min_severity": "info",
		"timeout":      "2s",
		"tls": map[string]any{
			"ca_file":     caPath,
			"server_name": "127.0.0.1",
			"min_version": "tls1.2",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Open(context.Background(), nil); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Emit(context.Background(), output.NewEvent(output.SeverityWarning, "backup", "manifest.replica_failed")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	_ = s.Close()

	select {
	case got := <-recv:
		if !strings.Contains(got, "manifest.replica_failed") {
			t.Errorf("event op missing in framed message: %s", got)
		}
		var ev output.Event
		idx := strings.Index(got, "{")
		if idx < 0 {
			t.Fatalf("no JSON body: %s", got)
		}
		body := got[idx:]
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			body = body[:nl]
		}
		if err := json.Unmarshal([]byte(body), &ev); err != nil {
			t.Fatalf("body not valid Event: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive a frame")
	}
}

func TestSyslog_TLS_MutualAuth(t *testing.T) {
	caPEM, srvCert, cliCertPEM, cliKeyPEM := genTestCA(t, true)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	addr, recv := startTLSServer(t, srvCert, pool)

	dir := t.TempDir()
	caPath := writeFile(t, dir, "ca.pem", caPEM)
	certPath := writeFile(t, dir, "client.pem", cliCertPEM)
	keyPath := writeFile(t, dir, "client.key", cliKeyPEM)

	s, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"protocol":     "tls",
		"address":      addr,
		"min_severity": "info",
		"timeout":      "2s",
		"tls": map[string]any{
			"ca_file":     caPath,
			"cert_file":   certPath,
			"key_file":    keyPath,
			"server_name": "127.0.0.1",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Open(context.Background(), nil); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Emit(context.Background(), output.NewEvent(output.SeverityNotice, "audit", "x")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	_ = s.Close()

	select {
	case got := <-recv:
		if !strings.Contains(got, `op="x"`) {
			t.Errorf("expected event in mTLS frame: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive a frame under mTLS")
	}
}

func TestSyslog_TLS_RejectsTLSConfigOnPlaintextProtocol(t *testing.T) {
	_, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"protocol": "tcp",
		"address":  "127.0.0.1:514",
		"tls":      map[string]any{"ca_file": "/dev/null"},
	}})
	if err == nil || !strings.Contains(err.Error(), "tls config provided") {
		t.Errorf("expected error about tls config on plaintext protocol; got %v", err)
	}
}

func TestSyslog_TLS_RejectsClientCertWithoutKey(t *testing.T) {
	_, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"protocol": "tls",
		"address":  "127.0.0.1:6514",
		"tls": map[string]any{
			"cert_file": "/some/cert.pem",
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "must both be set") {
		t.Errorf("expected mTLS half-config error; got %v", err)
	}
}

func TestSyslog_TLS_RejectsBadCAFile(t *testing.T) {
	dir := t.TempDir()
	bad := writeFile(t, dir, "ca.pem", []byte("not a PEM"))
	_, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"protocol": "tls",
		"address":  "127.0.0.1:6514",
		"tls":      map[string]any{"ca_file": bad},
	}})
	if err == nil || !strings.Contains(err.Error(), "no usable certs") {
		t.Errorf("expected no-usable-certs error; got %v", err)
	}
}

func TestSyslog_TLS_RejectsBadMinVersion(t *testing.T) {
	_, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"protocol": "tls",
		"address":  "127.0.0.1:6514",
		"tls":      map[string]any{"min_version": "ssl3"},
	}})
	if err == nil || !strings.Contains(err.Error(), "min_version") {
		t.Errorf("expected min_version error; got %v", err)
	}
}
