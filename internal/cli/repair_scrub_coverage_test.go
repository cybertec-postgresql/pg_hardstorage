package cli_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

const zeroHashHex = "0000000000000000000000000000000000000000000000000000000000000000"

// commitForeignSignedManifest commits a manifest signed by a key this
// host does not trust, so every scrub skips it as unverifiable.
func commitForeignSignedManifest(t *testing.T, w *readWorld, deployment string) {
	t.Helper()
	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := backup.LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("chunk nobody will scrub")
	info, err := casdefault.New(w.sp).PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: deployment + ".full.foreign", Deployment: deployment,
		Tenant: "default", Type: backup.BackupTypeFull, PGVersion: 17,
		SystemIdentifier: "7000000000000000001", StartLSN: "0/3000028", StopLSN: "0/30001A0",
		Timeline: 1, StartedAt: ts, StoppedAt: ts.Add(time.Minute),
		BackupLabel: "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{{Path: "data", Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Len: int64(len(body))}}}},
	}
	if err := w.store.Commit(context.Background(), m, foreign, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
}

// H5: `repair scrub` counted the manifests it could not verify and never
// reported them — a scrub that skipped every manifest printed "no
// integrity failures" and exited 0.
func TestRepairScrub_UnverifiableManifestsExitNonZero(t *testing.T) {
	w := newReadWorld(t)
	commitForeignSignedManifest(t, w, "db1")

	stdout, stderr, exit := runCmd(t, "repair", "scrub", "--repo", w.repoURL, "--limit", "0", "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("exit = %d, want 9 (every manifest was skipped)\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stderr, "verify.scrub_unverifiable_manifests") {
		t.Errorf("expected verify.scrub_unverifiable_manifests:\n%s", stderr)
	}
	if !strings.Contains(stdout, `"unverifiable_manifests": 1`) {
		t.Errorf("body must carry the count:\n%s", stdout)
	}
}

// M17: an encrypted manifest whose KEK is not on this host is a
// key-coverage finding, not a zero-hash "mismatch" — and `--heal` must
// not exit 0 "all mismatches healed" over it.
func TestRepairScrub_HealWithMissingKEKIsNotClean(t *testing.T) {
	hw := newHealWorld(t)
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	// NOT installed: this host cannot unwrap the DEK.
	commitEncryptedBackup(t, hw.readWorld, "db1", "nokek", 1, kek, "local:default", []byte("sealed"))

	stdout, stderr, exit := runCmd(t, "repair", "scrub", "--repo", hw.repoURL,
		"--limit", "0", "--heal", "--replica", hw.replicaURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("exit = %d, want 9 — the only finding is a manifest nobody could read\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stderr, "verify.scrub_key_unavailable") {
		t.Errorf("expected verify.scrub_key_unavailable:\n%s", stderr)
	}
	if strings.Contains(stdout+stderr, zeroHashHex) {
		t.Errorf("a missing KEK was reported as a corrupt chunk with the zero hash:\n%s\n%s", stdout, stderr)
	}
}

// LOW (repo scrub): same class — no 000… "corrupted chunk", no
// scrub_mismatch; a key-unavailable verdict instead.
func TestRepoScrub_MissingKEKIsNotACorruptChunk(t *testing.T) {
	w := newReadWorld(t)
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	commitEncryptedBackup(t, w, "db1", "nokek", 1, kek, "local:default", []byte("sealed"))

	stdout, stderr, exit := runCmd(t, "repo", "scrub", w.repoURL, "--full", "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("exit = %d, want 9\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if strings.Contains(stderr, "verify.scrub_mismatch") || strings.Contains(stdout+stderr, zeroHashHex) {
		t.Errorf("a missing KEK was reported as bit rot:\n%s\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "verify.scrub_key_unavailable") {
		t.Errorf("expected verify.scrub_key_unavailable:\n%s", stderr)
	}
}

// H9: WAL is encrypted since issue #106. The scrubs read WAL chunks
// through a non-decrypting CAS, so every encrypted WAL chunk was reported
// as bit rot (exit 9 + an audit mismatch event). They must decrypt with
// the segment's own envelope, as `wal fetch` does.
func TestScrub_EncryptedWALIsNotBitRot(t *testing.T) {
	w := newReadWorld(t)
	var kek, dek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(dek[:]); err != nil {
		t.Fatal(err)
	}
	installLocalKEK(t, kek)
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := aesgcm.New(dek[:])
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("an encrypted WAL chunk")
	info, err := casdefault.NewEncrypted(w.sp, enc).PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	seg := &walsink.SegmentManifest{
		Schema: walsink.Schema, Deployment: "db1", Timeline: 1, SegmentNumber: 1,
		SegmentName: "000000010000000000000001", SegmentSize: 16 << 20,
		CreatedAt: time.Unix(0, 0).UTC(),
		Chunks:    []walsink.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}},
		Encryption: &walsink.EncryptionInfo{Scheme: "aes-256-gcm", KEKRef: "local:default",
			WrappedDEK: base64.StdEncoding.EncodeToString(wrapped), EnvelopeVersion: 1},
	}
	raw, err := seg.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	putBytesExt(t, w, "wal/db1/00000001/000000010000000000000001.json", raw)

	for _, args := range [][]string{
		{"repair", "scrub", "--repo", w.repoURL, "--limit", "0", "-o", "json"},
		{"repo", "scrub", w.repoURL, "--full", "-o", "json"},
	} {
		stdout, stderr, exit := runCmd(t, args...)
		if exit != int(output.ExitOK) {
			t.Fatalf("%v: exit = %d — an intact encrypted WAL chunk was reported as a finding\nstdout=%s\nstderr=%s",
				args[:2], exit, stdout, stderr)
		}
		if !strings.Contains(stdout, `"mismatch_count": 0`) || !strings.Contains(stdout, `"ok": 1`) {
			t.Errorf("%v: expected the WAL chunk verified OK:\n%s", args[:2], stdout)
		}
	}
}

func putBytesExt(t *testing.T, w *readWorld, key string, body []byte) {
	t.Helper()
	if _, err := w.sp.Put(context.Background(), key, strings.NewReader(string(body)), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
}
