package cli_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

type forgerSigner struct{ s *backup.Signer }

func (f forgerSigner) Sign(p []byte) []byte          { return f.s.Sign(p) }
func (f forgerSigner) PublicKey() ed25519.PublicKey  { return f.s.PublicKey() }
func (f forgerSigner) PublicKeyPEM() ([]byte, error) { return f.s.PublicKeyPEM() }

// TestAuditVerifyBundle_SelfSignedForgeryRejected pins M43: verify-bundle
// trusted the public key shipped inside the bundle, so a bundle whose
// events were rewritten and re-signed under any fresh key verified
// clean. The signer must now be trusted: the operator's own keyring
// key by default, or keys/fingerprints passed explicitly.
func TestAuditVerifyBundle_SelfSignedForgeryRejected(t *testing.T) {
	w := newReadWorld(t)
	appendAuditEvent(t, w, "x.event", "db1", time.Now().UTC())

	priv, pubPEM, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fs, _ := backup.LoadSigner(priv)
	dir := t.TempDir()
	forged := filepath.Join(dir, "forged.tar.gz")
	f, err := os.Create(forged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.ExportBundle(context.Background(), w.sp, f, forgerSigner{fs}, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	_, errb, exit := runCLI(t, "audit", "verify-bundle", forged, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("self-signed forgery: exit = %d, want %d\n%s", exit, output.ExitVerifyFailed, errb)
	}
	if !strings.Contains(errb, "verify.bundle_untrusted_signer") {
		t.Errorf("want verify.bundle_untrusted_signer:\n%s", errb)
	}

	// Explicitly trusting the (third-party) key verifies it.
	keyFile := filepath.Join(dir, "partner.pub")
	if err := os.WriteFile(keyFile, pubPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, errb, exit := runCLI(t, "audit", "verify-bundle", forged, "--trusted-key", keyFile, "-o", "json"); exit != 0 {
		t.Fatalf("--trusted-key: exit = %d\n%s\n%s", exit, out, errb)
	}
}
