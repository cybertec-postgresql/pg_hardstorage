package backup_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// A Type=incremental_lsn manifest with an empty ParentBackupID used
// to pass Validate and commit cleanly — then skipped Commit's
// parent-liveness check (keyed on ParentBackupID != ""), was
// invisible to soft-delete chain protection, and failed only at
// restore with chain.no_full_anchor: a "successful", signed backup
// that was structurally unrestorable from the moment of commit.
func TestManifestValidate_ChainInvariants(t *testing.T) {
	base := func() *backup.Manifest {
		return &backup.Manifest{
			Schema:           backup.Schema,
			BackupID:         "db1.full.20260430T120000Z.aaaa",
			Deployment:       "db1",
			Type:             backup.BackupTypeFull,
			PGVersion:        17,
			SystemIdentifier: "7000000000000000001",
			StartLSN:         "0/3000028",
			StopLSN:          "0/30001A0",
			Timeline:         1,
			BackupLabel:      "START WAL LOCATION: 0/3000028\n",
			Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Files:            []backup.FileEntry{},
		}
	}

	cases := []struct {
		name    string
		mutate  func(*backup.Manifest)
		wantErr string
	}{
		{"incremental_without_parent", func(m *backup.Manifest) {
			m.Type = backup.BackupTypeIncremental
			m.ParentBackupID = ""
		}, "requires parent_backup_id"},
		{"incremental_self_parent", func(m *backup.Manifest) {
			m.Type = backup.BackupTypeIncremental
			m.ParentBackupID = m.BackupID
		}, "self-referential"},
		{"full_with_parent", func(m *backup.Manifest) {
			m.ParentBackupID = "db1.full.20260429T120000Z.zzzz"
		}, "must not carry parent_backup_id"},
		{"unknown_type", func(m *backup.Manifest) {
			m.Type = "differential"
		}, "unknown type"},
		{"empty_type", func(m *backup.Manifest) {
			m.Type = ""
		}, "type is empty"},
		{"zero_timeline", func(m *backup.Manifest) {
			m.Timeline = 0
		}, "timeline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mutate(m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a %s manifest — it would commit green and fail only at restore", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}

	// The valid shapes must keep passing.
	if err := base().Validate(); err != nil {
		t.Errorf("valid full manifest refused: %v", err)
	}
	inc := base()
	inc.Type = backup.BackupTypeIncremental
	inc.ParentBackupID = "db1.full.20260429T120000Z.zzzz"
	if err := inc.Validate(); err != nil {
		t.Errorf("valid incremental refused: %v", err)
	}
}

// Recovery replays [StartLSN, StopLSN] forward; an inverted pair
// describes a backup no recovery can reach consistency from, and it
// used to commit green.
func TestManifestValidate_RefusesInvertedLSNRange(t *testing.T) {
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.20260430T120000Z.eeee",
		Deployment:       "db1",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/9000000",
		StopLSN:          "0/3000028", // before start
		Timeline:         1,
		BackupLabel:      "START WAL LOCATION: 0/9000000\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:            []backup.FileEntry{},
	}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "precedes start_lsn") {
		t.Fatalf("inverted LSN range accepted (err=%v) — such a backup commits green and can never reach consistency", err)
	}
}
