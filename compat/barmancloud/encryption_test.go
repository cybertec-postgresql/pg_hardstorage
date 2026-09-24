package barmancloud

import "testing"

// --encryption (AES256 / aws:kms) asks S3 for server-side encryption.
// The native S3 plugin has no per-request SSE setting, so the flag was
// accepted and dropped: objects landed unencrypted unless the bucket
// happened to default-encrypt. Every verb must refuse it, before any
// dispatch.
func TestEncryptionRefused(t *testing.T) {
	env := map[string]string{"PGDATA": "/pgdata"}
	for _, tc := range []struct {
		name string
		verb func([]string) int
		argv []string
	}{
		{"wal-archive", ExecuteWalArchive, []string{"--encryption", "AES256", "s3://b/p", "srv", "pg_wal/000000010000000000000001"}},
		{"wal-restore", ExecuteWalRestore, []string{"--encryption", "aws:kms", "s3://b/p", "srv", "000000010000000000000001", "pg_wal/RECOVERYXLOG"}},
		{"backup", ExecuteBackup, []string{"--encryption", "AES256", "s3://b/p", "srv"}},
		{"restore", ExecuteRestore, []string{"--encryption", "AES256", "s3://b/p", "srv", "id", "/pgdata"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, rc := runWithStubbedDispatch(t, env, nil, tc.verb, tc.argv)
			if rc == 0 {
				t.Fatalf("--encryption accepted (dispatched %v)", got)
			}
			if got != nil {
				t.Fatalf("refused invocation still dispatched %v", got)
			}
			if tc.name == "wal-restore" && rc != exitAbortRecovery {
				t.Errorf("wal-restore refusal must abort recovery (126), got %d", rc)
			}
		})
	}
}
