package dsa

// canonical_golden_test.go — the signing input for a CLEAN report is
// frozen.
//
// ManifestsUnreadable is written into the canonical bytes only when it
// is non-zero. That conditional is load-bearing for the project's
// 24-month compatibility commitment: a report with nothing unreadable
// must produce byte-identical signing input to one written before the
// field existed, so every previously-signed DSA report still verifies.
//
// Signing it unconditionally would append an int64(0) and silently
// invalidate the signature on every stored report — an Article 15
// disclosure that could no longer be re-verified, which is the one
// property those reports exist to have. That failure is invisible to
// any test that signs and verifies within one process, because both
// sides would agree on the new format; only a golden catches it.
//
// If this test fails, the signing input changed. That is allowed only
// with a deliberate schema-version bump and a migration story — not as
// a side effect of adding a field.

import (
	"encoding/hex"
	"testing"
	"time"
)

// cleanGoldenReport is a fixed report with nothing unreadable. Every
// field that feeds canonicalReportBytes is pinned.
func cleanGoldenReport() *Report {
	at := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	return &Report{
		Schema:              SchemaReport,
		ID:                  "dsa-golden-0001",
		GeneratedAt:         at,
		SubjectIDHash:       "0f0e0d0c0b0a09080706050403020100",
		Tenant:              "acme",
		Article:             ArticleErasure,
		ManifestsScanned:    3,
		ManifestsAffected:   2,
		DeploymentsAffected: 1,
		AffectedBackups: []AffectedBackup{
			{Deployment: "db1", BackupID: "db1.full.20260428T120000Z.aaaa", KEKRef: "local:default"},
			{Deployment: "db1", BackupID: "db1.full.20260428T130000Z.bbbb", KEKRef: "local:default"},
		},
	}
}

func TestCanonicalReportBytes_CleanReportGolden(t *testing.T) {
	got := hex.EncodeToString(canonicalReportBytes(cleanGoldenReport()))

	// Regenerate ONLY with a deliberate schema bump:
	//   go test ./internal/dsa/ -run CleanReportGolden -v
	// and read the "got" value from the failure message.
	const want = "70675f6861726473746f726167652e6473612e7265706f72742e63616e6f6e2e763100000000000000001c70675f6861726473746f726167652e6473612e7265706f72742e7631000000000000000f6473612d676f6c64656e2d3030303100000000000000203066306530643063306230613039303830373036303530343033303230313030000000000000000461636d65000000000000000e6172745f31375f65726173757265000000000000000018aa83829fbc800000000000000000000000000000000000000000000000000300000000000000020000000000000001befabf107e14146235c221f3247a709b755b562c191af5fba4eca3f7f39abbad"

	if got != want {
		t.Fatalf("the signing input for a CLEAN report changed.\n got: %s\nwant: %s\n\n"+
			"Every previously-signed DSA report verifies against the old bytes. Changing "+
			"them silently invalidates every stored Article 15 disclosure — the one property "+
			"those reports exist to have.", got, want)
	}
}

// The conditional itself: a clean report and the same report with the
// field explicitly zeroed must produce identical bytes, and a report
// WITH unreadable manifests must not.
func TestCanonicalReportBytes_UnreadableChangesBytesOnlyWhenNonZero(t *testing.T) {
	clean := cleanGoldenReport()
	zeroed := cleanGoldenReport()
	zeroed.ManifestsUnreadable = 0
	if a, b := canonicalReportBytes(clean), canonicalReportBytes(zeroed); string(a) != string(b) {
		t.Fatal("a zero ManifestsUnreadable perturbed the signing input; every report signed " +
			"before the field existed would stop verifying")
	}

	dirty := cleanGoldenReport()
	dirty.ManifestsUnreadable = 2
	if string(canonicalReportBytes(clean)) == string(canonicalReportBytes(dirty)) {
		t.Fatal("ManifestsUnreadable=2 produced the same signing input as 0, so the " +
			"completeness claim is not signed and can be stripped")
	}
}
