package cli

// Path-parity: the interactive CLI (`backup`) and the agent/control-
// plane paths resolve backup encryption through DIFFERENT functions.
// They must agree byte-for-byte on posture for every keyring state —
// the failure class here produced scheduled backups that were
// silently PLAINTEXT in an --encrypt repo while interactive backups
// were encrypted, and plaintext-hash dedup then welded the two
// postures onto the same chunks.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
)

func TestEncryptionResolutionParity_CLIvsAgent(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, keyring string)
	}{
		{"empty_keyring", func(t *testing.T, keyring string) {}},
		{"kek_present", func(t *testing.T, keyring string) {
			if _, _, err := keystore.LoadOrGenerateKEK(keyring); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt_kek", func(t *testing.T, keyring string) {
			if err := os.WriteFile(filepath.Join(keyring, keystore.KEKFileName), []byte("short"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			keyring := t.TempDir()
			tc.setup(t, keyring)

			cliCfg, cliErr := resolveBackupEncryption(context.Background(), keyring, false, false, "", nil)
			agentCfg, agentErr := runner.LocalEncryptionFromKeyring(keyring)

			if (cliErr == nil) != (agentErr == nil) {
				t.Fatalf("error parity broken: cli=%v agent=%v — one path fails loudly, the other silently picks a posture", cliErr, agentErr)
			}
			if cliErr != nil {
				return // both refused: parity holds
			}
			if (cliCfg == nil) != (agentCfg == nil) {
				t.Fatalf("posture parity broken: cli encrypted=%v agent encrypted=%v — scheduled and interactive backups would mix plaintext and ciphertext on shared chunks",
					cliCfg != nil, agentCfg != nil)
			}
			if cliCfg == nil {
				return // both plaintext: parity holds
			}
			if cliCfg.KEKRef != agentCfg.KEKRef {
				t.Errorf("KEKRef parity broken: cli=%q agent=%q", cliCfg.KEKRef, agentCfg.KEKRef)
			}
			if cliCfg.KEK != agentCfg.KEK {
				t.Error("KEK material parity broken: the two paths loaded different keys from the same keyring")
			}
		})
	}
}
