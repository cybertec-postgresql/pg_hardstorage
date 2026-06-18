package cli_test

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// identityKMSProvider unwraps by identity (wrapped == dek).
type identityKMSProvider struct{ ref string }

func (p *identityKMSProvider) Name() string                                          { return "kmscfg-test" }
func (p *identityKMSProvider) KEKRef() string                                        { return p.ref }
func (p *identityKMSProvider) WrapDEK(_ context.Context, d []byte) ([]byte, error)   { return d, nil }
func (p *identityKMSProvider) UnwrapDEK(_ context.Context, w []byte) ([]byte, error) { return w, nil }
func (p *identityKMSProvider) Shred(_ context.Context) error                         { return nil }
func (p *identityKMSProvider) FIPSMode() bool                                        { return false }
func (p *identityKMSProvider) Close() error                                          { return nil }

// commitCloudEncryptedBackup commits a manifest whose chunk is encrypted
// under dek and whose DEK is "wrapped" under a cloud KEKRef (the fake
// provider wraps by identity, so WrappedDEK == dek).
func commitCloudEncryptedBackup(t *testing.T, w *readWorld, deployment, kekRef, filePath string, dek [encryption.KeyLen]byte, body []byte) string {
	t.Helper()
	enc, err := aesgcm.New(dek[:])
	if err != nil {
		t.Fatal(err)
	}
	info, err := casdefault.NewEncrypted(w.sp, enc).PutChunk(context.Background(), body)
	if err != nil {
		t.Fatalf("put encrypted chunk: %v", err)
	}
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	id := deployment + ".verify." + ts.Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: id, Deployment: deployment, Tenant: "default",
		Type: backup.BackupTypeFull, PGVersion: 17, SystemIdentifier: "7000000000000000001",
		StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
		StartedAt: ts, StoppedAt: ts.Add(30 * time.Second),
		BackupLabel: "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{{Path: filePath, Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}}}},
		Encryption: &backup.EncryptionInfo{
			Scheme: "aes-256-gcm", KEKRef: kekRef,
			WrappedDEK: base64.StdEncoding.EncodeToString(dek[:]), EnvelopeVersion: 2,
		},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit encrypted: %v", err)
	}
	return id
}

// TestVerify_CloudKMS_PassesKMSConfigToProvider is the end-to-end proof that
// `verify --kms-config region=...,endpoint=...` actually reaches the cloud
// KMS provider builder — not just that the flag parses.
func TestVerify_CloudKMS_PassesKMSConfigToProvider(t *testing.T) {
	w := newReadWorld(t)

	var dek [encryption.KeyLen]byte
	if d, err := encryption.GenerateDEK(); err != nil {
		t.Fatal(err)
	} else {
		dek = d
	}
	id := commitCloudEncryptedBackup(t, w, "db1", "kmscfg-test://key", "data/file", dek, []byte("cloud secret"))

	// Provider builder records the cfg it receives; identity unwrap returns
	// the DEK so verify can decrypt and succeed.
	var mu sync.Mutex
	var gotCfg map[string]any
	kms.DefaultRegistry.Register("kmscfg-test", func(_ context.Context, ref string, cfg map[string]any) (kms.Provider, error) {
		mu.Lock()
		gotCfg = cfg
		mu.Unlock()
		return &identityKMSProvider{ref: ref}, nil
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register("kmscfg-test", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, errors.New("cleared")
		})
	})

	stdout, stderr, exit := runCLI(t, "verify", "db1", id,
		"--repo", w.repoURL,
		"--kms-config", "region=eu-central-1,endpoint=https://kms.local",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("verify exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotCfg == nil {
		t.Fatal("provider builder was never called — verify did not reach the cloud KMS path")
	}
	if gotCfg["region"] != "eu-central-1" || gotCfg["endpoint"] != "https://kms.local" {
		t.Errorf("provider builder cfg = %v, want region=eu-central-1 endpoint=https://kms.local (the --kms-config flag)", gotCfg)
	}
}
