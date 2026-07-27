package sandbox

import "testing"

// A pg_verifybackup "could not open .../backup_manifest" used to be
// classified as a benign skip UNCONDITIONALLY — including when the
// caller had just written backup_manifest into the datadir and the
// sandbox simply couldn't see it (bad bind-mount, remote DOCKER_HOST
// mounting a nonexistent host path as an empty dir, permissions).
// verify --full then exited 0 with a fabricated "manifest was not
// captured" reason, and scheduled verification stayed green while
// verifying nothing.
func TestClassifySkip_ManifestCapturedIsNeverASkip(t *testing.T) {
	missing := `pg_verifybackup: error: could not open file "/var/lib/postgresql/data/backup_manifest": No such file or directory`

	if classifySkip(missing, true) {
		t.Error("caller captured a manifest but the sandbox can't see it — that is an environment FAILURE, classified as benign skip")
	}
	if !classifySkip(missing, false) {
		t.Error("genuinely-uncaptured manifest should remain a benign skip")
	}
	// A non-manifest failure is never a skip regardless of capture.
	if classifySkip("pg_verifybackup: error: checksum mismatch in file \"base/1/1259\"", false) {
		t.Error("checksum mismatch misclassified as skip")
	}
}
