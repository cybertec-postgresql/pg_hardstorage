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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// `kms verify` resolved every KEKRef through the local-only
// keystore.KEKResolver, which can only hand out raw key bytes — a cloud
// KEK never leaves the HSM, so every cloud-KMS-wrapped manifest was
// classified kek_unknown and a perfectly healthy repo exited 9. It must
// verify those through the KMS-aware unwrap path.
func TestKMSVerify_CloudKMSManifestIsOK(t *testing.T) {
	registerFakeWALKMS(t)
	w := newReadWorld(t)
	var dek [32]byte
	if _, err := rand.Read(dek[:]); err != nil {
		t.Fatal(err)
	}
	info, err := casdefault.New(w.sp).PutChunk(context.Background(), []byte("cloud-payload"))
	if err != nil {
		t.Fatal(err)
	}
	stoppedAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.cloud", Deployment: "db1", Tenant: "default",
		Type: backup.BackupTypeFull, PGVersion: 17, SystemIdentifier: "7000000000000000001",
		StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
		StartedAt: stoppedAt.Add(-30 * time.Second), StoppedAt: stoppedAt,
		BackupLabel: "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Encryption: &backup.EncryptionInfo{
			Scheme: "aes-256-gcm", KEKRef: "fake-wal-kms://prod-key",
			WrappedDEK: base64.StdEncoding.EncodeToString(xorMask(dek[:])), EnvelopeVersion: 1,
		},
		Files: []backup.FileEntry{{Path: "data/x", Size: 13, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Len: 13}}}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	stdout, errb, exit := runCLI(t, "kms", "verify", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("kms verify on a healthy cloud-KMS repo: exit=%d\n%s\n%s", exit, stdout, errb)
	}
	if !strings.Contains(stdout, `"ok": 1`) {
		t.Errorf("want ok=1:\n%s", stdout)
	}
}
