package config_test

// Issue #44: every KMS how-to in docs/ told operators to declare a
// top-level `kms:` block and a per-deployment `kek_ref`. The strict
// (KnownFields) loader had neither field, so following the docs
// produced `field kms not found in type config.Config` and cloud KMS
// was reachable only through per-invocation --kek / --kms-config flags
// — which the agent's scheduled and control-plane backups can't pass.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// The exact YAML from the issue report, assembled from the two
// snippets in docs/how-to/adding/kms-azure.md.
const issue44YAML = `
kms:
  providers:
    - kek_ref: azure-kv://acme-pg-vault/db1-kek
      config:
        use_fips_mode: true   # operator declaration; matches Premium / Managed HSM
deployments:
  db1:
    pg_connection: postgres://pgbackup@db1.example.com/postgres
    repo: azblob://acmebackups/prod
    kek_ref: azure-kv://acme-pg-vault/db1-kek
`

func TestLoad_Issue44DocumentedKMSConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), issue44YAML)

	res, err := config.Load(pathsForTempDir(t, dir))
	if err != nil {
		t.Fatalf("the config from docs/how-to/adding/kms-azure.md must load: %v", err)
	}

	dep, ok := res.Config.Deployments["db1"]
	if !ok {
		t.Fatal("deployment db1 missing")
	}
	if dep.KEKRef != "azure-kv://acme-pg-vault/db1-kek" {
		t.Errorf("db1.kek_ref = %q", dep.KEKRef)
	}
	if got := len(res.Config.KMS.Providers); got != 1 {
		t.Fatalf("kms.providers = %d entries, want 1", got)
	}
	pcfg := res.Config.KMSProviderConfig(dep.KEKRef)
	if pcfg["use_fips_mode"] != true {
		t.Errorf("provider config = %v, want use_fips_mode:true — the block parsed but never reached the lookup", pcfg)
	}
}

func TestKMSProviderConfig_Lookup(t *testing.T) {
	cfg := config.Config{KMS: config.KMSConfig{Providers: []config.KMSProvider{
		{KEKRef: "azure-kv://vault/key", Config: map[string]any{"tier": "base"}},
		{KEKRef: "aws-kms://alias/prod", Config: map[string]any{"region": "us-east-1"}},
	}}}

	t.Run("exact", func(t *testing.T) {
		if got := cfg.KMSProviderConfig("aws-kms://alias/prod")["region"]; got != "us-east-1" {
			t.Errorf("region = %v", got)
		}
	})

	t.Run("version_pinned_ref_matches_base_entry", func(t *testing.T) {
		// Azure's Shred path REQUIRES a version-pinned KEKRef. An
		// operator shouldn't have to re-declare the provider on every
		// key rotation.
		if got := cfg.KMSProviderConfig("azure-kv://vault/key/9f2c")["tier"]; got != "base" {
			t.Errorf("tier = %v, want the base entry's config", got)
		}
	})

	t.Run("prefix_match_does_not_leak_across_sibling_keys", func(t *testing.T) {
		// "azure-kv://vault/key-rsa" must NOT inherit "…/key"'s config:
		// they're different keys that merely share a name prefix.
		if got := cfg.KMSProviderConfig("azure-kv://vault/key-rsa"); got != nil {
			t.Errorf("sibling key matched the wrong entry: %v", got)
		}
	})

	t.Run("miss_is_nil_not_a_panic", func(t *testing.T) {
		if got := cfg.KMSProviderConfig("vault-transit://v/undeclared"); got != nil {
			t.Errorf("got %v, want nil", got)
		}
		if got := cfg.KMSProviderConfig(""); got != nil {
			t.Errorf("empty ref: got %v, want nil", got)
		}
	})
}

// conf.d drop-ins are the documented way to override a base config.
// A drop-in that re-declares a provider must win the lookup, or an
// operator's credential rotation lands in a file nothing reads.
func TestLoad_DropInOverridesProviderConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
kms:
  providers:
    - kek_ref: aws-kms://alias/prod
      config:
        region: us-east-1
deployments:
  db1:
    repo: file:///srv/repo
    kek_ref: aws-kms://alias/prod
`)
	writeFile(t, filepath.Join(dir, "conf.d", "90-region.yaml"), `
kms:
  providers:
    - kek_ref: aws-kms://alias/prod
      config:
        region: eu-central-1
`)

	res, err := config.Load(pathsForTempDir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(res.Config.KMS.Providers); got != 2 {
		t.Errorf("providers = %d, want both entries retained (additive merge, like sinks)", got)
	}
	if got := res.Config.KMSProviderConfig("aws-kms://alias/prod")["region"]; got != "eu-central-1" {
		t.Errorf("region = %v, want the drop-in's eu-central-1", got)
	}
}

func TestLoad_RejectsSchemelessKEKRef(t *testing.T) {
	cases := map[string]string{
		"deployment": `
deployments:
  db1:
    repo: file:///srv/repo
    kek_ref: db1-kek
`,
		"provider": `
kms:
  providers:
    - kek_ref: db1-kek
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), body)
			_, err := config.Load(pathsForTempDir(t, dir))
			if err == nil {
				t.Fatal("a kek_ref with no scheme loaded cleanly; the failure would surface at first backup instead")
			}
			if !strings.Contains(err.Error(), "no scheme") {
				t.Errorf("err = %v, want it to name the missing scheme", err)
			}
		})
	}
}

func TestLoad_LocalKEKRefIsValid(t *testing.T) {
	// Pinning the on-disk keyring explicitly is legitimate: it
	// documents intent and survives someone later adding a cloud
	// provider to the same file.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
deployments:
  db1:
    repo: file:///srv/repo
    kek_ref: local:default
`)
	if _, err := config.Load(pathsForTempDir(t, dir)); err != nil {
		t.Fatalf("local:default rejected: %v", err)
	}
}
