package output

import (
	"errors"
	"fmt"
	"testing"
)

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ExitCode
	}{
		{"nil", nil, ExitOK},
		{"plain", errors.New("boom"), ExitError},
		{"usage sentinel direct", ErrUsage, ExitMisuse},
		{"usage wrapped", fmt.Errorf("oops: %w", ErrUsage), ExitMisuse},
		{"auth.denied", NewError("auth.denied", "no"), ExitAuth},
		{"auth.token_expired", NewError("auth.token_expired", "expired"), ExitAuth},
		{"usage.unknown_flag", NewError("usage.unknown_flag", "x"), ExitMisuse},
		{"preflight.disk_full", NewError("preflight.disk_full", "full"), ExitPreflight},
		{"aborted.user_declined", NewError("aborted.user_declined", "n"), ExitAborted},
		{"notfound.deployment", NewError("notfound.deployment", "x"), ExitNotFound},
		{"conflict.lease_held", NewError("conflict.lease_held", "x"), ExitConflict},
		{"storage.unreachable", NewError("storage.unreachable", "x"), ExitUnreachable},
		{"kms.unreachable", NewError("kms.unreachable", "x"), ExitUnreachable},
		{"storage.permission_denied", NewError("storage.permission_denied", "x"), ExitError},
		// Issue #99 follow-up: restore.target_* leaves are
		// conflict-class so cron-driven restores can tell a
		// config error (bad --to-lsn) apart from a transient
		// infrastructure failure by exit code alone.
		{"restore.target_unreachable", NewError("restore.target_unreachable", "x"), ExitConflict},
		{"restore.target_in_wal_gap", NewError("restore.target_in_wal_gap", "x"), ExitConflict},
		// Other restore.* leaves stay in the generic bucket;
		// only the target_* conflict subset routes specially.
		{"restore.read_manifest_failed", NewError("restore.read_manifest_failed", "x"), ExitError},
		{"verify.checksum_mismatch", NewError("verify.checksum_mismatch", "x"), ExitVerifyFailed},
		{"anomaly.detected", NewError("anomaly.detected", "x"), ExitVerifyFailed},
		{"doctor.issues_present", NewError("doctor.issues_present", "x"), ExitDoctorIssues},
		{"unknown_namespace", NewError("zzz.foo", "x"), ExitError},
		{"empty_code", NewError("", "x"), ExitError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExitCodeFor(c.err); got != c.want {
				t.Errorf("ExitCodeFor(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

func TestExitCodeFor_StructuredErrorWrappedInJoin(t *testing.T) {
	oe := NewError("auth.denied", "no")
	wrapped := errors.Join(errors.New("ctx"), oe)
	if got := ExitCodeFor(wrapped); got != ExitAuth {
		t.Errorf("got %d, want %d", got, ExitAuth)
	}
}
