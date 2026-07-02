package pkcs11_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/pkcs11"
)

// fakeAESHSM implements pkcs11.Client using a real AES-GCM
// AEAD held in process memory.  It's the equivalent of a
// SoftHSM2 with a single AES-256 key, but without the cgo +
// fork mess.  Lets us drive the Provider end-to-end:
//
//   - Encrypt+Decrypt round-trip exercises the full envelope
//     framing (IV || ciphertext || tag).
//   - DestroyKey wipes the in-memory key so subsequent
//     operations fail with the right error path.
//   - DescribeKey returns plausible attributes.
type fakeAESHSM struct {
	keyLabel  string
	key       []byte
	destroyed bool

	encryptCalls int
	decryptCalls int

	forceEncryptErr  error
	forceDecryptErr  error
	forceDestroyErr  error
	forceDescribeErr error
}

func newFakeAESHSM(label string) *fakeAESHSM {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return &fakeAESHSM{keyLabel: label, key: k}
}

func (f *fakeAESHSM) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(f.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (f *fakeAESHSM) Encrypt(_ context.Context, mech pkcs11.Mechanism, label string, iv, plaintext []byte) ([]byte, error) {
	f.encryptCalls++
	if f.destroyed {
		return nil, errors.New("key destroyed")
	}
	if f.forceEncryptErr != nil {
		return nil, f.forceEncryptErr
	}
	if mech != pkcs11.MechAESGCM {
		return nil, errors.New("fake only handles aes-gcm")
	}
	if label != f.keyLabel {
		return nil, errors.New("no key with that label")
	}
	gcm, err := f.aead()
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, iv, plaintext, nil), nil
}

func (f *fakeAESHSM) Decrypt(_ context.Context, mech pkcs11.Mechanism, label string, iv, ciphertext []byte) ([]byte, error) {
	f.decryptCalls++
	if f.destroyed {
		return nil, errors.New("key destroyed")
	}
	if f.forceDecryptErr != nil {
		return nil, f.forceDecryptErr
	}
	if mech != pkcs11.MechAESGCM {
		return nil, errors.New("fake only handles aes-gcm")
	}
	if label != f.keyLabel {
		return nil, errors.New("no key with that label")
	}
	gcm, err := f.aead()
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, iv, ciphertext, nil)
}

func (f *fakeAESHSM) DestroyKey(_ context.Context, label string) error {
	if f.forceDestroyErr != nil {
		return f.forceDestroyErr
	}
	if label != f.keyLabel {
		return errors.New("no key with that label")
	}
	f.destroyed = true
	return nil
}

func (f *fakeAESHSM) DescribeKey(_ context.Context, label string) (map[string]any, error) {
	if f.forceDescribeErr != nil {
		return nil, f.forceDescribeErr
	}
	return map[string]any{
		"key_type":    uint64(0x1f), // CKK_AES per PKCS#11 v2.40
		"value_len":   uint64(len(f.key)),
		"label":       label,
		"sensitive":   true,
		"extractable": false,
	}, nil
}

func (f *fakeAESHSM) Close() error { return nil }

const sampleKEKRef = "pkcs11://prod-token/db-kek?module=/usr/lib/softhsm/libsofthsm2.so&pin=1234"

func TestProvider_WrapUnwrapRoundTrip(t *testing.T) {
	cli := newFakeAESHSM("db-kek")
	p, err := pkcs11.NewWithClient(sampleKEKRef, cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	dek := []byte("32-byte-aes-key-padded-here-OK!")
	wrapped, err := p.WrapDEK(context.Background(), dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if cli.encryptCalls != 1 {
		t.Errorf("Encrypt should fire once; saw %d", cli.encryptCalls)
	}
	// Envelope frames as `12-byte IV || GCM ciphertext+tag`.
	// Length must be at least IV(12) + tag(16) + plaintext.
	if len(wrapped) < 12+16+len(dek) {
		t.Errorf("envelope too short; got %d bytes", len(wrapped))
	}
	got, err := p.UnwrapDEK(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if string(got) != string(dek) {
		t.Errorf("round-trip lost bytes: got %q want %q", got, dek)
	}
	if cli.decryptCalls != 1 {
		t.Errorf("Decrypt should fire once; saw %d", cli.decryptCalls)
	}
}

func TestProvider_WrappedFormBeginsWithFreshIV(t *testing.T) {
	// Two consecutive wraps of identical plaintext must
	// produce different envelopes — proves the IV is freshly
	// generated each time, not reused.
	cli := newFakeAESHSM("db-kek")
	p, _ := pkcs11.NewWithClient(sampleKEKRef, cli)
	dek := []byte("0123456789abcdef0123456789abcdef")
	w1, _ := p.WrapDEK(context.Background(), dek)
	w2, _ := p.WrapDEK(context.Background(), dek)
	if string(w1) == string(w2) {
		t.Error("two wraps of identical DEK produced identical envelopes — IV reuse!")
	}
	// Both envelopes should still unwrap to the same DEK.
	for i, w := range [][]byte{w1, w2} {
		got, err := p.UnwrapDEK(context.Background(), w)
		if err != nil {
			t.Fatalf("unwrap %d: %v", i, err)
		}
		if string(got) != string(dek) {
			t.Errorf("unwrap %d: got %q want %q", i, got, dek)
		}
	}
}

func TestProvider_UnwrapTooShortWrapsSentinel(t *testing.T) {
	cli := newFakeAESHSM("db-kek")
	p, _ := pkcs11.NewWithClient(sampleKEKRef, cli)
	// 5 bytes — way under IV(12) + tag(16) minimum.
	_, err := p.UnwrapDEK(context.Background(), []byte("short"))
	if !errors.Is(err, stdkms.ErrUnwrap) {
		t.Errorf("expected ErrUnwrap on truncated envelope; got %v", err)
	}
}

func TestProvider_UnwrapAuthFailsWrapsSentinel(t *testing.T) {
	cli := newFakeAESHSM("db-kek")
	p, _ := pkcs11.NewWithClient(sampleKEKRef, cli)
	dek := []byte("dek-bytes-32-long-padded-to-here")
	wrapped, _ := p.WrapDEK(context.Background(), dek)
	// Flip a byte in the tag — Decrypt should fail
	// authentication.
	wrapped[len(wrapped)-1] ^= 0xff
	_, err := p.UnwrapDEK(context.Background(), wrapped)
	if !errors.Is(err, stdkms.ErrUnwrap) {
		t.Errorf("expected ErrUnwrap on tampered ciphertext; got %v", err)
	}
}

func TestProvider_Shred(t *testing.T) {
	cli := newFakeAESHSM("db-kek")
	p, _ := pkcs11.NewWithClient(sampleKEKRef, cli)
	if err := p.Shred(context.Background()); err != nil {
		t.Fatalf("Shred: %v", err)
	}
	if !cli.destroyed {
		t.Error("Shred should destroy the key")
	}
	// Subsequent ops should fail (key gone).
	_, err := p.WrapDEK(context.Background(), []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"))
	if err == nil {
		t.Error("WrapDEK after Shred should fail")
	}
}

func TestProvider_ShredErrorWrapsSentinel(t *testing.T) {
	cli := newFakeAESHSM("db-kek")
	cli.forceDestroyErr = errors.New("HSM operator-quorum required")
	p, _ := pkcs11.NewWithClient(sampleKEKRef, cli)
	err := p.Shred(context.Background())
	if !errors.Is(err, stdkms.ErrShredFailed) {
		t.Errorf("expected ErrShredFailed; got %v", err)
	}
}

func TestProvider_DescribeKey(t *testing.T) {
	cli := newFakeAESHSM("db-kek")
	p, _ := pkcs11.NewWithClient(sampleKEKRef, cli)
	body, err := p.DescribeKey(context.Background())
	if err != nil {
		t.Fatalf("DescribeKey: %v", err)
	}
	if body["token_label"] != "prod-token" {
		t.Errorf("token_label = %v", body["token_label"])
	}
	if body["key_label"] != "db-kek" {
		t.Errorf("key_label = %v", body["key_label"])
	}
	if body["mechanism"] != "aes-gcm" {
		t.Errorf("mechanism = %v", body["mechanism"])
	}
	if body["module_path"] != "/usr/lib/softhsm/libsofthsm2.so" {
		t.Errorf("module_path = %v", body["module_path"])
	}
	if body["sensitive"] != true {
		t.Errorf("sensitive = %v", body["sensitive"])
	}
}

func TestProvider_RSAOAEPMechRoutes(t *testing.T) {
	// RSA-OAEP just smoke-tests the mechanism routing —
	// the fakeAESHSM rejects RSA so we expect the error to
	// surface from inside the Encrypt path, NOT from the
	// envelope-framing code.
	ref := "pkcs11://prod-token/db-kek?module=/x.so&pin=1234&mech=rsa-oaep"
	cli := newFakeAESHSM("db-kek")
	p, err := pkcs11.NewWithClient(ref, cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	if p.Mechanism() != pkcs11.MechRSAOAEP {
		t.Errorf("Mechanism() = %v, want rsa-oaep", p.Mechanism())
	}
	_, err = p.WrapDEK(context.Background(), []byte("32-byte-aes-key-padded-here-OK!"))
	if err == nil {
		t.Fatal("expected RSA-OAEP wrap to fail (fake doesn't handle it)")
	}
	if !strings.Contains(err.Error(), "aes-gcm") {
		// The fake's error mentions "fake only handles
		// aes-gcm" — good signal that routing reached the
		// Client, not that envelope framing rejected the
		// call.
		t.Errorf("error should reach the Client; got %v", err)
	}
}

func TestProvider_FIPSMode(t *testing.T) {
	cli := newFakeAESHSM("db-kek")
	p, _ := pkcs11.NewWithClient(sampleKEKRef, cli, pkcs11.WithFIPSMode(true))
	if !p.FIPSMode() {
		t.Error("WithFIPSMode option not honoured")
	}
}

func TestProvider_ClosedRefuses(t *testing.T) {
	cli := newFakeAESHSM("db-kek")
	p, _ := pkcs11.NewWithClient(sampleKEKRef, cli)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WrapDEK(context.Background(), []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")); err == nil {
		t.Error("WrapDEK on closed provider should fail")
	}
	if _, err := p.UnwrapDEK(context.Background(), []byte("yyyy")); err == nil {
		t.Error("UnwrapDEK on closed provider should fail")
	}
	if err := p.Shred(context.Background()); err == nil {
		t.Error("Shred on closed provider should fail")
	}
	// Close should be idempotent.
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestProvider_KEKRefRedactsSecrets is the bug-20 regression:
// KEKRef() is stamped into manifests on untrusted-readable repo
// storage, so it must never contain the HSM PIN (or the
// pin_source path).  The redacted ref must stay a valid,
// re-parseable pkcs11:// identifier with the non-secret params
// (token, key, module, mech, slot) intact.
func TestProvider_KEKRefRedactsSecrets(t *testing.T) {
	ref := "pkcs11://prod-token/db-kek?module=/usr/lib/softhsm/libsofthsm2.so&pin=SUPERSECRET&pin_source=/etc/hsm/pin&slot=2"
	cli := newFakeAESHSM("db-kek")
	p, err := pkcs11.NewWithClient(ref, cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	got := p.KEKRef()

	if strings.Contains(got, "SUPERSECRET") || strings.Contains(strings.ToLower(got), "pin=") {
		t.Errorf("KEKRef leaks PIN: %q", got)
	}
	if strings.Contains(got, "pin_source") || strings.Contains(got, "/etc/hsm/pin") {
		t.Errorf("KEKRef leaks pin_source: %q", got)
	}
	// Non-secret params must survive so the ref stays resolvable.
	for _, want := range []string{"pkcs11://prod-token/db-kek", "module=", "slot=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("KEKRef dropped %q: %q", want, got)
		}
	}
	// The redacted ref must still parse as a valid KEKRef.
	if _, err := pkcs11.NewWithClient(got+"&pin=x", newFakeAESHSM("db-kek")); err != nil {
		t.Errorf("redacted KEKRef no longer parseable: %v", err)
	}
}

func TestParseKEKRef_Cases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		err  bool
	}{
		{"happy path", "pkcs11://prod/key?module=/x.so&pin=1234", false},
		{"with slot", "pkcs11://prod/key?module=/x.so&pin=1234&slot=2", false},
		{"rsa mech", "pkcs11://prod/key?module=/x.so&pin=1234&mech=rsa-oaep", false},
		{"label with hyphen", "pkcs11://prod-token/db-kek?module=/x.so&pin=1234", false},
		{"label with dot", "pkcs11://prod.token/db.kek?module=/x.so&pin=1234", false},
		{"wrong scheme", "vault-transit://host/k", true},
		{"empty token", "pkcs11:///key?module=/x.so&pin=1234", true},
		{"empty key", "pkcs11://prod/?module=/x.so&pin=1234", true},
		{"missing key", "pkcs11://prod?module=/x.so&pin=1234", true},
		{"bad slot", "pkcs11://prod/key?slot=not-a-number&module=/x.so&pin=1234", true},
		{"bad mech", "pkcs11://prod/key?mech=des&module=/x.so&pin=1234", true},
		{"key contains slash", "pkcs11://prod/sub/key?module=/x.so&pin=1234", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := newFakeAESHSM("db-kek")
			_, err := pkcs11.NewWithClient(c.in, cli)
			if c.err && err == nil {
				t.Errorf("expected error for %q", c.in)
			}
			if !c.err && err != nil {
				t.Errorf("unexpected error for %q: %v", c.in, err)
			}
		})
	}
}

func TestProvider_HyphenInLabels(t *testing.T) {
	// Labels with the punctuation RFC 3986 permits in host +
	// path-segment must round-trip through the URL parser
	// and reach the Client unmodified.
	ref := "pkcs11://prod-token-eu/db-kek-2026?module=/x.so&pin=1234"
	cli := newFakeAESHSM("db-kek-2026")
	p, err := pkcs11.NewWithClient(ref, cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	body, err := p.DescribeKey(context.Background())
	if err != nil {
		t.Fatalf("DescribeKey: %v", err)
	}
	if body["token_label"] != "prod-token-eu" {
		t.Errorf("token round-trip wrong: %v", body["token_label"])
	}
	if body["key_label"] != "db-kek-2026" {
		t.Errorf("key round-trip wrong: %v", body["key_label"])
	}
}

func TestProvider_RegistryRoundTrip(t *testing.T) {
	schemes := stdkms.DefaultRegistry.Schemes()
	for _, s := range schemes {
		if s == pkcs11.Scheme {
			return
		}
	}
	t.Errorf("pkcs11 not registered; schemes=%v", schemes)
}

func TestProvider_NameAndKEKRef(t *testing.T) {
	cli := newFakeAESHSM("db-kek")
	p, _ := pkcs11.NewWithClient(sampleKEKRef, cli)
	if p.Name() != "pkcs11" {
		t.Errorf("Name = %q", p.Name())
	}
	// KEKRef is stamped into manifests, so the PIN is redacted
	// (bug 20); the non-secret parts must still round-trip.
	got := p.KEKRef()
	if strings.Contains(got, "pin=") {
		t.Errorf("KEKRef must not carry the PIN: %q", got)
	}
	if !strings.HasPrefix(got, "pkcs11://prod-token/db-kek") {
		t.Errorf("KEKRef lost the token/key identity: %q", got)
	}
	if !strings.Contains(got, "module=") {
		t.Errorf("KEKRef dropped the module param: %q", got)
	}
}

// TestStubBuildRefuses asserts that on the default build
// (no -tags pkcs11), the registry's builder hands back a
// structured error rather than crashing.  The check is gated
// on Built()==false so when CI runs with -tags pkcs11 this
// test silently skips.
func TestStubBuildRefuses(t *testing.T) {
	if pkcs11.Built() {
		t.Skip("running under -tags pkcs11; stub-refusal test does not apply")
	}
	// Use the real DefaultRegistry path — that's what the
	// production binary takes.  PIN must be present or the
	// check fails earlier than the stub-refusal we want to
	// exercise.
	_, err := stdkms.DefaultRegistry.Open(
		context.Background(),
		"pkcs11://prod/db-kek?module=/x.so&pin=1234",
		map[string]any{},
	)
	if err == nil {
		t.Fatal("expected stub build to refuse Open")
	}
	if !strings.Contains(err.Error(), "-tags pkcs11") {
		t.Errorf("error should point at the rebuild step; got %v", err)
	}
}

// TestBuilder_RejectsBadMechanismFromCfg is the v27-audit F1
// regression: an invalid mechanism arriving via cfg["mechanism"]
// must be refused at Open, not deferred to the first WrapDEK
// where the surfacing is much harder to debug.  The URL-form
// parser already validates aes-gcm | rsa-oaep; this asserts
// the cfg-override path applies the same gate.
func TestBuilder_RejectsBadMechanismFromCfg(t *testing.T) {
	if pkcs11.Built() {
		t.Skip("running under -tags pkcs11; the cfg-override path needs the stub builder")
	}
	// Even though the stub will refuse for "not built", the
	// mechanism check fires first.  We assert the error
	// mentions the bad mechanism, NOT the stub-build refusal.
	_, err := stdkms.DefaultRegistry.Open(
		context.Background(),
		"pkcs11://prod/db-kek?module=/x.so&pin=1234",
		map[string]any{"mechanism": "des-cbc"},
	)
	if err == nil {
		t.Fatal("expected error for bad cfg mechanism")
	}
	if !strings.Contains(err.Error(), "des-cbc") {
		t.Errorf("error should name the bad mechanism; got %v", err)
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("error should explain why; got %v", err)
	}
}

// TestBuilder_AcceptsValidMechanismFromCfg is the positive
// counterpart: cfg["mechanism"] = "rsa-oaep" overrides the
// URL's default and reaches the realClient open path (which
// in stub builds refuses with the rebuild remediation —
// exactly the signal we want, meaning the cfg gate didn't
// reject it).
func TestBuilder_AcceptsValidMechanismFromCfg(t *testing.T) {
	if pkcs11.Built() {
		t.Skip("running under -tags pkcs11; needs the stub builder for the assertion")
	}
	_, err := stdkms.DefaultRegistry.Open(
		context.Background(),
		"pkcs11://prod/db-kek?module=/x.so&pin=1234",
		map[string]any{"mechanism": "rsa-oaep"},
	)
	if err == nil {
		t.Fatal("stub should still refuse")
	}
	// The error should be the stub-build refusal, not a
	// mechanism rejection — that signals our gate let the
	// valid value through.
	if !strings.Contains(err.Error(), "-tags pkcs11") {
		t.Errorf("expected stub-build refusal (mechanism gate let valid value through); got %v", err)
	}
}
