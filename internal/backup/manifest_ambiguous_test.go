package backup_test

// A signed manifest must have exactly one meaning.
//
// ParseAndVerify does not compare the file on disk against the signed
// bytes -- it cannot, because the attestation lives inside the file. It
// parses the document into a Manifest, zeroes the attestation and
// RE-SERIALISES, then checks that reconstruction against the signature.
// Everything the struct does not represent falls out of the comparison.
//
// A repeated object key is exactly that. encoding/json binds the LAST
// occurrence, so a file whose first "backup_id" says something else
// still parses to the genuine value, re-serialises to the genuine bytes
// and verifies -- while a first-wins reader, or an operator reading the
// file, sees the other value. `verify` would call such a backup
// cryptographically sound while the document says two different things
// about which backup it is.

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// signedSample returns a signed sample manifest's on-disk bytes plus a
// verifier for it.
func signedSample(t *testing.T) ([]byte, *backup.Verifier) {
	t.Helper()
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := backup.LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	ver, err := backup.LoadVerifier(pub)
	if err != nil {
		t.Fatal(err)
	}
	m := sampleManifest()
	if err := m.Sign(signer); err != nil {
		t.Fatal(err)
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backup.ParseAndVerify(raw, ver); err != nil {
		t.Fatalf("baseline manifest must verify: %v", err)
	}
	return raw, ver
}

func TestParseAndVerify_RejectsRepeatedKeys(t *testing.T) {
	raw, ver := signedSample(t)

	cases := []struct {
		name string
		// inject rewrites the document; the result must stay valid JSON
		// and must still parse (via encoding/json) to the ORIGINAL
		// manifest, which is what makes the signature check pass.
		inject func(string) string
	}{
		{
			// The plain attack: a losing duplicate at the front. Go binds
			// the genuine trailing value; a first-wins reader binds this.
			name: "top-level backup_id shadowed from the front",
			inject: func(s string) string {
				return strings.Replace(s, `{`, `{"backup_id":"db9.full.19700101T0000Z",`, 1)
			},
		},
		{
			// Same attack wearing a hat: Go matches JSON keys to struct
			// fields case-insensitively, so this is a duplicate of
			// backup_id even though the bytes differ.
			name: "case-variant of backup_id",
			inject: func(s string) string {
				return strings.Replace(s, `{`, `{"Backup_ID":"db9.full.19700101T0000Z",`, 1)
			},
		},
		{
			// Nested objects are reached through the recursion; an
			// attacker aims at whichever object carries the field they
			// want to misrepresent.
			name: "nested duplicate inside a tablespace",
			inject: func(s string) string {
				return strings.Replace(s, `{"oid":1663,`, `{"oid":9999,"oid":1663,`, 1)
			},
		},
		{
			// Objects inside arrays are reached too.
			name: "duplicate inside a chunk ref in an array",
			inject: func(s string) string {
				return strings.Replace(s, `"offset":4096`, `"offset":0,"offset":4096`, 1)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := tc.inject(string(raw))
			if doc == string(raw) {
				t.Fatalf("injection did not fire; the sample manifest's shape changed.\n%s", raw)
			}
			if !json.Valid([]byte(doc)) {
				t.Fatalf("injected document is not valid JSON:\n%s", doc)
			}
			// Precondition that gives the test its teeth: encoding/json
			// still reads this as the genuine manifest, so WITHOUT the
			// duplicate-key check the signature verifies and the file is
			// accepted. If this ever stops holding the test is no longer
			// exercising the hole.
			var probe backup.Manifest
			if err := json.Unmarshal([]byte(doc), &probe); err != nil {
				t.Fatalf("precondition: injected doc must still unmarshal: %v", err)
			}

			if _, err := backup.ParseAndVerify([]byte(doc), ver); err == nil {
				t.Fatal("ambiguous manifest accepted: it verified against a valid " +
					"signature while carrying two values for one key, so `verify` " +
					"would call it sound and a first-wins reader would see the other value")
			}

			// Re-signing must not launder it either. `repair attestation`
			// reads through ParseAttestationless and re-signs; if that
			// path accepted the document it would mint a genuine
			// signature over an ambiguous file.
			if _, err := backup.ParseAttestationless([]byte(doc)); err == nil {
				t.Error("ParseAttestationless accepted an ambiguous manifest; " +
					"`repair attestation` would re-sign it and launder the ambiguity " +
					"into an authenticated backup record")
			}
		})
	}
}

// The clean document must still pass, unchanged -- the check is a
// rejection of ambiguity, not a new encoding requirement.
func TestParseAndVerify_CleanManifestStillVerifies(t *testing.T) {
	raw, ver := signedSample(t)
	m, err := backup.ParseAndVerify(raw, ver)
	if err != nil {
		t.Fatalf("clean manifest rejected: %v", err)
	}
	if m.BackupID != "db1.full.20260428T1200Z" {
		t.Errorf("BackupID = %q", m.BackupID)
	}
	if _, err := backup.ParseAttestationless(raw); err != nil {
		t.Errorf("ParseAttestationless rejected a clean manifest: %v", err)
	}
}

// Unknown fields are tolerated ON PURPOSE, and this pins that decision.
//
// Schema has been "pg_hardstorage.manifest.v1" since v1.0.0 and is
// exact-matched on read, yet fields have been added under it --
// PGBackupManifest and WALGaps both document themselves as additive and
// empty on older backups. Tolerating unknown fields is therefore the
// mechanism by which a v1.3 reader can still read a v1.4 manifest.
// Turning on DisallowUnknownFields would close a narrow gap (an unknown
// field is not covered by the signature) at the cost of making every
// older binary reject every newer manifest -- a far worse failure, and
// one that would surface as unreadable backups during an upgrade.
//
// If a future change does want byte-exact authentication, the way to get
// it is to sign the on-disk bytes directly (detached attestation), not
// to make the parser strict.
func TestParseAndVerify_TolerantOfUnknownFieldsByDesign(t *testing.T) {
	raw, ver := signedSample(t)
	doc := strings.Replace(string(raw), `{`, `{"field_from_a_newer_version":"x",`, 1)
	if !json.Valid([]byte(doc)) {
		t.Fatal("injected document is not valid JSON")
	}
	if _, err := backup.ParseAndVerify([]byte(doc), ver); err != nil {
		t.Fatalf("a manifest carrying a field this binary does not know was rejected: %v\n\n"+
			"Schema is exact-matched and has never been bumped, so additive fields are how "+
			"the format evolves; rejecting them makes an older binary unable to read a "+
			"newer repository's manifests.", err)
	}
}
