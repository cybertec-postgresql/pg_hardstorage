package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Every failure to read the --incremental-from parent became
// notfound.backup (exit 6): a parent that fails signature verification
// (possible tampering) or an unreachable repository told the operator
// the backup did not exist. Map per error class.
func TestIncrementalParentReadError_PerClass(t *testing.T) {
	cases := []struct {
		err  error
		code string
		exit output.ExitCode
	}{
		{fmt.Errorf("backup: manifest db1/p: %w", storage.ErrNotFound), "notfound.backup", output.ExitNotFound},
		{backup.ErrTombstoned, "notfound.backup_tombstoned", output.ExitNotFound},
		{fmt.Errorf("read: %w", backup.ErrBadSignature), "verify.manifest_signature", output.ExitVerifyFailed},
		{fmt.Errorf("read: %w", backup.ErrPublicKeyMismatch), "verify.manifest_signature", output.ExitVerifyFailed},
		{errors.New("s3: dial tcp: i/o timeout"), "backup.parent_read_failed", output.ExitError},
	}
	for _, tc := range cases {
		err := incrementalParentReadError("p", tc.err)
		var oe *output.Error
		if !errors.As(err, &oe) || oe.Code != tc.code {
			t.Errorf("%v → %v, want %s", tc.err, err, tc.code)
			continue
		}
		if got := output.ExitCodeFor(err); got != tc.exit {
			t.Errorf("%v → exit %d, want %d", tc.err, got, tc.exit)
		}
	}
}
