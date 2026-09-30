package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// A SoftDeleteBatch refusal (a hold or a live incremental that landed
// mid-rotation) used to surface as rotate.soft_delete_failed / exit 1,
// while `backup delete` maps the same refusals to conflict.* / exit 7;
// and a multi-deployment rotate that aborted half-way never said which
// deployments it had already rotated.
func TestRotateBatchError_ConflictsAndReportsProgress(t *testing.T) {
	applied := []rotationPerDeployment{{Deployment: "db1", Applied: 3}}

	cases := []struct {
		name string
		err  error
		code string
		exit output.ExitCode
	}{
		{"held", &backup.ManifestHeldError{Deployment: "db2", BackupID: "b7", Holder: "legal"}, "conflict.manifest_held", output.ExitConflict},
		{"chain", &backup.ChainHasLiveDescendantsError{Deployment: "db2", BackupID: "b7", Descendants: []string{"b8"}}, "conflict.chain_has_live_descendants", output.ExitConflict},
		{"other", errors.New("storage: boom"), "rotate.soft_delete_failed", output.ExitError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rotateBatchError("db2", tc.err, []string{"b5"}, applied, "file:///r")
			var oe *output.Error
			if !errors.As(err, &oe) || oe.Code != tc.code {
				t.Fatalf("code = %v, want %s", err, tc.code)
			}
			if got := output.ExitCodeFor(err); got != tc.exit {
				t.Errorf("exit = %d, want %d", got, tc.exit)
			}
			for _, want := range []string{"db1 (3 deleted)", "b5"} {
				if !strings.Contains(oe.Message, want) {
					t.Errorf("message %q does not report partial progress %q", oe.Message, want)
				}
			}
		})
	}
}
