package kms_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
)

const secretPIN = "73519246"

// TestRegistryOpen_ErrorsDoNotEchoPIN pins M35: a pkcs11 KEKRef may
// carry the HSM PIN inline (?pin=), and Registry.Open quoted the whole
// KEKRef in its errors — which end up in logs, JSON output and audit
// events.
func TestRegistryOpen_ErrorsDoNotEchoPIN(t *testing.T) {
	r := kms.NewRegistry()
	r.Register("pkcs11", func(context.Context, string, map[string]any) (kms.Provider, error) {
		return nil, errors.New("token not present")
	})
	for _, ref := range []string{
		"pkcs11://tok/key?module=/usr/lib/softhsm.so&pin=" + secretPIN,
		"nosuch://tok/key?pin=" + secretPIN,
	} {
		_, err := r.Open(context.Background(), ref, nil)
		if err == nil {
			t.Fatalf("%s: Open succeeded", ref)
		}
		if strings.Contains(err.Error(), secretPIN) {
			t.Errorf("error echoes the PIN: %v", err)
		}
	}
}

func TestRedactKEKRef(t *testing.T) {
	for in, want := range map[string]string{
		"pkcs11://tok/key?module=/m.so&pin=" + secretPIN + "&mech=aes-gcm": "pkcs11://tok/key?module=/m.so&pin=REDACTED&mech=aes-gcm",
		"pkcs11://tok/key?PIN=" + secretPIN:                                "pkcs11://tok/key?PIN=REDACTED",
		"pkcs11://tok/key?pin_source=/etc/pin":                             "pkcs11://tok/key?pin_source=REDACTED",
		"vault-transit://u:" + secretPIN + "@10.0.0.5:8200/transit/k":      "vault-transit://u:REDACTED@10.0.0.5:8200/transit/k",
		"aws-kms://arn:aws:kms:us-east-1:123:key/abcd":                     "aws-kms://arn:aws:kms:us-east-1:123:key/abcd",
		"local:default": "local:default",
		"x://h/k?secret_id=" + secretPIN + "&token=" + secretPIN: "x://h/k?secret_id=REDACTED&token=REDACTED",
	} {
		if got := kms.RedactKEKRef(in); got != want {
			t.Errorf("RedactKEKRef(%q) = %q, want %q", in, got, want)
		}
	}
}
