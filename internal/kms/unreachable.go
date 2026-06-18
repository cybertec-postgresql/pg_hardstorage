// unreachable.go — classify a KMS provider error as a network-reachability
// failure so callers can map it to the documented `kms.unreachable` exit
// code (ExitUnreachable / 8), mirroring how pg/conn.go treats a failed
// PostgreSQL connection as `storage.unreachable`.
package kms

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
)

// IsUnreachable reports whether err is (or wraps) a network-reachability
// failure against the KMS endpoint — DNS failure, connection refused, no
// route, TLS-handshake or dial timeout, etc. — as opposed to a structured
// KMS error (wrong key, permission denied, key pending deletion).
//
// Cloud KMS SDKs surface connectivity failures in different shapes: some
// preserve a typed *net.OpError / net.Error (often nested in a *url.Error),
// others collapse the cause into an opaque message. We match both: the
// typed cause via errors.As/errors.Is, and a lowercase-substring fallback
// for the opaque case. The posture is deliberately lenient — a KMS call
// that fails with a network-class error is operationally "unreachable",
// which is the cron-contract signal an operator wants (exit 8, transient,
// retry) rather than the generic exit 1.
func IsUnreachable(err error) bool {
	if err == nil {
		return false
	}

	// Typed network causes — these unwrap through *url.Error and most SDK
	// wrappers via errors.As.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	for _, sentinel := range []error{
		syscall.ECONNREFUSED, syscall.EHOSTUNREACH, syscall.ENETUNREACH,
		syscall.ETIMEDOUT, syscall.ECONNRESET,
		context.DeadlineExceeded, os.ErrDeadlineExceeded,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	// Opaque-message fallback for SDK errors that drop the typed cause.
	msg := strings.ToLower(err.Error())
	for _, sub := range []string{
		"connection refused", "no such host", "i/o timeout",
		"network is unreachable", "no route to host",
		"tls handshake timeout", "context deadline exceeded",
		"dial tcp", "dial udp", "connection reset by peer",
		"operation timed out", "server misbehaving",
	} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}
