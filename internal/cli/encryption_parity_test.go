package cli

// Path-parity: the interactive CLI (`backup`) and the agent/control-
// plane paths resolve backup encryption through DIFFERENT call sites.
// They must agree byte-for-byte on posture for every keyring state and
// every configured kek_ref — the failure class here produced scheduled
// backups that were silently PLAINTEXT in an --encrypt repo while
// interactive backups were encrypted, and plaintext-hash dedup then
// welded the two postures onto the same chunks.
//
// Since issue #44 both sides call runner.ResolveEncryption; the CLI
// adds only an error-taxonomy shim. This test pins that the shim
// doesn't drift into a decision of its own, and that a deployment's
// kek_ref reaches both.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

func TestEncryptionResolutionParity_CLIvsAgent(t *testing.T) {
	registerFakeKMS(t)

	cases := []struct {
		name string
		// kekRef is what a deployment's `kek_ref` would resolve to; ""
		// is the historical local-custody-only shape.
		kekRef string
		setup  func(t *testing.T, keyring string)
	}{
		{"empty_keyring", "", func(t *testing.T, keyring string) {}},
		{"kek_present", "", func(t *testing.T, keyring string) {
			if _, _, err := keystore.LoadOrGenerateKEK(keyring); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt_kek", "", func(t *testing.T, keyring string) {
			if err := os.WriteFile(filepath.Join(keyring, keystore.KEKFileName), []byte("short"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"configured_cloud_kek_ref", "fake-kms://db1-kek", func(t *testing.T, keyring string) {}},
		{"configured_cloud_kek_ref_over_local_kek", "fake-kms://db1-kek", func(t *testing.T, keyring string) {
			// The host also has a local kek.bin. Both paths must
			// prefer the configured cloud ref, or a scheduled backup
			// would wrap under a different KEK than an interactive one
			// on the very same host.
			if _, _, err := keystore.LoadOrGenerateKEK(keyring); err != nil {
				t.Fatal(err)
			}
		}},
		{"unopenable_configured_kek_ref", "never-registered://db1-kek", func(t *testing.T, keyring string) {
			if _, _, err := keystore.LoadOrGenerateKEK(keyring); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			keyring := t.TempDir()
			tc.setup(t, keyring)

			cliCfg, cliErr := resolveBackupEncryption(context.Background(), keyring, false, false, tc.kekRef, nil)
			// Exactly the request internal/agent/executor.go and
			// buildBackupTask build for a deployment.
			agentCfg, agentErr := runner.ResolveEncryption(context.Background(), runner.EncryptionRequest{
				KeyringDir: keyring,
				KEKRef:     tc.kekRef,
			})

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
			defer closeProvider(cliCfg)
			defer closeProvider(agentCfg)

			if cliCfg.KEKRef != agentCfg.KEKRef {
				t.Errorf("KEKRef parity broken: cli=%q agent=%q", cliCfg.KEKRef, agentCfg.KEKRef)
			}
			if (cliCfg.Provider == nil) != (agentCfg.Provider == nil) {
				t.Errorf("custody parity broken: cli cloud=%v agent cloud=%v — one wraps in the KMS, the other with the on-disk KEK",
					cliCfg.Provider != nil, agentCfg.Provider != nil)
			}
			if cliCfg.Provider == nil && cliCfg.KEK != agentCfg.KEK {
				t.Error("KEK material parity broken: the two paths loaded different keys from the same keyring")
			}
		})
	}
}

func closeProvider(c *runner.EncryptionConfig) {
	if c != nil && c.Provider != nil {
		_ = c.Provider.Close()
	}
}

// A deployment's kek_ref is what makes cloud KMS reachable from paths
// that have no command line (the scheduler, the control-plane
// executor). This pins the config→request translation those paths
// share with `backup`.
func TestResolveDeploymentKMS(t *testing.T) {
	cfg := config.Config{
		KMS: config.KMSConfig{Providers: []config.KMSProvider{
			{KEKRef: "azure-kv://vault/db1-kek", Config: map[string]any{"use_fips_mode": true}},
			{KEKRef: "aws-kms://alias/other", Config: map[string]any{"region": "us-east-1"}},
		}},
		Deployments: map[string]config.DeploymentConfig{
			"db1": {KEKRef: "azure-kv://vault/db1-kek"},
			"db2": {KEKRef: "azure-kv://vault/db1-kek/9f2c"}, // version-pinned
			"db3": {},
		},
	}

	t.Run("deployment_ref_and_matching_provider_config", func(t *testing.T) {
		ref, pcfg := resolveDeploymentKMS("db1", "", nil, cfg)
		if ref != "azure-kv://vault/db1-kek" {
			t.Fatalf("ref = %q", ref)
		}
		if pcfg["use_fips_mode"] != true {
			t.Errorf("provider config = %v, want the vault entry's", pcfg)
		}
	})

	t.Run("version_pinned_ref_inherits_base_entry", func(t *testing.T) {
		// Azure shred REQUIRES version-pinned refs; making operators
		// re-declare the provider for every key version would be a
		// papercut that guarantees drift.
		ref, pcfg := resolveDeploymentKMS("db2", "", nil, cfg)
		if ref != "azure-kv://vault/db1-kek/9f2c" {
			t.Fatalf("ref = %q", ref)
		}
		if pcfg["use_fips_mode"] != true {
			t.Errorf("provider config = %v, want the base entry's", pcfg)
		}
	})

	t.Run("flag_beats_config", func(t *testing.T) {
		ref, _ := resolveDeploymentKMS("db1", "aws-kms://alias/other", nil, cfg)
		if ref != "aws-kms://alias/other" {
			t.Errorf("--kek did not override the deployment's kek_ref: %q", ref)
		}
	})

	t.Run("flag_kms_config_beats_config_block", func(t *testing.T) {
		_, pcfg := resolveDeploymentKMS("db1", "", map[string]string{"region": "eu-west-1"}, cfg)
		if pcfg["region"] != "eu-west-1" {
			t.Errorf("provider config = %v, want the --kms-config value", pcfg)
		}
		if _, leaked := pcfg["use_fips_mode"]; leaked {
			t.Error("--kms-config was merged with the file's entry; it must replace it wholesale")
		}
	})

	t.Run("deployment_without_kek_ref_stays_local", func(t *testing.T) {
		ref, pcfg := resolveDeploymentKMS("db3", "", nil, cfg)
		if ref != "" || pcfg != nil {
			t.Errorf("ref = %q, cfg = %v; want the local-custody path untouched", ref, pcfg)
		}
	})

	t.Run("unknown_deployment_is_not_an_error", func(t *testing.T) {
		ref, _ := resolveDeploymentKMS("nope", "", nil, cfg)
		if ref != "" {
			t.Errorf("ref = %q, want empty", ref)
		}
	})
}

// An unopenable configured provider must fail the run. Falling back to
// the local kek.bin would write local-wrapped manifests into a repo
// whose other manifests are KMS-wrapped — restorable only by whoever
// still holds that host's keyring.
func TestResolveBackupEncryption_ConfiguredRefNeverFallsBackToLocal(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := keystore.LoadOrGenerateKEK(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveBackupEncryption(context.Background(), dir, false, false,
		"never-registered://db1-kek", nil)
	if err == nil {
		t.Fatalf("unopenable provider accepted; cfg = %+v", cfg)
	}
	if errors.Is(err, runner.ErrEncryptNoKEK) {
		t.Error("misreported as a missing-KEK error")
	}
}
