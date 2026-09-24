package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// TestVerifyBundle_UntrustedSignerRefused pins M43: the bundle ships its
// own public_key.pem and verification used that key, so anyone could
// rewrite the events, re-sign under a fresh key, and have the forgery
// verify clean. With a trust set, only a signer in it passes; a
// validly-signed bundle from anyone else fails ErrBundleSignerUntrusted.
func TestVerifyBundle_UntrustedSignerRefused(t *testing.T) {
	w := setupBundleWorld(t) // w.signer: the operator's key
	w.appendEvent(t, "x.event", "db1", time.Now().UTC())

	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forgerSigner, _ := backup.LoadSigner(priv)
	forger := signerAdapter{s: forgerSigner}

	var forged bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &forged, forger, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	trusted := audit.VerifyBundleOptions{TrustedKeys: []ed25519.PublicKey{w.signer.PublicKey()}}
	m, err := audit.VerifyBundleWithOptions(bytes.NewReader(forged.Bytes()), trusted)
	if !errors.Is(err, audit.ErrBundleSignerUntrusted) {
		t.Fatalf("self-signed forgery: err = %v, want ErrBundleSignerUntrusted", err)
	}
	if m == nil {
		t.Error("untrusted verdict should still return the manifest (signature was valid) for reporting")
	}

	// The operator's own export passes, by key and by fingerprint.
	var genuine bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &genuine, w.signer, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.VerifyBundleWithOptions(bytes.NewReader(genuine.Bytes()), trusted); err != nil {
		t.Fatalf("genuine bundle with trusted key: %v", err)
	}
	sum := sha256.Sum256(w.signer.PublicKey())
	for _, fp := range []string{hex.EncodeToString(sum[:]), hex.EncodeToString(sum[:8])} {
		if _, err := audit.VerifyBundleWithOptions(bytes.NewReader(genuine.Bytes()),
			audit.VerifyBundleOptions{TrustedFingerprints: []string{fp}}); err != nil {
			t.Errorf("genuine bundle with trusted fingerprint %s: %v", fp, err)
		}
	}
}
