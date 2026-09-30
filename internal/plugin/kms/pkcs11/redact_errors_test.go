package pkcs11_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/pkcs11"
)

// TestParseErrors_DoNotEchoPIN pins M35: parse errors quoted the whole
// KEKRef, inline ?pin= included.
func TestParseErrors_DoNotEchoPIN(t *testing.T) {
	const pin = "73519246"
	for _, ref := range []string{
		"pkcs11:///key?pin=" + pin,        // empty token
		"pkcs11://tok/?pin=" + pin,        // empty key
		"pkcs11://tok/key\x7f?pin=" + pin, // unparseable
		"pkcsXX://tok/key?pin=" + pin,     // wrong prefix
	} {
		_, err := pkcs11.NewWithClient(ref, nil)
		if err == nil {
			t.Fatalf("%q: no error", ref)
		}
		if strings.Contains(err.Error(), pin) {
			t.Errorf("error echoes the PIN: %v", err)
		}
	}
}
