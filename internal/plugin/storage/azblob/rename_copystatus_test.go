package azblob

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
)

// TestCopyStatusVerdict is the regression guard for the async-copy bug in
// RenameIfNotExists (bug 2): the old code deleted the source blob right
// after StartCopyFromURL without waiting for the async copy to reach
// Success. A pending/failed/aborted copy would then leave the destination
// absent while rename reported success — silent data loss.
//
// copyStatusVerdict is the decision point awaitCopyComplete uses to decide
// whether it is safe to delete the source. This asserts:
//   - Success   → done, no error (safe to delete)
//   - Failed    → done, error (must NOT delete)
//   - Aborted   → done, error (must NOT delete)
//   - Pending   → not done (keep polling; must NOT delete)
//   - nil/empty → not done (unknown; must NOT delete)
func TestCopyStatusVerdict(t *testing.T) {
	tests := []struct {
		name     string
		status   *blob.CopyStatusType
		wantDone bool
		wantErr  bool
	}{
		{"success", to.Ptr(blob.CopyStatusTypeSuccess), true, false},
		{"failed", to.Ptr(blob.CopyStatusTypeFailed), true, true},
		{"aborted", to.Ptr(blob.CopyStatusTypeAborted), true, true},
		{"pending", to.Ptr(blob.CopyStatusTypePending), false, false},
		{"nil", nil, false, false},
		{"empty", to.Ptr(blob.CopyStatusType("")), false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			done, err := copyStatusVerdict(tc.status)
			if done != tc.wantDone {
				t.Errorf("done = %v, want %v", done, tc.wantDone)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			// Only a Success verdict may be both terminal (done) and
			// error-free — that is the ONLY state in which the source
			// delete is allowed to proceed.
			safeToDelete := done && err == nil
			if safeToDelete && (tc.status == nil || *tc.status != blob.CopyStatusTypeSuccess) {
				t.Errorf("classified non-success status %v as safe-to-delete", tc.status)
			}
		})
	}
}
