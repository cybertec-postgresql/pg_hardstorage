package cli

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestKmsOpError_ClassifiesUnreachable pins the kms.unreachable fix: a KMS
// provider error that is a network-reachability failure is coded
// kms.unreachable (ExitUnreachable / 8 — the contract documented in
// docs/reference/exit-codes.md), while any other provider error keeps the
// operation's fallback code (generic exit). Before the fix nothing produced
// kms.unreachable, so the exitcode.go consumer arm and the documented exit-8
// contract were dead.
func TestKmsOpError_ClassifiesUnreachable(t *testing.T) {
	// A network-reachability failure → kms.unreachable → exit 8, even when a
	// fallback suggestion is supplied (the backup path passes one).
	netErr := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	misconfigHint := &output.Suggestion{Human: "check the KEKRef + --kms-config"}
	unreachable := kmsOpError(netErr, "backup: open cloud KMS for q", "backup.kms_open_failed", misconfigHint)
	if got := output.ExitCodeFor(unreachable); got != output.ExitUnreachable {
		t.Errorf("unreachable KMS error: exit = %d, want %d (ExitUnreachable)", got, output.ExitUnreachable)
	}
	oe, ok := output.AsOutputError(unreachable)
	if !ok || oe.Code != "kms.unreachable" {
		t.Fatalf("unreachable KMS error: code = %v, want kms.unreachable", oe)
	}
	// The reachability case carries its own retry hint, not the misconfig one.
	if oe.Suggestion == nil || !strings.Contains(oe.Suggestion.Human, "retry") {
		t.Errorf("unreachable KMS error: want retry suggestion, got %v", oe.Suggestion)
	}

	// A structured (non-network) provider error keeps the fallback code AND
	// the supplied fallback suggestion.
	authErr := errors.New("AccessDeniedException: not authorized to perform kms:Decrypt")
	fallback := kmsOpError(authErr, "backup: open cloud KMS for q", "backup.kms_open_failed", misconfigHint)
	if got := output.ExitCodeFor(fallback); got != output.ExitError {
		t.Errorf("non-network KMS error: exit = %d, want %d (ExitError)", got, output.ExitError)
	}
	fe, ok := output.AsOutputError(fallback)
	if !ok || fe.Code != "backup.kms_open_failed" {
		t.Fatalf("non-network KMS error: code = %v, want backup.kms_open_failed", fe)
	}
	if fe.Suggestion == nil || !strings.Contains(fe.Suggestion.Human, "KEKRef") {
		t.Errorf("non-network KMS error: want the supplied fallback suggestion, got %v", fe.Suggestion)
	}

	// nil fallback suggestion is fine (the kms verify/rotate/shred sites).
	plain := kmsOpError(authErr, "kms verify", "kms.verify_failed", nil)
	if pe, ok := output.AsOutputError(plain); !ok || pe.Code != "kms.verify_failed" || pe.Suggestion != nil {
		t.Errorf("nil-suggestion fallback: got %v", pe)
	}
}
