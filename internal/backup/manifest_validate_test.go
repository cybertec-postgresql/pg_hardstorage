package backup_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// validManifest returns a minimal-but-complete Manifest that
// passes Validate.  Tests mutate one field at a time to assert
// each invariant fires in isolation.
func validManifest() *backup.Manifest {
	hash := repo.Hash{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	return &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.20260506T120000Z.0001",
		Deployment:       "db1",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000007",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        time.Now().UTC(),
		StoppedAt:        time.Now().UTC(),
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: 3, Chunks: []backup.ChunkRef{
				{Hash: hash, Offset: 0, Len: 3},
			}},
			{Path: "empty_marker", Size: 0, Chunks: nil},
			{Path: "base/16384/2619", Size: 100, Chunks: []backup.ChunkRef{
				{Hash: hash, Offset: 0, Len: 60},
				{Hash: hash, Offset: 60, Len: 40},
			}},
		},
		Dirs: []backup.DirEntry{
			{Path: "pg_wal", Mode: 0o700},
			{Path: "pg_replslot", Mode: 0o700},
		},
		BackupLabel: "START WAL LOCATION: 0/3000028\n",
	}
}

func TestManifestValidate_Happy(t *testing.T) {
	if err := validManifest().Validate(); err != nil {
		t.Fatalf("expected valid manifest to pass: %v", err)
	}
}

func TestManifestValidate_Invariants(t *testing.T) {
	// Each case mutates one field on a fresh valid manifest
	// and asserts Validate surfaces the expected substring.
	cases := []struct {
		name    string
		mutate  func(*backup.Manifest)
		wantSub string
	}{
		{"nil-receiver", func(m *backup.Manifest) {}, ""}, // handled below
		{"wrong-schema", func(m *backup.Manifest) { m.Schema = "wrong" }, "schema"},
		{"empty-backup-id", func(m *backup.Manifest) { m.BackupID = "" }, "backup_id"},
		{"empty-deployment", func(m *backup.Manifest) { m.Deployment = "" }, "deployment"},
		{"zero-pg-version", func(m *backup.Manifest) { m.PGVersion = 0 }, "pg_version"},
		{"empty-system-id", func(m *backup.Manifest) { m.SystemIdentifier = "" }, "system_identifier"},
		{"empty-start-lsn", func(m *backup.Manifest) { m.StartLSN = "" }, "LSN"},
		{"empty-stop-lsn", func(m *backup.Manifest) { m.StopLSN = "" }, "LSN"},
		{"empty-backup-label", func(m *backup.Manifest) { m.BackupLabel = "" }, "backup_label"},
		{"no-tablespaces", func(m *backup.Manifest) { m.Tablespaces = nil }, "tablespaces"},
		{"empty-file-path", func(m *backup.Manifest) {
			m.Files[0].Path = ""
		}, "empty path"},
		{"duplicate-file-path", func(m *backup.Manifest) {
			m.Files[1].Path = m.Files[0].Path
		}, "duplicate file path"},
		{"negative-size", func(m *backup.Manifest) {
			m.Files[0].Size = -1
		}, "size=-1"},
		{"chunk-len-mismatch", func(m *backup.Manifest) {
			m.Files[2].Size = 99 // chunks total 100
		}, "chunks total"},
		{"chunk-zero-len", func(m *backup.Manifest) {
			m.Files[2].Chunks[0].Len = 0
			m.Files[2].Chunks[1].Len = 100
			m.Files[2].Chunks[1].Offset = 0
		}, "len=0"},
		{"chunk-noncontiguous-offset", func(m *backup.Manifest) {
			m.Files[2].Chunks[1].Offset = 50 // should be 60
		}, "must be contiguous"},
		{"empty-file-with-chunks", func(m *backup.Manifest) {
			// Naturally caught by the sum-mismatch check —
			// chunks total 1 vs declared size 0.
			hash := repo.Hash{1}
			m.Files[1].Chunks = []backup.ChunkRef{{Hash: hash, Len: 1}}
		}, "chunks total"},
		{"duplicate-dir-path", func(m *backup.Manifest) {
			m.Dirs = append(m.Dirs, backup.DirEntry{Path: "pg_wal", Mode: 0o700})
		}, "duplicate dir path"},
		{"dir-collides-with-file", func(m *backup.Manifest) {
			m.Dirs = append(m.Dirs, backup.DirEntry{Path: "PG_VERSION", Mode: 0o700})
		}, "collides with a file"},
		{"empty-dir-path", func(m *backup.Manifest) {
			m.Dirs = append(m.Dirs, backup.DirEntry{Path: "", Mode: 0o700})
		}, "empty path"},
		// Path-safety invariants: a file/dir path must not escape the
		// restore target.
		{"file-path-traversal", func(m *backup.Manifest) {
			m.Files[0].Path = "../../etc/cron.d/evil"
		}, "escapes the backup root"},
		{"file-path-absolute", func(m *backup.Manifest) {
			m.Files[0].Path = "/etc/shadow"
		}, "is absolute"},
		{"file-path-cleans-to-escape", func(m *backup.Manifest) {
			m.Files[0].Path = "base/../../../../escape"
		}, "escapes the backup root"},
		{"file-path-backslash", func(m *backup.Manifest) {
			m.Files[0].Path = `base\16384\2619`
		}, "backslash"},
		{"file-path-nul", func(m *backup.Manifest) {
			m.Files[0].Path = "ok\x00name"
		}, "NUL"},
		{"dir-path-traversal", func(m *backup.Manifest) {
			m.Dirs = append(m.Dirs, backup.DirEntry{Path: "../outside", Mode: 0o700})
		}, "escapes the backup root"},
		{"dir-path-absolute", func(m *backup.Manifest) {
			m.Dirs = append(m.Dirs, backup.DirEntry{Path: "/abs/dir", Mode: 0o700})
		}, "is absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "nil-receiver" {
				var m *backup.Manifest
				if err := m.Validate(); err == nil {
					t.Fatal("expected nil-receiver to error")
				}
				return
			}
			m := validManifest()
			tc.mutate(m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// encValidManifest returns a valid manifest with COMPLETE encryption
// metadata — every field restore needs to unwrap the data key.
func encValidManifest() *backup.Manifest {
	m := validManifest()
	m.Encryption = &backup.EncryptionInfo{
		Scheme:          "aes-256-gcm",
		KEKRef:          "test:v1",
		WrappedDEK:      base64.StdEncoding.EncodeToString([]byte("0123456789abcdef-wrapped-dek-payload")),
		EnvelopeVersion: 2,
	}
	return m
}

// TestManifestValidate_Encryption pins the encryption self-consistency
// invariant (data-loss path #1): a manifest that declares itself
// encrypted but is missing/malformed the unwrap metadata would be a
// permanently undecryptable backup, so Validate must reject it at the
// commit gate rather than letting restore discover it later.
func TestManifestValidate_Encryption(t *testing.T) {
	if err := encValidManifest().Validate(); err != nil {
		t.Fatalf("complete encryption metadata should pass: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*backup.EncryptionInfo)
		want   string
	}{
		{"empty scheme", func(e *backup.EncryptionInfo) { e.Scheme = "" }, "scheme is empty"},
		{"empty kek_ref", func(e *backup.EncryptionInfo) { e.KEKRef = "" }, "kek_ref is empty"},
		{"empty wrapped_dek", func(e *backup.EncryptionInfo) { e.WrappedDEK = "" }, "wrapped_dek is empty"},
		{"non-base64 wrapped_dek", func(e *backup.EncryptionInfo) { e.WrappedDEK = "not valid base64 !!" }, "not valid base64"},
		{"bad envelope version", func(e *backup.EncryptionInfo) { e.EnvelopeVersion = 0 }, "envelope_version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := encValidManifest()
			tc.mutate(m.Encryption)
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate should reject %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}

	// Unencrypted (nil Encryption) must still pass.
	m := validManifest()
	m.Encryption = nil
	if err := m.Validate(); err != nil {
		t.Errorf("nil encryption (unencrypted) should pass: %v", err)
	}
}
