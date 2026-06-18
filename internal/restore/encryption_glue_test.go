package restore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func newKEK(t *testing.T) [encryption.KeyLen]byte {
	t.Helper()
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	return kek
}

func newSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// localKEKRef is the only KEKRef the local-custody path accepts (matches
// keystore.KEKRefLocal). The cloud branch is keyed off the scheme, so any
// scheme'd ref (aws-kms://…) routes through unwrapDEK instead.
const localKEKRef = "local:default"

func TestBuildEncryptedCAS_RoundTrip(t *testing.T) {
	sp := newSP(t)
	kek := newKEK(t)
	dek, _ := encryption.GenerateDEK()
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	info := &backup.EncryptionInfo{
		Scheme:          "aes-256-gcm",
		KEKRef:          localKEKRef,
		WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
		EnvelopeVersion: 2,
	}

	// Use the writer side to put a chunk under the same DEK.
	enc, _ := aesgcm.New(dek[:])
	writeCAS := casdefault.NewEncrypted(sp, enc)
	body := []byte("the secret recipe")
	chunkInfo, err := writeCAS.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}

	// Now build a read CAS via the function under test (local path).
	readCAS, err := buildEncryptedCAS(context.Background(), sp, info, func(ref string) ([encryption.KeyLen]byte, error) {
		if ref != localKEKRef {
			return [encryption.KeyLen]byte{}, errors.New("unexpected ref")
		}
		return kek, nil
	}, nil)
	if err != nil {
		t.Fatalf("buildEncryptedCAS: %v", err)
	}
	got, err := readCAS.GetChunkBytes(context.Background(), chunkInfo.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("round-trip differs: got %q, want %q", got, body)
	}
}

// TestBuildEncryptedCAS_CloudKMS_RoundTrip pins issue #102: a backup wrapped
// with a CLOUD KMS KEK is restorable. The DEK is unwrapped server-side via
// the unwrapDEK resolver (the KEK never leaves the HSM), and the local
// kekForRef must NOT be consulted for a cloud KEKRef.
func TestBuildEncryptedCAS_CloudKMS_RoundTrip(t *testing.T) {
	sp := newSP(t)
	dek, _ := encryption.GenerateDEK()
	// For a cloud KMS the WrappedDEK is opaque to restore; the unwrapDEK
	// resolver is the source of truth. This fake "wraps" by identity.
	info := &backup.EncryptionInfo{
		Scheme:          "aes-256-gcm",
		KEKRef:          "aws-kms://arn:aws:kms:eu-central-1:123:key/abc",
		WrappedDEK:      base64.StdEncoding.EncodeToString(dek[:]),
		EnvelopeVersion: 2,
	}

	enc, _ := aesgcm.New(dek[:])
	writeCAS := casdefault.NewEncrypted(sp, enc)
	body := []byte("cloud-kms secret")
	chunkInfo, err := writeCAS.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}

	unwrapDEK := func(_ context.Context, kekRef string, w []byte) ([]byte, error) {
		if kekRef != info.KEKRef {
			return nil, fmt.Errorf("unexpected kekRef %q", kekRef)
		}
		return w, nil // fake provider: wrapped == dek
	}
	// The local resolver must not be reached for a cloud KEKRef.
	localMustNotRun := func(string) ([encryption.KeyLen]byte, error) {
		t.Error("local kekForRef must not be called for a cloud KEKRef")
		return [encryption.KeyLen]byte{}, errors.New("should not be called")
	}

	readCAS, err := buildEncryptedCAS(context.Background(), sp, info, localMustNotRun, unwrapDEK)
	if err != nil {
		t.Fatalf("buildEncryptedCAS (cloud): %v", err)
	}
	got, err := readCAS.GetChunkBytes(context.Background(), chunkInfo.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("cloud round-trip differs: got %q, want %q", got, body)
	}
}

// TestBuildEncryptedCAS_CloudKMS_Errors pins the cloud-branch error taxonomy.
func TestBuildEncryptedCAS_CloudKMS_Errors(t *testing.T) {
	sp := newSP(t)
	mk := func() *backup.EncryptionInfo {
		return &backup.EncryptionInfo{
			Scheme:     "aes-256-gcm",
			KEKRef:     "aws-kms://k",
			WrappedDEK: base64.StdEncoding.EncodeToString(make([]byte, encryption.KeyLen)),
		}
	}
	assert := func(t *testing.T, err error, code string, exit output.ExitCode) {
		t.Helper()
		if err == nil {
			t.Fatal("expected error")
		}
		if oe, ok := output.AsOutputError(err); !ok || oe.Code != code {
			t.Errorf("code = %v, want %q", err, code)
		}
		if got := output.ExitCodeFor(err); got != exit {
			t.Errorf("exit = %d, want %d", got, exit)
		}
	}

	// No cloud resolver wired → config.no_kek_resolver (ExitMisuse).
	_, err := buildEncryptedCAS(context.Background(), sp, mk(), nil, nil)
	assert(t, err, "config.no_kek_resolver", output.ExitMisuse)

	// Unreachable KMS endpoint → kms.unreachable (ExitUnreachable / 8).
	_, err = buildEncryptedCAS(context.Background(), sp, mk(), nil,
		func(_ context.Context, _ string, _ []byte) ([]byte, error) {
			return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
		})
	assert(t, err, "kms.unreachable", output.ExitUnreachable)

	// Cloud unwrap auth failure (kms.ErrUnwrap) → restore.kek_mismatch.
	_, err = buildEncryptedCAS(context.Background(), sp, mk(), nil,
		func(_ context.Context, _ string, _ []byte) ([]byte, error) {
			return nil, fmt.Errorf("decrypt: %w", kms.ErrUnwrap)
		})
	assert(t, err, "restore.kek_mismatch", output.ExitError)
}

func TestBuildEncryptedCAS_RejectsMissingResolver(t *testing.T) {
	sp := newSP(t)
	info := &backup.EncryptionInfo{
		Scheme:     "aes-256-gcm",
		KEKRef:     localKEKRef,
		WrappedDEK: base64.StdEncoding.EncodeToString(make([]byte, encryption.WrappedKeyLen)),
	}
	_, err := buildEncryptedCAS(context.Background(), sp, info, nil, nil)
	if err == nil {
		t.Fatal("expected error when resolver is nil")
	}
	oe, ok := output.AsOutputError(err)
	if !ok || oe.Code != "config.no_kek_resolver" {
		t.Errorf("expected config.no_kek_resolver code; got %v", err)
	}
	// "encrypted backup, no key supplied" is an invocation/config
	// error → ExitMisuse (2), per the buildEncryptedCAS docstring.
	// The config.* namespace alone falls through to ExitError, so the
	// error must wrap ErrUsage to land on ExitMisuse.
	if got := output.ExitCodeFor(err); got != output.ExitMisuse {
		t.Errorf("ExitCodeFor(no_kek_resolver) = %d; want ExitMisuse (%d)", got, output.ExitMisuse)
	}
}

// TestBuildEncryptedCAS_ExitCodeTaxonomy pins the documented exit code
// for every local-path buildEncryptedCAS failure mode, so a future
// error-code or namespace change can't silently shift how automation
// classifies an encrypted-restore failure.
func TestBuildEncryptedCAS_ExitCodeTaxonomy(t *testing.T) {
	sp := newSP(t)
	goodWrapped := base64.StdEncoding.EncodeToString(make([]byte, encryption.WrappedKeyLen))
	resolverOK := func(string) ([encryption.KeyLen]byte, error) {
		var k [encryption.KeyLen]byte
		return k, nil
	}
	resolverErr := func(string) ([encryption.KeyLen]byte, error) {
		var k [encryption.KeyLen]byte
		return k, errFakeKEK
	}

	cases := []struct {
		name     string
		info     *backup.EncryptionInfo
		resolver func(string) ([encryption.KeyLen]byte, error)
		wantCode string
		wantExit output.ExitCode
	}{
		{
			name:     "missing resolver",
			info:     &backup.EncryptionInfo{Scheme: "aes-256-gcm", WrappedDEK: goodWrapped},
			resolver: nil,
			wantCode: "config.no_kek_resolver",
			wantExit: output.ExitMisuse,
		},
		{
			name:     "unknown scheme",
			info:     &backup.EncryptionInfo{Scheme: "chacha20", WrappedDEK: goodWrapped},
			resolver: resolverOK,
			wantCode: "restore.unknown_scheme",
			wantExit: output.ExitError,
		},
		{
			name:     "empty scheme",
			info:     &backup.EncryptionInfo{Scheme: "", WrappedDEK: goodWrapped},
			resolver: resolverOK,
			wantCode: "restore.unknown_scheme",
			wantExit: output.ExitError,
		},
		{
			name:     "bad base64",
			info:     &backup.EncryptionInfo{Scheme: "aes-256-gcm", WrappedDEK: "!!!not base64!!!"},
			resolver: resolverOK,
			wantCode: "restore.bad_wrapped_dek",
			wantExit: output.ExitError,
		},
		{
			name:     "kek resolve failed",
			info:     &backup.EncryptionInfo{Scheme: "aes-256-gcm", WrappedDEK: goodWrapped},
			resolver: resolverErr,
			wantCode: "restore.kek_resolve_failed",
			wantExit: output.ExitError,
		},
		{
			name:     "kek mismatch (auth fail)",
			info:     &backup.EncryptionInfo{Scheme: "aes-256-gcm", WrappedDEK: goodWrapped},
			resolver: resolverOK, // valid len, wrong key → GCM auth fails
			wantCode: "restore.kek_mismatch",
			wantExit: output.ExitError,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildEncryptedCAS(context.Background(), sp, c.info, c.resolver, nil)
			if err == nil {
				t.Fatalf("expected error")
			}
			oe, ok := output.AsOutputError(err)
			if !ok || oe.Code != c.wantCode {
				t.Errorf("code = %v; want %q", err, c.wantCode)
			}
			if got := output.ExitCodeFor(err); got != c.wantExit {
				t.Errorf("ExitCodeFor = %d; want %d", got, c.wantExit)
			}
		})
	}
}

var errFakeKEK = errors.New("fake kek resolve failure")

func TestBuildEncryptedCAS_RejectsUnknownScheme(t *testing.T) {
	sp := newSP(t)
	info := &backup.EncryptionInfo{
		Scheme:     "rot13",
		WrappedDEK: base64.StdEncoding.EncodeToString(make([]byte, encryption.WrappedKeyLen)),
	}
	_, err := buildEncryptedCAS(context.Background(), sp, info, func(_ string) ([encryption.KeyLen]byte, error) {
		return [encryption.KeyLen]byte{}, nil
	}, nil)
	if err == nil {
		t.Fatal("expected error on unknown scheme")
	}
	oe, ok := output.AsOutputError(err)
	if !ok || oe.Code != "restore.unknown_scheme" {
		t.Errorf("expected restore.unknown_scheme; got %v", err)
	}
}

func TestBuildEncryptedCAS_RejectsBadBase64(t *testing.T) {
	sp := newSP(t)
	info := &backup.EncryptionInfo{
		Scheme:     "aes-256-gcm",
		WrappedDEK: "this is not base64 !!!",
	}
	_, err := buildEncryptedCAS(context.Background(), sp, info, func(_ string) ([encryption.KeyLen]byte, error) {
		return [encryption.KeyLen]byte{}, nil
	}, nil)
	if err == nil {
		t.Fatal("expected base64 error")
	}
	oe, _ := output.AsOutputError(err)
	if oe == nil || oe.Code != "restore.bad_wrapped_dek" {
		t.Errorf("expected restore.bad_wrapped_dek; got %v", err)
	}
}

func TestBuildEncryptedCAS_RejectsKEKResolverError(t *testing.T) {
	sp := newSP(t)
	kek := newKEK(t)
	dek, _ := encryption.GenerateDEK()
	wrapped, _ := encryption.Wrap(kek, dek)
	info := &backup.EncryptionInfo{
		Scheme:     "aes-256-gcm",
		KEKRef:     localKEKRef,
		WrappedDEK: base64.StdEncoding.EncodeToString(wrapped),
	}
	_, err := buildEncryptedCAS(context.Background(), sp, info, func(_ string) ([encryption.KeyLen]byte, error) {
		return [encryption.KeyLen]byte{}, errors.New("not in keyring")
	}, nil)
	if err == nil {
		t.Fatal("expected resolver error to propagate")
	}
	oe, _ := output.AsOutputError(err)
	if oe == nil || oe.Code != "restore.kek_resolve_failed" {
		t.Errorf("expected restore.kek_resolve_failed; got %v", err)
	}
	if !strings.Contains(err.Error(), "not in keyring") {
		t.Errorf("error should preserve underlying message; got %v", err)
	}
}

func TestBuildEncryptedCAS_RejectsKEKMismatch(t *testing.T) {
	sp := newSP(t)
	kekA := newKEK(t)
	kekB := newKEK(t) // different
	dek, _ := encryption.GenerateDEK()
	wrapped, _ := encryption.Wrap(kekA, dek)
	info := &backup.EncryptionInfo{
		Scheme:     "aes-256-gcm",
		KEKRef:     localKEKRef,
		WrappedDEK: base64.StdEncoding.EncodeToString(wrapped),
	}
	_, err := buildEncryptedCAS(context.Background(), sp, info, func(_ string) ([encryption.KeyLen]byte, error) {
		return kekB, nil
	}, nil)
	if err == nil {
		t.Fatal("expected unwrap mismatch")
	}
	oe, _ := output.AsOutputError(err)
	if oe == nil || oe.Code != "restore.kek_mismatch" {
		t.Errorf("expected restore.kek_mismatch; got %v", err)
	}
}
