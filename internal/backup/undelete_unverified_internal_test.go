package backup

// undelete_unverified_internal_test.go — "I could not check" must not
// be reported as "I checked and it was fine".
//
// Undelete re-verifies the manifest's chunks AFTER the marker flip,
// because the pre-flight ran while the backup was still hidden and a
// concurrent `repo gc --apply` sweeping in that window is the entire
// reason the second check exists (see undelete_race_internal_test.go).
//
// CheckChunkExistence is deliberately careful about the difference
// between "the chunk is gone" and "I cannot reach the backend" — on a
// real backend error it refuses to guess, with the comment "answering
// missing would be wrong". Undelete then swallowed that distinction:
//
//	recheck, rcErr := ms.recheckResurrected(ctx, m)
//	if rcErr != nil {
//		// Transient verification failure is not evidence of loss
//		return true, nil
//	}
//
// so a backend error came back as a plain, fully-verified success. The
// operator saw restored=true, the audit chain recorded a resurrection
// indistinguishable from a checked one, and nothing anywhere said the
// integrity gate had been skipped. The justification — "the chunks were
// present moments ago" — is the pre-flight's evidence, which is exactly
// the reasoning the surrounding comment rejects.
//
// The manifest still stays live: rolling back means another write to
// the backend that just failed, and a half-undone undelete is worse
// than a visible one. What changes is that the caller is told.

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/faultinject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

var errSimulatedOutage = errors.New("simulated backend outage")

// unverifiedWorld is raceWorld with a fault-injection layer between the
// store and the filesystem, so a Stat storm can be switched on at an
// exact moment.
func unverifiedWorld(t *testing.T) (*ManifestStore, *faultinject.Middleware, storage.StoragePlugin, *Signer) {
	t.Helper()
	inner := &fs.Plugin{}
	if err := inner.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	fi := faultinject.New(inner)

	priv, _, err := GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	return NewManifestStore(fi), fi, fi, signer
}

func TestUndelete_BackendErrorDuringRecheck_ReportsUnverified(t *testing.T) {
	store, fi, sp, signer := unverifiedWorld(t)
	body := []byte("undelete-unverified-chunk")
	m := commitRaceManifest(t, store, sp, signer, "db1.full.U", body)

	if err := store.SoftDelete(context.Background(), "db1", m.BackupID, "manual", "mistake"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	// The backend goes down in the exact window: after the marker flip,
	// before the post-flip re-verification. Only chunk Stats fail, so
	// everything up to that point behaves normally.
	fired := false
	undeleteTestHookAfterUnmark = func() {
		fired = true
		fi.Activate([]faultinject.Rule{{
			Name:      "chunk-stat-outage",
			Ops:       faultinject.OpStat,
			KeyPrefix: "chunks/",
			Err:       errSimulatedOutage,
		}}, faultinject.ActivateOptions{})
	}
	t.Cleanup(func() { undeleteTestHookAfterUnmark = nil; fi.Deactivate() })

	restored, uerr := store.Undelete(context.Background(), "db1", m.BackupID)
	if !fired {
		t.Fatal("the seam never fired; the outage was not staged and this test proves nothing")
	}
	fi.Deactivate()

	if uerr == nil {
		t.Fatalf("Undelete returned a clean success (restored=%v) although the chunk "+
			"re-check could not run.\n\nThat is the pre-flight's verdict being reused at "+
			"the visibility point — the exact reasoning this second check exists to "+
			"replace. The operator is told the backup was verified when it was not.", restored)
	}
	if !errors.Is(uerr, ErrUndeleteUnverified) {
		t.Fatalf("got %v, want ErrUndeleteUnverified", uerr)
	}
	// "Could not check" is NOT "the chunks are gone": one is a retry,
	// the other is terminal without --force.
	if errors.Is(uerr, ErrUndeleteChunksMissing) {
		t.Error("reported as chunks-missing; a backend outage is not evidence of loss and " +
			"would send the operator to --force instead of a retry")
	}
	// The manifest must be LIVE regardless. Rolling back means another
	// write to the backend that just failed.
	if !restored {
		t.Error("restored=false, but the marker was already removed — the caller would " +
			"not record a state change that did happen")
	}
	live, lerr := store.backupIsLive(context.Background(), "db1", m.BackupID)
	if lerr != nil {
		t.Fatalf("backupIsLive: %v", lerr)
	}
	if !live {
		t.Error("the backup is not live after an unverified undelete; the manifest should " +
			"stay visible and only the verification verdict should be withheld")
	}
	// The cause has to survive for the operator to act on.
	if !errors.Is(uerr, errSimulatedOutage) {
		t.Errorf("the backend error was not wrapped through: %v", uerr)
	}
}

// The healthy path must stay a plain success — the new error type must
// not leak into normal operation.
func TestUndelete_HealthyBackend_StillPlainSuccess(t *testing.T) {
	store, _, sp, signer := unverifiedWorld(t)
	body := []byte("undelete-healthy-chunk")
	m := commitRaceManifest(t, store, sp, signer, "db1.full.H", body)

	if err := store.SoftDelete(context.Background(), "db1", m.BackupID, "manual", "mistake"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	restored, uerr := store.Undelete(context.Background(), "db1", m.BackupID)
	if uerr != nil || !restored {
		t.Fatalf("healthy undelete returned (restored=%v, err=%v); want (true, nil)", restored, uerr)
	}
}
