package recovery_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

const cloudRef = "aws-kms://arn:aws:kms:eu-central-1:111122223333:key/abcd"

func (w *recoveryWorld) commitCloudKMSBackup(t *testing.T, stoppedAt time.Time) {
	t.Helper()
	info, err := casdefault.New(w.sp).PutChunk(context.Background(), []byte("xxxxxxxxxxxxxxxx"))
	if err != nil {
		t.Fatal(err)
	}
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.cloud", Deployment: "db1", Tenant: "default",
		Type: backup.BackupTypeFull, PGVersion: 17, SystemIdentifier: "7000000000000000001",
		StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
		StartedAt: stoppedAt.Add(-30 * time.Second), StoppedAt: stoppedAt,
		BackupLabel: "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Encryption: &backup.EncryptionInfo{
			Scheme: "aes-256-gcm", KEKRef: cloudRef, EnvelopeVersion: 1,
			WrappedDEK: base64.StdEncoding.EncodeToString([]byte("kms-ciphertext-blob")),
		},
		Files: []backup.FileEntry{{Path: "data/x", Size: 16, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: 16}}}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
}

// localOnlyResolver is what keystore.KEKResolver does with a cloud ref:
// it only understands local keyring refs.
func localOnlyResolver(ref string) ([encryption.KeyLen]byte, error) {
	return [encryption.KeyLen]byte{}, errors.New("unknown KEK ref scheme")
}

// Regression (M103): readiness resolved every KEKRef through the local
// keyring resolver, so every cloud-KMS-encrypted deployment was
// reported NOT READY (kek_unreachable) although restore — which
// unwraps the DEK server-side — works. Cloud refs must go through the
// same DEK-unwrap path restore uses.
func TestReadiness_CloudKMSUsesDEKUnwrap(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitCloudKMSBackup(t, now.Add(-time.Hour))

	var gotRef string
	var gotWrapped []byte
	r, err := recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{
		Verifier:    w.verifier,
		Now:         now,
		KEKResolver: localOnlyResolver,
		DEKUnwrapper: func(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error) {
			gotRef, gotWrapped = kekRef, wrapped
			return make([]byte, encryption.KeyLen), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Encryption == nil || !r.Encryption.KEKReachable || !r.Encryption.UnwrapOK {
		t.Fatalf("cloud KMS KEK reported unreachable: %+v", r.Encryption)
	}
	if r.OverallStatus == recovery.StatusNotReady {
		t.Errorf("status = %s; a reachable cloud KMS must not make the deployment not_ready", r.OverallStatus)
	}
	if gotRef != cloudRef || string(gotWrapped) != "kms-ciphertext-blob" {
		t.Errorf("unwrapper got ref=%q wrapped=%q; want the manifest's KEKRef and decoded WrappedDEK", gotRef, gotWrapped)
	}

	// And a KMS that genuinely refuses is still NOT READY.
	r, err = recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{
		Verifier: w.verifier, Now: now, KEKResolver: localOnlyResolver,
		DEKUnwrapper: func(context.Context, string, []byte) ([]byte, error) {
			return nil, errors.New("AccessDeniedException")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Encryption.KEKReachable || r.OverallStatus != recovery.StatusNotReady {
		t.Errorf("failing KMS: KEKReachable=%v status=%s, want false/not_ready", r.Encryption.KEKReachable, r.OverallStatus)
	}
}
