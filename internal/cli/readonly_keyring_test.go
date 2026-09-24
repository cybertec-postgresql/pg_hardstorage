package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
)

// Read-only verbs used keystore.LoadOrGenerate and so MINTED a signing
// keypair on a host whose keyring was empty or mis-pointed. That key
// verifies none of the repo's manifests, and the next backup silently
// signed under it — the repo split across two keys. Only init/backup
// may create a keypair.
func TestReadOnlyVerbs_NeverGenerateSigningKeypair(t *testing.T) {
	isolateHOME(t)
	repoURL := initRepoForTest(t)
	keyringDir := resolvedKeyringDir(t)
	priv := filepath.Join(keyringDir, keystore.PrivateKeyFile)

	for _, argv := range [][]string{
		{"doctor", "-o", "json"},
		{"list", "db1", "--repo", repoURL, "-o", "json"},
		{"kms", "verify", "--repo", repoURL, "-o", "json"},
		{"rotate", "--repo", repoURL, "-o", "json"},
	} {
		_, errb, _ := runCLI(t, argv...)
		if _, err := os.Stat(priv); err == nil {
			t.Fatalf("`%s` generated a signing keypair in %s", strings.Join(argv, " "), keyringDir)
		}
		if argv[0] == "list" && !strings.Contains(errb, "notfound.signing_key") {
			t.Errorf("list on an empty keyring should say notfound.signing_key:\n%s", errb)
		}
	}
}
