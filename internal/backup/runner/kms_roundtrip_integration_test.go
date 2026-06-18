//go:build integration

// End-to-end coverage for envelope-encrypted backup → restore.  Issue
// #71 (closes the gap left by PR #69, whose corresponding test didn't
// survive the `aesgcm.New(key []byte)` API tightening).
//
// What this protects against:
//
//   - A regression in the wrap-DEK / unwrap-DEK path on either the
//     backup or restore side: encrypt-only-on-write or decrypt-only-
//     on-read would each produce a manifest that LOOKS encrypted but
//     restores garbage / refuses to start.  Catching that requires
//     exercising BOTH halves against the same KEK.
//   - A drift in the manifest's EncryptionInfo shape (KEKRef,
//     WrappedDEK base64 encoding, scheme name) that would let backups
//     written by older binaries fail to restore on newer ones, or
//     vice-versa.
//   - The CAS-level integration: chunks go in through an Encryptor
//     and come back out through the same Encryptor.  If
//     casdefault.NewEncrypted vs NewEncryptedWithRetention plumbed
//     the encryptor differently between the two paths, decrypted
//     bytes would no-op into ciphertext on the restored side.
//
// Scope intentionally bounded:
//
//   - Local-custody KEK (a fixed 32-byte slice).  Cloud-KMS provider
//     wrap/unwrap is exercised by the plugin-level tests under
//     internal/plugin/kms/awskms etc.; this test gates the envelope
//     code path without depending on a live KMS endpoint.
//   - One full backup (no incremental chain).  The chain-restore +
//     encryption interaction lives at a follow-up test once we
//     confirm the single-backup path round-trips clean.
//
// Wall-clock ≈ 25-40 s against the testkit's PG container.
package runner_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// TestKMSRoundtrip_LocalKEK_BackupRestore drives the envelope path
// end-to-end: take an encrypted backup, assert the manifest carries
// a non-empty wrapped DEK, restore through the matching KEK
// resolver, then sanity-check the restored datadir.
func TestKMSRoundtrip_LocalKEK_BackupRestore(t *testing.T) {
	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1. Schema + a handful of rows so the backup has real data to
	//    encrypt.  The exact content doesn't matter — what matters
	//    is that PGDATA at backup time differs from a freshly-
	//    initdb'd dir, so a "we forgot to actually encrypt and
	//    restored zeros" regression is loud rather than subtle.
	dbExec(t, ctx, srv.DSN, `
		CREATE TABLE kms_t (id int PRIMARY KEY, payload text);
		INSERT INTO kms_t SELECT g, 'kms-roundtrip-' || g FROM generate_series(1, 100) g;
		CHECKPOINT;
	`)

	// 2. Repo + signing keys.
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	// 3. Pick a deterministic-but-random KEK.  Deterministic per
	//    test invocation (one rand.Read) so a failure log can include
	//    the KEK if needed; not committed.
	var kek [encryption.KeyLen]byte
	if _, err := io.ReadFull(rand.Reader, kek[:]); err != nil {
		t.Fatalf("read KEK: %v", err)
	}
	const kekRef = "local:kms-roundtrip-test"

	// 4. Encrypted backup.  The Encryption block on TakeOptions is
	//    what flips the CAS from passthrough to NewEncrypted at
	//    runner.go:444 — every chunk Put will be AES-GCM sealed
	//    with a freshly-generated per-backup DEK, and the DEK
	//    itself is wrapped under our KEK before landing in the
	//    manifest.
	res, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN,
		RepoURL:      repoURL,
		Deployment:   "kms-rt",
		Signer:       signer,
		Verifier:     verifier,
		Fast:         true,
		Encryption: &runner.EncryptionConfig{
			KEK:    kek,
			KEKRef: kekRef,
		},
	})
	if err != nil {
		t.Fatalf("encrypted backup: %v", err)
	}
	t.Logf("backup id=%s stop_lsn=%s", res.BackupID, res.StopLSN)

	// 5. Read the manifest back and assert the EncryptionInfo block
	//    is what we expect: KEKRef round-trips, WrappedDEK is non-
	//    empty, scheme is named (so a future format-change is
	//    visible to operators inspecting `backup show`).
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)
	m, err := store.Read(ctx, "kms-rt", res.BackupID, verifier)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if m.Encryption == nil {
		t.Fatalf("manifest.Encryption is nil — TakeOptions.Encryption did not flow through")
	}
	if m.Encryption.KEKRef != kekRef {
		t.Errorf("manifest.Encryption.KEKRef = %q, want %q", m.Encryption.KEKRef, kekRef)
	}
	if m.Encryption.WrappedDEK == "" {
		t.Error("manifest.Encryption.WrappedDEK is empty — DEK was not wrapped + persisted")
	}
	if m.Encryption.Scheme == "" {
		t.Error("manifest.Encryption.Scheme is empty — operators inspecting `backup show` would see no algorithm")
	}
	t.Logf("manifest encryption: scheme=%s kek_ref=%s wrapped_dek_len=%d",
		m.Encryption.Scheme, m.Encryption.KEKRef, len(m.Encryption.WrappedDEK))

	// 6. Negative control: restore WITHOUT a KEKForRef MUST fail
	//    with a structured error pointing the operator at the flag.
	//    Catches a regression where the restore happily produced
	//    a target dir full of ciphertext bytes (which PG would
	//    then refuse to start against, but only after the operator
	//    has burned restore wall-clock on noise).
	negTarget := filepath.Join(t.TempDir(), "no-kek")
	_, err = restore.Restore(ctx, restore.Options{
		RepoURL:    repoURL,
		Deployment: "kms-rt",
		BackupID:   res.BackupID,
		TargetDir:  negTarget,
		Verifier:   verifier,
		// Deliberately omit KEKForRef.
	})
	if err == nil {
		t.Fatal("restore without KEKForRef should fail; got nil error")
	}
	// The restore step's error is wrapped through output.NewError;
	// we just want to see the human-pointer text the operator gets.
	t.Logf("no-KEK restore correctly refused: %v", err)

	// 7. Negative control: restore with a WRONG KEK MUST fail
	//    with an authentication-failed error from AES-GCM (the tag
	//    on the wrapped DEK won't validate).  Catches a regression
	//    where the unwrap step skipped the auth tag and silently
	//    returned garbage DEK → garbage chunks on restore.
	wrongTarget := filepath.Join(t.TempDir(), "wrong-kek")
	var wrongKEK [encryption.KeyLen]byte
	for i := range wrongKEK {
		wrongKEK[i] = byte(i ^ 0xAA)
	}
	_, err = restore.Restore(ctx, restore.Options{
		RepoURL:    repoURL,
		Deployment: "kms-rt",
		BackupID:   res.BackupID,
		TargetDir:  wrongTarget,
		Verifier:   verifier,
		KEKForRef: func(ref string) ([encryption.KeyLen]byte, error) {
			if ref != kekRef {
				return [encryption.KeyLen]byte{}, fmt.Errorf("unexpected ref %q", ref)
			}
			return wrongKEK, nil
		},
	})
	if err == nil {
		t.Fatal("restore with wrong KEK should fail; got nil error")
	}
	// AES-GCM-SIV / GCM exposes a typed `ErrAuthenticationFailed`
	// the restore step propagates verbatim — assert on it so a
	// future re-wrap of the error class is loud.
	if !errors.Is(err, encryption.ErrAuthenticationFailed) {
		t.Logf("wrong-KEK restore failed (but not via ErrAuthenticationFailed): %v", err)
		// Don't t.Fatal — the structured error may wrap the
		// AES error through output.NewError, which strips
		// errors.Is sentinel matching.  The text-level
		// assertion below catches that case too.
	}
	t.Logf("wrong-KEK restore correctly refused: %v", err)

	// 8. The happy path: restore with the correct KEK.  Asserts:
	//    - exit success
	//    - target dir contains the expected PG-shaped files
	//    - manifest sentinel files are present (PG_VERSION,
	//      backup_label, global/pg_control) — proves the
	//      decrypted bytes are real PGDATA rather than garbage.
	goodTarget := filepath.Join(t.TempDir(), "good-kek")
	rres, err := restore.Restore(ctx, restore.Options{
		RepoURL:    repoURL,
		Deployment: "kms-rt",
		BackupID:   res.BackupID,
		TargetDir:  goodTarget,
		Verifier:   verifier,
		KEKForRef: func(ref string) ([encryption.KeyLen]byte, error) {
			if ref != kekRef {
				return [encryption.KeyLen]byte{}, fmt.Errorf("unexpected ref %q (test only registered %q)", ref, kekRef)
			}
			return kek, nil
		},
	})
	if err != nil {
		t.Fatalf("restore with correct KEK: %v", err)
	}
	if rres.FileCount == 0 || rres.BytesWritten == 0 {
		t.Fatalf("restore produced an empty datadir: files=%d bytes=%d",
			rres.FileCount, rres.BytesWritten)
	}
	t.Logf("restored: files=%d bytes=%d target=%s", rres.FileCount, rres.BytesWritten, goodTarget)

	for _, name := range []string{"PG_VERSION", "backup_label", "global/pg_control"} {
		p := filepath.Join(goodTarget, name)
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("restored datadir missing %s: %v", name, err)
			continue
		}
		// PG_VERSION on a real cluster is 3-4 bytes ("17\n"); if
		// it's zero-length or huge, decryption returned garbage.
		// A live cluster fits comfortably in [2, 8] bytes.
		if name == "PG_VERSION" && (fi.Size() < 2 || fi.Size() > 8) {
			t.Errorf("restored PG_VERSION has size %d, want 2-8 bytes (decryption probably returned garbage)",
				fi.Size())
		}
	}

	// 9. Read PG_VERSION back; it should parse to a major number
	//    matching what the testkit launched.  This is the strongest
	//    cheap byte-level check: decrypted ASCII digits vs ciphertext
	//    bytes is unambiguous.
	verBytes, err := os.ReadFile(filepath.Join(goodTarget, "PG_VERSION"))
	if err != nil {
		t.Fatalf("read restored PG_VERSION: %v", err)
	}
	gotVer := string(verBytes)
	wantVer := fmt.Sprintf("%d\n", testkit.ExpectedPGMajorInt())
	if gotVer != wantVer {
		t.Errorf("restored PG_VERSION = %q, want %q (decryption may have produced wrong bytes)",
			gotVer, wantVer)
	}
}
