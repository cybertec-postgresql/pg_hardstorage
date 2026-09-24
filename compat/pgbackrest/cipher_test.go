package pgbackrest

import (
	"strings"
	"testing"
)

// --repo1-cipher-type=aes-256-cbc asks for an encrypted repository.
// The shim used to forward nothing and print "passphrase is honoured",
// so the backup ran in plaintext. It must now demand native encryption
// (--encrypt: the backup fails if no KEK is configured) and never
// claim the passphrase is used.
func TestCipher_BackupRequiresNativeEncryption(t *testing.T) {
	got := captureDispatch(t)
	globalArgs = pgbackrestArgs{
		stanza: "db1", pg1Host: "h", repo1Path: "/r",
		repo1CipherType: "aes-256-cbc", repo1CipherPass: "s3cret",
	}
	native, warnings, err := mapToNativeArgs("backup", globalArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !containsArg(native, "--encrypt") {
		t.Fatalf("cipher requested but backup not forced to encrypt: %v", native)
	}
	for _, w := range warnings {
		if strings.Contains(w, "honoured") {
			t.Errorf("warning still claims the passphrase is honoured: %q", w)
		}
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "NOT used") {
		t.Errorf("warnings must say the passphrase is not used; got %q", warnings)
	}
	for _, a := range native {
		if strings.Contains(a, "s3cret") {
			t.Errorf("passphrase leaked into argv: %v", native)
		}
	}
	_ = got
}

// The flag only makes sense on verbs that write data; restores and
// archive-get decrypt automatically, and native `restore` has no
// --encrypt flag.
func TestCipher_ReadVerbsGetNoEncryptFlag(t *testing.T) {
	captureDispatch(t)
	a := pgbackrestArgs{stanza: "db1", repo1Path: "/r", repo1CipherType: "aes-256-cbc", repo1CipherPass: "x"}
	for _, verb := range []string{"restore", "wal fetch", "wal push", "list"} {
		native, _, err := mapToNativeArgs(verb, a)
		if err != nil {
			t.Fatal(err)
		}
		if containsArg(native, "--encrypt") {
			t.Errorf("%s: --encrypt must not be forwarded: %v", verb, native)
		}
	}
}

// archive-push cannot force encryption (native wal push encrypts
// whenever the deployment has a KEK); it must say so rather than
// claim the passphrase is in effect.
func TestCipher_ArchivePushWarnsHonestly(t *testing.T) {
	captureDispatch(t)
	a := pgbackrestArgs{stanza: "db1", repo1Path: "/r", repo1CipherType: "aes-256-cbc", repo1CipherPass: "x"}
	_, warnings, err := mapToNativeArgs("wal push", a)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(warnings, "\n")
	if strings.Contains(joined, "honoured") || !strings.Contains(joined, "NOT used") {
		t.Errorf("archive-push cipher warning is misleading: %q", joined)
	}
}

// stanza-create must tell the operator where encryption really comes
// from instead of initialising a repository that looks encrypted.
func TestCipher_StanzaCreateNotice(t *testing.T) {
	captureDispatch(t)
	var buf strings.Builder
	prev := stderrWriter
	stderrWriter = &buf
	t.Cleanup(func() { stderrWriter = prev })
	a := pgbackrestArgs{stanza: "db1", repo1Path: "/r", repo1CipherType: "aes-256-cbc", repo1CipherPass: "x"}
	if err := runStanzaCreate(a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "NOT used") {
		t.Errorf("stanza-create did not warn about the unused passphrase: %q", buf.String())
	}
}
