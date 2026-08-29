package dsa

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

type localSigner struct{ priv ed25519.PrivateKey }

func (s localSigner) Sign(p []byte) []byte         { return ed25519.Sign(s.priv, p) }
func (s localSigner) PublicKey() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// TestVerifyReport_AcceptsLegacyDigestSignature: a DSA report signed
// before the length-prefix digest fix must still verify (24-month
// compat); new reports sign with the current digest.
func TestVerifyReport_AcceptsLegacyDigestSignature(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	pub := priv.Public().(ed25519.PublicKey)
	resolver := &SingleKeyResolver{Key: pub}

	r := &Report{
		Schema: SchemaReport, ID: "rep-legacy", Tenant: "t1",
		AffectedBackups: []AffectedBackup{
			{Deployment: "db1", BackupID: "b1", KEKRef: "aws-kms://k|weird"},
		},
	}
	canon := canonicalReportBytesWith(r, digestAffectedLegacy)
	bh := sha256.Sum256(canon)
	r.BodyHash = hex.EncodeToString(bh[:])
	r.PublicKeyFingerprint = publicKeyFingerprint(pub)
	r.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canon))

	if err := VerifyReport(r, resolver); err != nil {
		t.Fatalf("legacy-signed DSA report must still verify: %v", err)
	}

	r2 := &Report{Schema: SchemaReport, ID: "rep-new", Tenant: "t1"}
	if err := SignReport(r2, localSigner{priv: priv}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReport(r2, resolver); err != nil {
		t.Fatalf("current-signed report failed to verify: %v", err)
	}
}

// TestDigestAffected_DelimiterInjectionCannotCollide mirrors the
// integrity check: two different affected-backup lists must not share
// a digest via | / \n in a field.
func TestDigestAffected_DelimiterInjectionCannotCollide(t *testing.T) {
	a := &Report{AffectedBackups: []AffectedBackup{
		{Deployment: "db1", BackupID: "b1", KEKRef: "k\ndb1|b2|k2"},
	}}
	b := &Report{AffectedBackups: []AffectedBackup{
		{Deployment: "db1", BackupID: "b1", KEKRef: "k"},
		{Deployment: "db1", BackupID: "b2", KEKRef: "k2"},
	}}
	if digestAffected(a) == digestAffected(b) {
		t.Fatal("delimiter-injection collision in the DSA affected-backup digest")
	}
}
