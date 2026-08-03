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

// Drop-ins are applied in lexicographic order, later winning. With a
// single drop-in that is unobservable — TestLoad_DropInOverridesProviderConfig
// would pass even if the loader picked "the other one" by luck. Two
// drop-ins whose ordering is the ONLY thing distinguishing them is what
// actually pins the documented precedence.
func TestLoad_DropInPrecedenceIsLexicographic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
kms:
  providers:
    - kek_ref: aws-kms://alias/prod
      config:
        region: base
deployments:
  db1:
    repo: file:///srv/repo
    kek_ref: aws-kms://alias/prod
`)
	// Deliberately written in the order that would win if the loader
	// used directory order rather than sorting.
	writeFile(t, filepath.Join(dir, "conf.d", "90-late.yaml"), `
kms:
  providers:
    - kek_ref: aws-kms://alias/prod
      config:
        region: from-90
`)
	writeFile(t, filepath.Join(dir, "conf.d", "10-early.yaml"), `
kms:
  providers:
    - kek_ref: aws-kms://alias/prod
      config:
        region: from-10
`)

	res, err := config.Load(pathsForTempDir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Config.KMSProviderConfig("aws-kms://alias/prod")["region"]; got != "from-90" {
		t.Errorf("region = %v, want from-90 — 90-late.yaml must beat 10-early.yaml, "+
			"which is the numeric-prefix convention operators rely on (sysctl.d / sudoers.d)", got)
	}
}

// A drop-in overriding a DEPLOYMENT's kek_ref is the operator's path
// for pointing one deployment at a new key without editing the base
// file. Deployments merge by name, so this exercises a different merge
// arm than the provider list (which is additive).
func TestLoad_DropInOverridesDeploymentKEKRef(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
kms:
  providers:
    - kek_ref: aws-kms://alias/old
      config:
        region: us-east-1
    - kek_ref: aws-kms://alias/new
      config:
        region: eu-central-1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: file:///srv/repo
    kek_ref: aws-kms://alias/old
`)
	writeFile(t, filepath.Join(dir, "conf.d", "50-rotate.yaml"), `
deployments:
  db1:
    kek_ref: aws-kms://alias/new
`)

	res, err := config.Load(pathsForTempDir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	dep := res.Config.Deployments["db1"]
	if dep.KEKRef != "aws-kms://alias/new" {
		t.Fatalf("kek_ref = %q, want the drop-in's alias/new", dep.KEKRef)
	}
	// The rest of the deployment must survive the overlay — a merge arm
	// that replaced the whole entry would silently drop pg_connection
	// and repo, and the deployment would fail at first use.
	if dep.Repo != "file:///srv/repo" || dep.PGConnection == "" {
		t.Errorf("drop-in overlay dropped sibling fields: repo=%q pg_connection=%q",
			dep.Repo, dep.PGConnection)
	}
	// And the ref must resolve to the NEW provider's settings.
	if got := res.Config.KMSProviderConfig(dep.KEKRef)["region"]; got != "eu-central-1" {
		t.Errorf("provider region = %v, want eu-central-1", got)
	}
}

// TestMarshal_RoundTripsKMSSection covers the path `pg_hardstorage
// init` takes: load the merged config, Marshal it back out, write it
// as the canonical pg_hardstorage.yaml.
//
// Marshal had no test at all. A `kms:` section that serialises wrong —
// dropped by omitempty, emitted under a different key, flattened —
// would rewrite an operator's file WITHOUT their cloud-KMS wiring, and
// the next backup would fall back to the local keyring or fail. The
// loader is strict, so the reload below is a real check: a wrong key
// name fails rather than being ignored.
func TestMarshal_RoundTripsKMSSection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), issue44YAML)
	res, err := config.Load(pathsForTempDir(t, dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	out, err := config.Marshal(&res.Config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Reload the serialised form through the same strict loader.
	dir2 := t.TempDir()
	writeFile(t, filepath.Join(dir2, "pg_hardstorage.yaml"), string(out))
	back, err := config.Load(pathsForTempDir(t, dir2))
	if err != nil {
		t.Fatalf("re-loading marshalled config failed: %v\n\nMarshal emitted YAML its own "+
			"loader rejects, so `init` would write a file the tool cannot read:\n%s", err, out)
	}

	dep, ok := back.Config.Deployments["db1"]
	if !ok {
		t.Fatal("deployment db1 did not survive the round trip")
	}
	if dep.KEKRef != "azure-kv://acme-pg-vault/db1-kek" {
		t.Errorf("kek_ref = %q after round trip; `init` would drop the deployment's KMS "+
			"binding and the next backup would silently use a different key custody posture",
			dep.KEKRef)
	}
	if got := len(back.Config.KMS.Providers); got != 1 {
		t.Fatalf("kms.providers = %d after round trip, want 1:\n%s", got, out)
	}
	if got := back.Config.KMSProviderConfig(dep.KEKRef)["use_fips_mode"]; got != true {
		t.Errorf("provider config = %v after round trip, want use_fips_mode:true", got)
	}
}

// TestLoad_RejectsEmptyProviderKEKRef pins the other half of provider
// validation. TestLoad_RejectsSchemelessKEKRef covers a ref that is
// present but malformed; an ABSENT one takes a different branch, and
// an unkeyed provider entry is what a half-finished copy-paste from
// the docs produces.
//
// It must name the index: an operator with several providers and a
// conf.d stack needs to know WHICH entry, not just that one is wrong.
func TestLoad_RejectsEmptyProviderKEKRef(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
kms:
  providers:
    - kek_ref: aws-kms://alias/good
      config:
        region: us-east-1
    - config:
        region: eu-central-1
`)
	_, err := config.Load(pathsForTempDir(t, dir))
	if err == nil {
		t.Fatal("a provider entry with no kek_ref loaded cleanly; it can never match a " +
			"lookup, so its config silently never applies")
	}
	msg := err.Error()
	if !strings.Contains(msg, "kek_ref is required") {
		t.Errorf("err = %v, want it to say the field is required", err)
	}
	if !strings.Contains(msg, "[1]") {
		t.Errorf("err = %v, want it to name the offending index — with a conf.d stack "+
			"\"one of your providers\" is not actionable", err)
	}
}

// TestKMSProviderConfig_ExactBeatsLaterPrefix pins the interaction
// between the lookup's two passes.
//
// Exact matches are scanned in a complete pass BEFORE any prefix
// matching, so an exact entry wins wherever it sits in the list. Fold
// the two passes into one loop — the obvious simplification — and the
// later prefix entry starts winning instead, because the scan runs
// last-to-first.
//
// That would be near-invisible: the wrong config still resolves, still
// looks plausible, and only shows up as a KMS call made against the
// wrong region or credential.
func TestKMSProviderConfig_ExactBeatsLaterPrefix(t *testing.T) {
	cfg := config.Config{KMS: config.KMSConfig{Providers: []config.KMSProvider{
		// Exact entry FIRST...
		{KEKRef: "azure-kv://vault/key/9f2c", Config: map[string]any{"which": "exact"}},
		// ...and a prefix candidate LATER, where last-to-first would find it first.
		{KEKRef: "azure-kv://vault/key", Config: map[string]any{"which": "prefix"}},
	}}}

	if got := cfg.KMSProviderConfig("azure-kv://vault/key/9f2c")["which"]; got != "exact" {
		t.Errorf("resolved the %q entry; an exact kek_ref match must beat a prefix match "+
			"regardless of declaration order, or a version-pinned override is silently "+
			"ignored in favour of the base entry", got)
	}
	// The base entry must still serve refs that have no exact entry.
	if got := cfg.KMSProviderConfig("azure-kv://vault/key/aaaa")["which"]; got != "prefix" {
		t.Errorf("resolved %q; a ref with no exact entry must fall back to the base", got)
	}
}

// TestLoad_ProvidersAccumulateAcrossDropIns checks the additive merge
// across a three-file conf.d stack, and that last-wins still holds at
// the end of it.
//
// The existing drop-in test uses two files, which cannot distinguish
// "appended" from "replaced by the last file" — with two entries both
// readings give the same count. Three files with two DIFFERENT refs
// separate them.
func TestLoad_ProvidersAccumulateAcrossDropIns(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
kms:
  providers:
    - kek_ref: aws-kms://alias/a
      config:
        region: base
`)
	// writeFile creates parent directories.
	writeFile(t, filepath.Join(dir, "conf.d", "10-b.yaml"), `
kms:
  providers:
    - kek_ref: aws-kms://alias/b
      config:
        region: from-10
`)
	writeFile(t, filepath.Join(dir, "conf.d", "20-a-override.yaml"), `
kms:
  providers:
    - kek_ref: aws-kms://alias/a
      config:
        region: from-20
`)

	res, err := config.Load(pathsForTempDir(t, dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := len(res.Config.KMS.Providers); got != 3 {
		t.Fatalf("kms.providers = %d, want 3 — drop-ins must APPEND; replacing the list "+
			"would drop providers declared in the base file", got)
	}
	// The unrelated drop-in provider survives...
	if got := res.Config.KMSProviderConfig("aws-kms://alias/b")["region"]; got != "from-10" {
		t.Errorf("alias/b region = %v, want from-10", got)
	}
	// ...and the later re-declaration of alias/a wins the lookup.
	if got := res.Config.KMSProviderConfig("aws-kms://alias/a")["region"]; got != "from-20" {
		t.Errorf("alias/a region = %v, want from-20 — a drop-in that re-declares a "+
			"provider must win, or a credential rotation lands in a file nothing reads", got)
	}
}

// TestLoad_UnknownButWellFormedSchemeIsAccepted pins a layering
// decision that looks like a missing check.
//
// validKEKRef verifies SHAPE only. It deliberately does not require a
// registered scheme, because the KMS registry is a plugin surface and
// a build may carry providers this validator has never heard of.
// `doctor` is the layer that flags an unregistered scheme, with the
// registry actually in hand (see TestAppendKEKRefChecks).
//
// Without this test, "the loader accepts nonsense://" reads like an
// oversight, and tightening it here would break every out-of-tree
// provider while moving no real check earlier.
func TestLoad_UnknownButWellFormedSchemeIsAccepted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
deployments:
  db1:
    repo: file:///srv/repo
    kek_ref: acme-hsm://appliance-3/key-7
`)
	res, err := config.Load(pathsForTempDir(t, dir))
	if err != nil {
		t.Fatalf("a well-formed kek_ref with an unregistered scheme must load: %v\n"+
			"Scheme registration is checked by `doctor`, which has the registry; "+
			"rejecting here would break out-of-tree KMS providers", err)
	}
	if got := res.Config.Deployments["db1"].KEKRef; got != "acme-hsm://appliance-3/key-7" {
		t.Errorf("kek_ref = %q", got)
	}
}

// TestKMSProviderConfig_ReturnsSharedMap documents that the lookup
// hands back the config's own map rather than a copy.
//
// Callers thread it straight into keystore.UnwrapOpts.ProviderConfig
// and none of them write to it, so this is a read-only contract rather
// than a bug. It is pinned because the contract is invisible at the
// call site: a future caller that "just adds a default" would mutate
// the loaded config for every later lookup in the process, and an
// agent resolving several deployments would see the leak.
//
// If this ever starts failing because the lookup began copying, that
// is an improvement — update the test deliberately.
func TestKMSProviderConfig_ReturnsSharedMap(t *testing.T) {
	cfg := config.Config{KMS: config.KMSConfig{Providers: []config.KMSProvider{
		{KEKRef: "aws-kms://alias/prod", Config: map[string]any{"region": "us-east-1"}},
	}}}

	got := cfg.KMSProviderConfig("aws-kms://alias/prod")
	got["region"] = "MUTATED"

	if again := cfg.KMSProviderConfig("aws-kms://alias/prod")["region"]; again != "MUTATED" {
		t.Skip("the lookup now returns a copy — safer than the documented contract; " +
			"remove this test or invert it")
	}
	t.Log("provider config is shared, not copied: callers must treat it as read-only")
}
