package sharedkey_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"iter"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/sharedkey"
)

func newSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatalf("fs.Open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// putEnvelopeManifest writes a minimal manifest JSON carrying just an
// encryption envelope — enough for sharedkey, which reads only that block.
func putEnvelopeManifest(t *testing.T, sp storage.StoragePlugin, key, kekRef string, wrapped []byte) {
	t.Helper()
	body := fmt.Sprintf(`{"schema":"x","encryption":{"scheme":"aes-256-gcm","kek_ref":%q,"wrapped_dek":%q,"envelope_version":2}}`,
		kekRef, base64.StdEncoding.EncodeToString(wrapped))
	if _, err := sp.Put(context.Background(), key, bytes.NewReader([]byte(body)), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func putRaw(t *testing.T, sp storage.StoragePlugin, key, body string) {
	t.Helper()
	if _, err := sp.Put(context.Background(), key, bytes.NewReader([]byte(body)), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// testKEK + wrap/unwrap helpers give Resolve a real local-KEK unwrapper.
func testKEK(seed byte) [encryption.KeyLen]byte {
	var k [encryption.KeyLen]byte
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

func unwrapperFor(kek [encryption.KeyLen]byte) sharedkey.Unwrapper {
	return func(wrapped []byte) ([]byte, error) {
		d, err := encryption.Unwrap(kek, wrapped)
		if err != nil {
			return nil, err
		}
		return d[:], nil
	}
}

func mustWrap(t *testing.T, kek, dek [encryption.KeyLen]byte) []byte {
	t.Helper()
	w, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	return w
}

// TestResolve_FindsInBackupManifest: the classic issue-#28 path — a wrapped
// DEK on a base-backup manifest is found and unwrapped.
func TestResolve_FindsInBackupManifest(t *testing.T) {
	sp := newSP(t)
	kek := testKEK(1)
	dek, _ := encryption.GenerateDEK()
	putEnvelopeManifest(t, sp, "manifests/db1/backups/db1.full.x/manifest.json", "local:default", mustWrap(t, kek, dek))

	res, err := sharedkey.Resolve(context.Background(), sp, "local:default", unwrapperFor(kek))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !bytes.Equal(res.DEK, dek[:]) {
		t.Errorf("DEK = %x, want %x", res.DEK, dek[:])
	}
}

// TestResolve_FindsInWALManifest is the #106 convergence path: with NO backup
// yet, a wrapped DEK recorded on a WAL segment manifest is found — so a later
// backup reuses the DEK WAL minted first.
func TestResolve_FindsInWALManifest(t *testing.T) {
	sp := newSP(t)
	kek := testKEK(7)
	dek, _ := encryption.GenerateDEK()
	putEnvelopeManifest(t, sp, "wal/db1/00000001/000000010000000000000001.json", "local:default", mustWrap(t, kek, dek))

	res, err := sharedkey.Resolve(context.Background(), sp, "local:default", unwrapperFor(kek))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !bytes.Equal(res.DEK, dek[:]) {
		t.Errorf("DEK from WAL manifest = %x, want %x", res.DEK, dek[:])
	}
}

// TestResolve_KEKRefMismatch: a wrapped DEK under a different KEK is not a
// candidate (matching across refs would mis-pair a DEK with the wrong KEK).
func TestResolve_KEKRefMismatch(t *testing.T) {
	sp := newSP(t)
	kek := testKEK(1)
	dek, _ := encryption.GenerateDEK()
	putEnvelopeManifest(t, sp, "manifests/db1/backups/db1.full.x/manifest.json", "aws-kms://alias/other", mustWrap(t, kek, dek))

	res, err := sharedkey.Resolve(context.Background(), sp, "local:default", unwrapperFor(kek))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.DEK != nil || res.SawCandidate {
		t.Errorf("mismatched KEKRef must not match: %+v", res)
	}
}

// TestResolve_SkipsUnencryptedAndReplicas: unencrypted manifests, replica
// copies, and .tmp staging objects are not candidates; the scan walks past
// them to the real envelope.
func TestResolve_SkipsUnencryptedAndReplicas(t *testing.T) {
	sp := newSP(t)
	kek := testKEK(3)
	dek, _ := encryption.GenerateDEK()
	putRaw(t, sp, "manifests/db1/backups/plain/manifest.json", `{"schema":"x"}`)
	putRaw(t, sp, "manifests/_replicas/r.manifest.json", `{"schema":"x","encryption":{"scheme":"aes-256-gcm","kek_ref":"local:default","wrapped_dek":"AAAA"}}`)
	putRaw(t, sp, "wal/db1/00000001/seg.json.tmp.abcd", `{"encryption":{"scheme":"aes-256-gcm","kek_ref":"local:default","wrapped_dek":"AAAA"}}`)
	putEnvelopeManifest(t, sp, "manifests/db1/backups/real/manifest.json", "local:default", mustWrap(t, kek, dek))

	res, err := sharedkey.Resolve(context.Background(), sp, "local:default", unwrapperFor(kek))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !bytes.Equal(res.DEK, dek[:]) {
		t.Errorf("should skip plain/replica/tmp and find the real DEK; got %+v", res)
	}
}

// TestResolve_EmptyRepo: no candidates → mint signal (DEK nil, SawCandidate
// false).
func TestResolve_EmptyRepo(t *testing.T) {
	res, err := sharedkey.Resolve(context.Background(), newSP(t), "local:default", unwrapperFor(testKEK(1)))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.DEK != nil || res.SawCandidate {
		t.Errorf("empty repo must signal mint-fresh: %+v", res)
	}
}

// TestResolve_CandidateExistsButUnwrapFails is the dedup-safety gate: a
// wrapped DEK exists but the caller's KEK can't unwrap it → SawCandidate
// true, DEK nil, so the caller FAILS rather than forking a fresh DEK.
func TestResolve_CandidateExistsButUnwrapFails(t *testing.T) {
	sp := newSP(t)
	dek, _ := encryption.GenerateDEK()
	putEnvelopeManifest(t, sp, "manifests/db1/backups/x/manifest.json", "local:default", mustWrap(t, testKEK(1), dek))

	// Resolve with a DIFFERENT KEK → unwrap fails.
	res, err := sharedkey.Resolve(context.Background(), sp, "local:default", unwrapperFor(testKEK(9)))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.DEK != nil {
		t.Error("DEK should be nil when no wrapped form unwraps")
	}
	if !res.SawCandidate {
		t.Error("SawCandidate must be true so the caller fails instead of minting a fresh DEK")
	}
}

// listErrSP injects a List error to prove a transient backend failure is
// surfaced (not mistaken for "no DEK exists").
type listErrSP struct {
	storage.StoragePlugin
	err error
}

func (s listErrSP) List(_ context.Context, _ string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) { yield(storage.ObjectInfo{}, s.err) }
}

func TestResolve_ListErrorSurfaces(t *testing.T) {
	sp := listErrSP{StoragePlugin: newSP(t), err: errors.New("boom")}
	if _, err := sharedkey.Resolve(context.Background(), sp, "local:default", unwrapperFor(testKEK(1))); err == nil {
		t.Fatal("Resolve must surface a List error, not return empty")
	}
}
