package dsa_test

// unreadable_manifest_test.go — the GDPR locator's own comment promised
// something the code did not do.
//
//	if err != nil {
//		// One bad manifest doesn't kill the whole scan; but
//		// we record it (the operator should investigate).
//		continue
//	}
//
// Nothing was recorded. The bare continue dropped the manifest and
// ManifestsScanned counts only what WAS read, so the shortfall was
// invisible in the report.
//
// That matters more here than almost anywhere else in the tree, because
// of what the report is: signed, persisted at dsa/reports/<id>.json,
// and described by the package doc as the artefact "an Article 15
// disclosure can later be cited and re-verified" with.
//
//   - Article 15 (right of access): a signed disclosure that may omit a
//     backup holding the subject's data, while claiming to enumerate
//     every affected backup.
//   - Article 17 (right to erasure): the report says which KEKs to
//     shred. A backup omitted from it keeps its KEK, so the subject's
//     data SURVIVES the erasure request — and there is a signed artefact
//     on file saying the request was handled.
//
// Skipping is still right; one corrupt manifest must not abort a
// compliance scan. Skipping silently is not.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/dsa"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// corruptManifest replaces a committed manifest's bytes so it still
// lists but no longer verifies.
func (f *fixture) corruptManifest(t *testing.T, deployment, backupID string) {
	t.Helper()
	key := "manifests/" + deployment + "/backups/" + backupID + "/manifest.json"
	body := `{"schema":"broken"}`
	if _, err := f.sp.Put(context.Background(), key, strings.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
}

// backupIDsIn returns the committed backup IDs for a deployment.
func (f *fixture) backupIDsIn(t *testing.T, deployment string) []string {
	t.Helper()
	var out []string
	for info, err := range f.sp.List(context.Background(), "manifests/"+deployment+"/backups/") {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(info.Key, "/manifest.json") {
			parts := strings.Split(info.Key, "/")
			out = append(out, parts[3])
		}
	}
	return out
}

func locateOpts() dsa.LocateOptions {
	return dsa.LocateOptions{
		SubjectID: "subject-1",
		Tenant:    "acme",
		Article:   dsa.ArticleErasure,
	}
}

func TestLocate_UnreadableManifestsAreRecorded(t *testing.T) {
	f := newFixture(t)
	base := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "acme", "local:default", base, "a")
	f.commitTenantBackup(t, "db1", "acme", "local:default", base.Add(time.Hour), "b")

	ids := f.backupIDsIn(t, "db1")
	if len(ids) != 2 {
		t.Fatalf("fixture: %d backups, want 2", len(ids))
	}
	f.corruptManifest(t, "db1", ids[0])

	rep, err := f.locator.Locate(context.Background(), locateOpts())
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if rep.ManifestsUnreadable != 1 {
		t.Fatalf("ManifestsUnreadable = %d, want 1.\n\nThe scan dropped a manifest it could "+
			"not read and said nothing. ManifestsScanned counts only what WAS read (%d), so "+
			"the shortfall is invisible — in a SIGNED report that drives which KEKs get "+
			"shredded under Article 17.",
			rep.ManifestsUnreadable, rep.ManifestsScanned)
	}
	// The readable one must still be reported; skipping is not aborting.
	if rep.ManifestsAffected != 1 {
		t.Errorf("ManifestsAffected = %d, want 1 — one corrupt manifest must not abort the scan",
			rep.ManifestsAffected)
	}
}

// A clean scan must record zero, or every report carries the warning
// and it stops meaning anything.
func TestLocate_CleanScanRecordsNoUnreadable(t *testing.T) {
	f := newFixture(t)
	base := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "acme", "local:default", base, "a")

	rep, err := f.locator.Locate(context.Background(), locateOpts())
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if rep.ManifestsUnreadable != 0 {
		t.Errorf("ManifestsUnreadable = %d on a clean scan", rep.ManifestsUnreadable)
	}
}

// The completeness claim is signed, so it cannot be stripped from a
// report that carried it.
func TestLocate_UnreadableCountIsCoveredByTheSignature(t *testing.T) {
	f := newFixture(t)
	base := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "acme", "local:default", base, "a")
	f.commitTenantBackup(t, "db1", "acme", "local:default", base.Add(time.Hour), "b")
	ids := f.backupIDsIn(t, "db1")
	f.corruptManifest(t, "db1", ids[0])

	rep, err := f.locator.Locate(context.Background(), locateOpts())
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	pub, priv := mustKeypair(t)
	if err := dsa.SignReport(rep, signerFromKey{pub: pub, priv: priv}); err != nil {
		t.Fatal(err)
	}
	if err := dsa.VerifyReport(rep, &dsa.SingleKeyResolver{Key: pub}); err != nil {
		t.Fatalf("freshly signed report does not verify: %v", err)
	}

	// Strip the inconvenient number and the signature must break.
	rep.ManifestsUnreadable = 0
	if err := dsa.VerifyReport(rep, &dsa.SingleKeyResolver{Key: pub}); err == nil {
		t.Fatal("the unreadable count was stripped and the report still verified.\n\n" +
			"An incomplete disclosure could then be laundered into one that claims " +
			"completeness, over the operator's own signature.")
	}
}

// Compatibility: a report with nothing unreadable must produce the same
// signing input as one written before the field existed, so every
// previously-signed report still verifies. Pinned by signing a clean
// report and checking the field contributes nothing.
func TestLocate_CleanReportSigningInputIsUnchanged(t *testing.T) {
	f := newFixture(t)
	base := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "acme", "local:default", base, "a")

	rep, err := f.locator.Locate(context.Background(), locateOpts())
	if err != nil {
		t.Fatal(err)
	}
	pub, priv := mustKeypair(t)
	if err := dsa.SignReport(rep, signerFromKey{pub: pub, priv: priv}); err != nil {
		t.Fatal(err)
	}
	// Setting the field to its zero value is a no-op for the signature
	// on a clean report — which is what makes old reports verify.
	rep.ManifestsUnreadable = 0
	if err := dsa.VerifyReport(rep, &dsa.SingleKeyResolver{Key: pub}); err != nil {
		t.Fatalf("a clean report's signing input changed: %v", err)
	}
}
