package verifybackup_test

// empty_manifest_test.go — a verification gate must not pass by
// checking nothing.
//
// Verify's entire body is a loop over the manifest's Files. A manifest
// that parses, carries a version, and lists zero files therefore ran to
// completion with FilesChecked=0 and no error — and both restore call
// sites turned that into a `verifybackup_ok` event carrying
// "files_checked": 0. The restore's only in-process integrity gate
// reported success over a datadir it had not looked at.
//
// The reachable path is the chain restore: it reads `backup_manifest`
// from pg_combinebackup's OUTPUT — a file produced by an external tool
// during that very run, not a signed artefact — and the gate's stated
// job is "catching a missing / truncated / corrupted file the merge
// produced". A merge broken enough to emit a degenerate manifest is
// exactly the case that slipped through.
//
// No real cluster produces a zero-file manifest, so refusing it costs
// nothing; the shape only appears when the writer is broken. An ABSENT
// manifest stays a skip (legacy backups predate the field) — the two
// must not collapse into one another.

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/verifybackup"
)

func TestVerify_EmptyFileListIsRefusedNotPassed(t *testing.T) {
	for name, body := range map[string]string{
		"empty array":        `{"PostgreSQL-Backup-Manifest-Version":1,"Files":[]}`,
		"files key absent":   `{"PostgreSQL-Backup-Manifest-Version":1}`,
		"incremental, empty": `{"PostgreSQL-Backup-Manifest-Version":1,"Files":[],"Incremental":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			res, err := verifybackup.Verify(context.Background(), []byte(body), t.TempDir())
			if err == nil {
				t.Fatalf("Verify returned OK over a manifest listing no files "+
					"(files_checked=%d) — the gate would report success having hashed nothing",
					res.FilesChecked)
			}
			if !errors.Is(err, verifybackup.ErrEmptyManifest) {
				t.Fatalf("got %v, want ErrEmptyManifest", err)
			}
		})
	}
}

// The two "nothing to verify" cases must stay distinguishable: an
// absent manifest is a legacy backup and is skipped, an empty one is a
// broken writer and fails. Collapsing them would either fail every old
// backup or re-open the vacuous pass.
func TestVerify_AbsentAndEmptyManifestsAreDifferentErrors(t *testing.T) {
	_, absent := verifybackup.Verify(context.Background(), nil, t.TempDir())
	if !errors.Is(absent, verifybackup.ErrNoManifest) {
		t.Fatalf("empty bytes: got %v, want ErrNoManifest", absent)
	}
	if errors.Is(absent, verifybackup.ErrEmptyManifest) {
		t.Fatal("ErrNoManifest also matches ErrEmptyManifest; the restore paths " +
			"treat one as a skip and the other as a failure and could not tell them apart")
	}

	_, empty := verifybackup.Verify(context.Background(),
		[]byte(`{"PostgreSQL-Backup-Manifest-Version":1,"Files":[]}`), t.TempDir())
	if errors.Is(empty, verifybackup.ErrNoManifest) {
		t.Fatal("an empty manifest reports as ErrNoManifest and would be silently skipped")
	}
}

// A manifest with no version field is still rejected on its own terms —
// the empty-files check must not shadow that.
func TestVerify_MissingVersionStillRejected(t *testing.T) {
	_, err := verifybackup.Verify(context.Background(), []byte(`{"Files":[]}`), t.TempDir())
	if err == nil {
		t.Fatal("manifest without a version field was accepted")
	}
	if errors.Is(err, verifybackup.ErrEmptyManifest) {
		t.Fatal("the empty-files check fired first; a manifest with no version is not " +
			"a PG backup_manifest at all and should say so")
	}
}
