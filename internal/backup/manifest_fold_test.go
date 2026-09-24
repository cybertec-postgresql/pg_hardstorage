package backup_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// The duplicate-key guard folded keys with strings.ToLower, but
// encoding/json matches keys to fields with Unicode simple folding:
// "ſ" (U+017F) folds to "s" and "K" (U+212A, Kelvin) to "k". So
// "ſchema"/"bacKup_id" slipped past the guard as distinct keys,
// Go bound them to the real fields (last wins), the signature verified,
// and every other reader saw a different value. A disguised key that
// appears ALONE is the same problem: Go binds it, a case-sensitive
// reader does not see the field at all. Every key must be the plain
// lowercase ASCII the schema writes.
func TestParseAndVerify_RejectsUnicodeFoldedKeys(t *testing.T) {
	raw, ver := signedSample(t)
	s := string(raw)
	if !strings.Contains(s, `"backup_id":"`) {
		t.Fatal("fixture shape changed")
	}
	cases := map[string]string{
		"kelvin-K duplicate in front": strings.Replace(s, `{`, "{\"bacKup_id\":\"db9.full.19700101T0000Z\",", 1),
		"long-s duplicate in front":   strings.Replace(s, `{`, "{\"ſchema\":\"evil\",", 1),
		"disguised key alone":         strings.Replace(s, `"backup_id":`, "\"bacKup_id\":", 1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := backup.ParseAndVerify([]byte(doc), ver)
			if !errors.Is(err, backup.ErrAmbiguousManifest) {
				t.Errorf("ParseAndVerify: err=%v, want ErrAmbiguousManifest", err)
			}
			if _, err := backup.ParseAttestationless([]byte(doc)); !errors.Is(err, backup.ErrAmbiguousManifest) {
				t.Errorf("ParseAttestationless: err=%v, want ErrAmbiguousManifest", err)
			}
			// VerifyEmbedded (repair attestation's self-consistency
			// gate) must apply the same checks — it would otherwise
			// re-sign the ambiguous document under the operator's key.
			if _, err := backup.VerifyEmbedded([]byte(doc)); !errors.Is(err, backup.ErrAmbiguousManifest) {
				t.Errorf("VerifyEmbedded: err=%v, want ErrAmbiguousManifest", err)
			}
		})
	}
}

// VerifyEmbedded skipped the duplicate-key AND schema checks entirely.
func TestVerifyEmbedded_RejectsPlainDuplicateAndWrongSchema(t *testing.T) {
	raw, _ := signedSample(t)
	s := string(raw)
	dup := strings.Replace(s, `{`, `{"backup_id":"db9.full.19700101T0000Z",`, 1)
	if _, err := backup.VerifyEmbedded([]byte(dup)); !errors.Is(err, backup.ErrAmbiguousManifest) {
		t.Errorf("duplicate key: err=%v, want ErrAmbiguousManifest", err)
	}
	if _, err := backup.VerifyEmbedded(raw); err != nil {
		t.Fatalf("clean manifest must pass: %v", err)
	}
}
