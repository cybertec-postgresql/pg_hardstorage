package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// pathsForTempDir builds a Paths whose Config domain points at dir.
// We exercise the real Resolve by passing dir as an explicit override.
func pathsForTempDir(t *testing.T, dir string) *paths.Paths {
	t.Helper()
	p, err := paths.Resolve(paths.Options{
		Mode: paths.ModeUser,
		Env:  func(string) string { return "" },
		Overrides: map[paths.Domain]string{
			paths.DomainConfig:     dir,
			paths.DomainState:      filepath.Join(dir, "state"),
			paths.DomainCache:      filepath.Join(dir, "cache"),
			paths.DomainLogs:       filepath.Join(dir, "logs"),
			paths.DomainRuntime:    filepath.Join(dir, "run"),
			paths.DomainSharedData: filepath.Join(dir, "share"),
		},
	})
	if err != nil {
		t.Fatalf("paths.Resolve: %v", err)
	}
	return p
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_SinksRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
schema: pg_hardstorage.config.v1
sinks:
  - name: ops-slack
    plugin: slack
    config:
      webhook_url: https://hooks.slack.com/services/T/B/X
      min_severity: warning
  - name: prod-syslog
    plugin: syslog
    config:
      protocol: tcp
      address: siem.example.com:6514
      facility: local6
`)
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(res.Config.Sinks); got != 2 {
		t.Fatalf("got %d sinks, want 2", got)
	}
	first := res.Config.Sinks[0]
	if first.Name != "ops-slack" || first.Plugin != "slack" {
		t.Errorf("first sink = %+v", first)
	}
	if got := first.Config["webhook_url"]; got != "https://hooks.slack.com/services/T/B/X" {
		t.Errorf("webhook_url round-trip lost; got %v", got)
	}
	if got := first.Config["min_severity"]; got != "warning" {
		t.Errorf("min_severity round-trip lost; got %v", got)
	}
	second := res.Config.Sinks[1]
	if second.Plugin != "syslog" {
		t.Errorf("second.Plugin = %q", second.Plugin)
	}
	if got := second.Config["address"]; got != "siem.example.com:6514" {
		t.Errorf("address round-trip lost; got %v", got)
	}
}

func TestLoad_DropInAppendsSinks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
schema: pg_hardstorage.config.v1
sinks:
  - { name: a, plugin: webhook, config: {url: "https://a"} }
`)
	writeFile(t, filepath.Join(dir, "conf.d", "20-extra.yaml"), `
sinks:
  - { name: b, plugin: webhook, config: {url: "https://b"} }
  - { name: c, plugin: webhook, config: {url: "https://c"} }
`)
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(res.Config.Sinks); got != 3 {
		t.Errorf("got %d sinks (a from main + b/c from drop-in), want 3", got)
	}
}

func TestLoad_NoFile(t *testing.T) {
	p := pathsForTempDir(t, t.TempDir())
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.IsConfigured() {
		t.Error("a fresh dir should not look configured")
	}
	if res.Config.Schema != "" {
		t.Errorf("Schema should be empty; got %q", res.Config.Schema)
	}
	if len(res.SourceFiles) != 1 {
		t.Errorf("expected 1 attempted source file, got %d", len(res.SourceFiles))
	}
	if res.SourceFiles[0].ReadOK {
		t.Error("the missing main file should have ReadOK=false")
	}
}

func TestLoad_MainFileOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
schema: pg_hardstorage.config.v1
paths:
  root: /opt/pg_hardstorage
llm:
  provider: ollama
  privacy: local-only
`)
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !res.IsConfigured() {
		t.Fatal("IsConfigured should be true")
	}
	if res.Config.Schema != config.Schema {
		t.Errorf("Schema = %q", res.Config.Schema)
	}
	if res.Config.Paths.Root != "/opt/pg_hardstorage" {
		t.Errorf("Paths.Root = %q", res.Config.Paths.Root)
	}
	if res.Config.LLM.Provider != "ollama" {
		t.Errorf("LLM.Provider = %q", res.Config.LLM.Provider)
	}
	if res.Config.LLM.Privacy != "local-only" {
		t.Errorf("LLM.Privacy = %q", res.Config.LLM.Privacy)
	}
}

func TestLoad_DropInsApplyInOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
llm:
  provider: openai
  privacy: standard
`)
	// 90-overrides should override 10-base.
	writeFile(t, filepath.Join(dir, "conf.d", "10-base.yaml"), `
llm:
  privacy: strict
`)
	writeFile(t, filepath.Join(dir, "conf.d", "90-overrides.yaml"), `
llm:
  provider: bedrock
`)
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.LLM.Provider; got != "bedrock" {
		t.Errorf("LLM.Provider after merge = %q, want bedrock", got)
	}
	if got := res.Config.LLM.Privacy; got != "strict" {
		t.Errorf("LLM.Privacy after merge = %q, want strict", got)
	}
	if len(res.SourceFiles) != 3 { // main + 2 drop-ins
		t.Errorf("expected 3 source files; got %d", len(res.SourceFiles))
	}
	// All sources should be ReadOK.
	for _, sf := range res.SourceFiles {
		if !sf.ReadOK {
			t.Errorf("expected ReadOK for %s", sf.Path)
		}
	}
}

func TestLoad_RejectsForeignSchema(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
schema: pg_hardstorage.config.v999
`)
	p := pathsForTempDir(t, dir)
	_, err := config.Load(p)
	if err == nil {
		t.Fatal("expected schema rejection")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("error should mention schema; got %q", err)
	}
}

func TestLoad_ParseError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
schema: "pg_hardstorage.config.v1
`) // intentionally malformed (unclosed quote)
	p := pathsForTempDir(t, dir)
	_, err := config.Load(p)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoad_DropInDirMissingIsOK(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `schema: pg_hardstorage.config.v1`)
	// Note: no conf.d/ directory at all.
	p := pathsForTempDir(t, dir)
	if _, err := config.Load(p); err != nil {
		t.Fatalf("missing drop-in dir should not error: %v", err)
	}
}

func TestLoad_NonYAMLFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), ``)
	writeFile(t, filepath.Join(dir, "conf.d", "README.txt"), `not yaml`)
	writeFile(t, filepath.Join(dir, "conf.d", "ok.yml"), `llm: { provider: openai }`)
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.LLM.Provider != "openai" {
		t.Errorf("ok.yml should have been picked up; got %+v", res.Config.LLM)
	}
	for _, sf := range res.SourceFiles {
		if strings.HasSuffix(sf.Path, "README.txt") {
			t.Errorf("README.txt should not be in source files: %v", sf)
		}
	}
}

// TestLoad_DeploymentPatroniRoundTrip: the v0.6+ Patroni config
// block round-trips cleanly. Pins the YAML field names against
// regression — the Patroni follower coordinator relies on these
// names to opt deployments in.
func TestLoad_DeploymentPatroniRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://backup@db1.example.com/postgres
    repo: file:///var/lib/pg_hardstorage/db1
    patroni:
      url: http://patroni-leader:8008
      user: opspg
      password: secret
      slot: hs_db1_custom
      interval: 3s
  db2:
    pg_connection: postgres://backup@db2.example.com/postgres
    repo: file:///var/lib/pg_hardstorage/db2
    # No patroni block: IsEnabled() returns false for db2.
`)
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	db1, ok := res.Config.Deployments["db1"]
	if !ok {
		t.Fatal("db1 missing from deployments")
	}
	if !db1.Patroni.IsEnabled() {
		t.Error("db1.Patroni.IsEnabled() = false (URL is set)")
	}
	if db1.Patroni.URL != "http://patroni-leader:8008" {
		t.Errorf("db1.Patroni.URL = %q", db1.Patroni.URL)
	}
	if db1.Patroni.User != "opspg" {
		t.Errorf("db1.Patroni.User = %q", db1.Patroni.User)
	}
	if db1.Patroni.Password != "secret" {
		t.Errorf("db1.Patroni.Password = %q", db1.Patroni.Password)
	}
	if db1.Patroni.Slot != "hs_db1_custom" {
		t.Errorf("db1.Patroni.Slot = %q", db1.Patroni.Slot)
	}
	if db1.Patroni.Interval != "3s" {
		t.Errorf("db1.Patroni.Interval = %q", db1.Patroni.Interval)
	}

	db2, ok := res.Config.Deployments["db2"]
	if !ok {
		t.Fatal("db2 missing")
	}
	if db2.Patroni.IsEnabled() {
		t.Error("db2.Patroni.IsEnabled() = true; should be false (no URL)")
	}
}

// TestPatroniConfig_IsEnabled: regression guard on the trigger
// condition. The agent's per-deployment goroutine spawn pivots
// on this; renaming or rewriting the predicate would silently
// disable the leader-follow loop.
func TestPatroniConfig_IsEnabled(t *testing.T) {
	cases := []struct {
		cfg  config.PatroniConfig
		want bool
	}{
		{config.PatroniConfig{}, false},
		{config.PatroniConfig{URL: ""}, false},
		{config.PatroniConfig{URL: "http://p:8008"}, true},
		{config.PatroniConfig{URL: "http://p:8008", User: "u"}, true},
	}
	for _, c := range cases {
		if got := c.cfg.IsEnabled(); got != c.want {
			t.Errorf("PatroniConfig{URL=%q}.IsEnabled() = %v, want %v",
				c.cfg.URL, got, c.want)
		}
	}
}

// TestLoad_DeploymentPatroniMultiSlotRoundTrip: the v0.6+
// Mechanism 3 dual-slot config (patroni.slots array)
// round-trips. Each entry's name + role survives YAML decode.
func TestLoad_DeploymentPatroniMultiSlotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://backup@host/db
    repo: file:///var/lib/pg_hardstorage/db1
    patroni:
      url: http://patroni:8008
      slots:
        - { name: hs_db1_primary, role: leader }
        - { name: hs_db1_replica, role: replica }
`)
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	db1 := res.Config.Deployments["db1"]
	if len(db1.Patroni.Slots) != 2 {
		t.Fatalf("len(Slots) = %d, want 2", len(db1.Patroni.Slots))
	}
	if db1.Patroni.Slots[0].Name != "hs_db1_primary" || db1.Patroni.Slots[0].Role != "leader" {
		t.Errorf("Slots[0] = %+v", db1.Patroni.Slots[0])
	}
	if db1.Patroni.Slots[1].Name != "hs_db1_replica" || db1.Patroni.Slots[1].Role != "replica" {
		t.Errorf("Slots[1] = %+v", db1.Patroni.Slots[1])
	}
	if db1.Patroni.Slot != "" {
		t.Errorf("Slot should be empty when Slots is set; got %q", db1.Patroni.Slot)
	}
}

// TestLoad_LLMFieldsRoundTrip — regression for B14.  mergeConfig
// silently dropped every LLM field except Provider and Privacy.
// Operators following the sample yaml's `api_key_file:` (or
// `api_key:`) line got an empty struct downstream and saw
// `llm.provider_open_failed: APIKey is required for the
// canonical OpenAI endpoint` even with a correct yaml.
//
// Pre-fix this test fails because Endpoint / Model / APIKey /
// APIKeyFile / MaxTokens / Extra round-trip to their zero values.
func TestLoad_LLMFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
schema: pg_hardstorage.config.v1
llm:
  provider: openai
  endpoint: https://example.test/v1
  model: gpt-4o-mini
  api_key: sk-test-INLINE-KEY-OK
  api_key_file: /etc/pg_hardstorage/openai.key
  max_tokens: 8192
  privacy: redact
  extra:
    api_key_header: api-key
    x-extra: zzz
`)
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	llm := res.Config.LLM
	cases := []struct {
		name, got, want string
	}{
		{"Provider", llm.Provider, "openai"},
		{"Endpoint", llm.Endpoint, "https://example.test/v1"},
		{"Model", llm.Model, "gpt-4o-mini"},
		{"APIKey", llm.APIKey, "sk-test-INLINE-KEY-OK"},
		{"APIKeyFile", llm.APIKeyFile, "/etc/pg_hardstorage/openai.key"},
		{"Privacy", llm.Privacy, "redact"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if llm.MaxTokens != 8192 {
		t.Errorf("MaxTokens = %d, want 8192", llm.MaxTokens)
	}
	if llm.Extra["api_key_header"] != "api-key" {
		t.Errorf("Extra[api_key_header] = %v, want %q", llm.Extra["api_key_header"], "api-key")
	}
	if llm.Extra["x-extra"] != "zzz" {
		t.Errorf("Extra[x-extra] = %v, want %q", llm.Extra["x-extra"], "zzz")
	}
}

// TestLoad_LLMDropInOverridesEndpoint — drop-in precedence on
// LLM fields.  Base yaml sets endpoint=A; a higher-precedence
// drop-in sets endpoint=B.  The result must be B.  Pre-fix
// mergeConfig didn't copy Endpoint at all, so the drop-in value
// was silently lost.
func TestLoad_LLMDropInOverridesEndpoint(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
schema: pg_hardstorage.config.v1
llm:
  provider: openai
  endpoint: https://base.test/v1
  model: base-model
`)
	dropIn := filepath.Join(dir, "conf.d")
	if err := os.MkdirAll(dropIn, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dropIn, "90-overrides.yaml"), `
schema: pg_hardstorage.config.v1
llm:
  endpoint: https://override.test/v1
  model: override-model
  api_key: sk-override
`)
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	llm := res.Config.LLM
	if llm.Provider != "openai" {
		t.Errorf("Provider = %q (base should survive), want %q", llm.Provider, "openai")
	}
	if llm.Endpoint != "https://override.test/v1" {
		t.Errorf("Endpoint = %q (drop-in should win)", llm.Endpoint)
	}
	if llm.Model != "override-model" {
		t.Errorf("Model = %q (drop-in should win)", llm.Model)
	}
	if llm.APIKey != "sk-override" {
		t.Errorf("APIKey = %q (drop-in only; should be present)", llm.APIKey)
	}
}

// TestLoad_LLMExtraMergesKeyByKey — Extra is a map; drop-ins
// should overlay key-by-key, not wholesale replace.  Operators
// declare common extras in the base file and add per-env
// extras in a 90-*.yaml drop-in.
func TestLoad_LLMExtraMergesKeyByKey(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
schema: pg_hardstorage.config.v1
llm:
  provider: openai
  extra:
    base-key: base-val
    common: from-base
`)
	dropIn := filepath.Join(dir, "conf.d")
	if err := os.MkdirAll(dropIn, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dropIn, "90-azure.yaml"), `
schema: pg_hardstorage.config.v1
llm:
  extra:
    api-version: "2024-02-15-preview"
    common: from-dropin
`)
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	extra := res.Config.LLM.Extra
	if extra["base-key"] != "base-val" {
		t.Errorf("base-key dropped: %v", extra["base-key"])
	}
	if extra["api-version"] != "2024-02-15-preview" {
		t.Errorf("api-version missing: %v", extra["api-version"])
	}
	if extra["common"] != "from-dropin" {
		t.Errorf("common = %v, want %q (drop-in wins)", extra["common"], "from-dropin")
	}
}

// TestLoad_DeploymentTDERoundTrip confirms the `tde:` block in
// deployment config parses into DeploymentConfig.TDE, including
// the informational engine + key_ref fields.  Catches a YAML-tag
// regression that would silently drop the operator's TDE
// declaration (the failure mode that would surface as `wal push`
// stamping a bogus xlp_sysid on TDE source segments).
func TestLoad_DeploymentTDERoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `
schema: pg_hardstorage.config.v1
deployments:
  encrypted-pgee:
    pg_connection: postgres://backup@db/postgres
    repo: file:///var/lib/pg_hardstorage/pgee
    tde:
      enabled: true
      engine: cybertec_enterprise
      key_ref: kms-secret://prod/pgee
  vanilla:
    pg_connection: postgres://backup@db/postgres
    repo: file:///var/lib/pg_hardstorage/vanilla
    # No tde block: Enabled defaults to false.
`)
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pgee, ok := res.Config.Deployments["encrypted-pgee"]
	if !ok {
		t.Fatal("encrypted-pgee missing from deployments")
	}
	if !pgee.TDE.Enabled {
		t.Errorf("encrypted-pgee.tde.enabled = false, want true")
	}
	if pgee.TDE.Engine != "cybertec_enterprise" {
		t.Errorf("engine = %q, want cybertec_enterprise", pgee.TDE.Engine)
	}
	if pgee.TDE.KeyRef != "kms-secret://prod/pgee" {
		t.Errorf("key_ref = %q, want kms-secret://prod/pgee", pgee.TDE.KeyRef)
	}

	vanilla, ok := res.Config.Deployments["vanilla"]
	if !ok {
		t.Fatal("vanilla missing from deployments")
	}
	if vanilla.TDE.Enabled {
		t.Errorf("vanilla.tde.enabled = true; expected default false")
	}
}

// TestLoad_SampleYAMLParses confirms the shipped sample config
// at share/pg_hardstorage.sample.yaml parses through the loader
// without error.  A regression where a sample edit drifted from
// the schema (e.g. mis-spelled YAML tag, removed required field)
// would surface here, BEFORE the operator-facing
// `pg_hardstorage init` step produces a sample that the next
// `pg_hardstorage doctor` then refuses to load.
func TestLoad_SampleYAMLParses(t *testing.T) {
	src, err := os.ReadFile("../../share/pg_hardstorage.sample.yaml")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), string(src))
	p := pathsForTempDir(t, dir)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load(sample): %v", err)
	}
	// Spot-checks against the curated examples in the sample.
	if _, ok := res.Config.Deployments["prod-db"]; !ok {
		t.Error("sample is missing the prod-db example")
	}
	pgee, ok := res.Config.Deployments["encrypted-pgee"]
	if !ok {
		t.Fatal("sample is missing the encrypted-pgee TDE example")
	}
	if !pgee.TDE.Enabled {
		t.Error("encrypted-pgee.tde.enabled must be true in the sample (this is the TDE-source example)")
	}
}
