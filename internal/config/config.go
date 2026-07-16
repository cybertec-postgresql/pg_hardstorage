// Package config loads the pg_hardstorage YAML configuration.
//
// The on-disk layout follows the spec:
//
//	<config>/pg_hardstorage.yaml      # main file
//	<config>/conf.d/*.yaml            # drop-ins, applied in lexicographic order
//	<config>/deployments/*.yaml       # one file per deployment (loaded by other slices)
//	<config>/sinks/*.yaml             # one file per sink         (ditto)
//
// For this slice we only parse the top-level file plus drop-ins. Deployment
// and sink files are reserved for the slices that need them — we keep the
// scope tight so doctor has something concrete to verify against.
//
// If no config file exists, Load returns a zero Config (with no error)
// and a SourceFiles list containing one entry per attempted path with
// ReadOK=false — so doctor can show users which paths were tried and
// missed. The absence of config is normal on a fresh box (pg_hardstorage
// init is what creates the first one).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// Schema is the wire-format identifier expected on the YAML top-level.
//
// Loaded files with a non-empty schema field MUST match this value. We
// commit to 24-month backward compatibility, same as the output schema.
const Schema = "pg_hardstorage.config.v1"

// Config is the merged top-level configuration. Fields not yet
// implemented are kept absent rather than declared with zero meanings.
type Config struct {
	// Schema is informational; if set in YAML it's validated at load time.
	Schema string `yaml:"schema,omitempty"`

	// Paths overrides path resolution at config-time. The CLI feeds this
	// into paths.Options.Root etc. so a single config file can drive the
	// "RHEL-style appliance under /opt" deployment without env vars.
	Paths PathsConfig `yaml:"paths,omitempty"`

	// LLM is a placeholder for the LLM-helper config. The structure
	// firms up in the LLM slice; for now we just round-trip the keys.
	LLM LLMConfig `yaml:"llm,omitempty"`

	// Airgapped is the air-gap policy posture.  Values:
	//
	//   off    — no enforcement (default).  Outbound endpoints are
	//            permitted regardless of where they resolve.
	//   strict — refuse every configured outbound endpoint (LLM
	//            provider, sink, OTLP collector) that doesn't
	//            resolve to loopback, RFC1918, RFC4193, CGNAT, or
	//            an entry in `airgap.allowlist`.  Schemes other
	//            than http/https/grpc/syslog (file, unix, fd,
	//            stdio) are always permitted.
	//
	// Resolution precedence: --airgapped flag > PG_HARDSTORAGE_AIRGAPPED
	// env > this field.  Set to "strict" (or "true"/"1"/"on"/"yes")
	// to enable the gate.
	Airgapped string `yaml:"airgapped,omitempty"`

	// Airgap holds policy details for strict mode.  Empty / absent
	// is fine; only `allowlist` is used today.
	Airgap AirgapConfig `yaml:"airgap,omitempty"`

	// Sinks declares the operator-configured event sinks (slack,
	// webhook, syslog, etc.). The dispatcher resolves each spec via
	// output.DefaultSinkRegistry at startup.
	//
	// Example:
	//
	//	sinks:
	//	  - name: ops-slack
	//	    plugin: slack
	//	    config:
	//	      webhook_url: https://hooks.slack.com/services/...
	//	      min_severity: warning
	Sinks []output.SinkSpec `yaml:"sinks,omitempty"`

	// Deployments declares the PG deployments the agent manages.
	// Each entry is keyed by deployment name (the same name that
	// flows into manifest.deployment, repo paths, and slot names).
	//
	// Example:
	//
	//	deployments:
	//	  db1:
	//	    pg_connection: postgres://backup@db1.example.com/postgres
	//	    repo: s3://acme-pg-backups/
	//	    schedule:
	//	      backup: { every: "6h" }
	//	      rotate: { daily_at: "04:00" }
	Deployments map[string]DeploymentConfig `yaml:"deployments,omitempty"`
}

// DeploymentConfig is one deployment's per-deployment settings,
// loaded from the top-level config map. v0.1 covers connection +
// repo + schedule; later slices add encryption, retention policy,
// notifier overrides, etc.
type DeploymentConfig struct {
	// PGConnection is a libpq URI. Same format the backup CLI
	// accepts on --pg-connection.
	PGConnection string `yaml:"pg_connection,omitempty"`

	// Repo is the repository URL.
	Repo string `yaml:"repo,omitempty"`

	// Tenant scopes the deployment for multi-tenant deployments.
	// Empty defaults to "default".
	Tenant string `yaml:"tenant,omitempty"`

	// Schedule declares the recurring tasks for this deployment.
	Schedule DeploymentSchedule `yaml:"schedule,omitempty"`

	// Retention overrides the default retention policy. v0.1 ships
	// with a hard-coded GFS default; this is forward-compat scaffolding.
	Retention RetentionConfig `yaml:"retention,omitempty"`

	// Classification is the data sensitivity tag for this deployment.
	// One of: public, internal, confidential, restricted. Empty
	// defaults to "internal" (sensible middle ground for a tool that
	// touches PostgreSQL data — the operator should opt UP for
	// restricted, not have to opt down from a default of confidential).
	//
	// Today the tag is informational + visible in doctor / fleet
	// reports.+ wires it to per-classification retention floors,
	// region pinning, and required-encryption enforcement.
	Classification string `yaml:"classification,omitempty" json:"classification,omitempty"`

	// Residency is the list of allowed regions for this deployment's
	// repository. Empty list means "no residency constraint". Each
	// entry is a case-insensitive prefix or suffix-pair match against
	// the storage plugin's reported region:
	//
	//   residency: ["eu-west-1"]      → exact-match only
	//   residency: ["eu"]             → matches any region whose
	//                                    code starts with "eu-" (eu-west-1, eu-central-1, ...)
	//   residency: ["eu", "us"]       → either EU or US regions
	//
	// Today's surface: `pg_hardstorage residency check <deployment>`
	// validates the configured repo's region against the list, and
	// `doctor` surfaces the result. Automatic enforcement at
	// backup-commit time is a lift (the runner needs a residency
	// gate before pg_backup_start).
	Residency []string `yaml:"residency,omitempty" json:"residency,omitempty"`

	// SLO declares per-deployment RPO/RTO targets. Empty fields
	// mean "no target declared"; `slo report` compares actual
	// last-backup-age against RPO and (when+ wires it)
	// observed restore times against RTO.
	SLO SLOConfig `yaml:"slo,omitempty" json:"slo,omitempty"`

	// Patroni opts the deployment into the+ leader-follow
	// coordinator (internal/wal/follower). When URL is set, the
	// agent runs a dedicated goroutine per deployment that polls
	// Patroni REST, reconciles the replication slot on leader
	// change, and captures TIMELINE_HISTORY files into the repo.
	//
	// Empty URL = no leader-follow loop (single-leader deployments
	// or non-Patroni HA setups). The coordinator is opt-in because
	// it adds REST polling traffic + a long-running goroutine that
	// every operator shouldn't pay for.
	Patroni PatroniConfig `yaml:"patroni,omitempty" json:"patroni,omitempty"`

	// TDE declares that the source PostgreSQL has Transparent Data
	// Encryption enabled (CYBERTEC PGEE, pg_tde, EDB TDE, or
	// equivalent).  When set, every code path in pg_hardstorage
	// that would otherwise PARSE bytes off disk treats those bytes
	// as opaque ciphertext and skips its inspection:
	//
	//   - `wal push %p` (archive_command target) does NOT extract
	//     xlp_sysid from the segment file's first-page header;
	//     `--system-identifier` or `--pg-connection` must supply
	//     it explicitly.  See docs/explanation/tde-awareness.md.
	//   - The manifest is stamped with TDE source markers so
	//     restore preflight can refuse a "TDE backup → vanilla PG"
	//     mismatch loudly.
	//
	// What does NOT change under TDE:
	//
	//   - BASE_BACKUP, START_REPLICATION, and START_REPLICATION
	//     LOGICAL all deliver plaintext over the wire because the
	//     PGEE / pg_tde server-side decryption happens above the
	//     replication boundary.  Our chunker is content-defined
	//     (not page-aware) and treats input bytes opaquely either
	//     way, so the on-the-wire path needs no changes.
	//   - System identifier still comes from IDENTIFY_SYSTEM, a
	//     server function call returning plaintext.
	//   - pg_verifybackup is run by PG inside the restored
	//     sandbox.  When sandboxing a TDE backup, the sandbox PG
	//     must be a TDE-capable image with key access; otherwise
	//     the verify gate is skipped (see verify documentation).
	//
	// Default (zero value, Enabled=false) means "no TDE" — the
	// historical behaviour with strict on-disk header parsing.
	TDE TDEConfig `yaml:"tde,omitempty" json:"tde,omitempty"`
}

// TDEConfig declares Transparent Data Encryption posture for a
// deployment.  All fields are forward-compatible: future TDE engines
// (pg_tde, EDB TDE) can be expressed by setting `Engine` without a
// schema change.  An empty Engine + Enabled=true is accepted (we
// don't bind to a specific engine for the relaxation behaviour;
// the operator's word is enough).
type TDEConfig struct {
	// Enabled flips the relaxed-inspection posture on.  When false
	// (default) every byte-parsing path runs as today; the rest
	// of this struct is ignored.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// Engine is a free-form label naming the TDE implementation —
	// "cybertec_enterprise", "pg_tde", "edb_tde", or whatever
	// matches the operator's deployment.  Stamped on every
	// manifest for forensic / migration purposes.  Empty is
	// accepted and stamps as the literal "unspecified".
	Engine string `yaml:"engine,omitempty" json:"engine,omitempty"`

	// KeyRef is an operator-supplied opaque reference that
	// identifies which KEK / wrapping-key set protects this
	// deployment's data.  pg_hardstorage never inspects this
	// field's value — it's stamped on the manifest so a future
	// restore against a different cluster can refuse cleanly if
	// the operator notices a KeyRef mismatch.  Empty means
	// "operator did not declare a key reference".
	KeyRef string `yaml:"key_ref,omitempty" json:"key_ref,omitempty"`
}

// PatroniConfig configures the leader-follow coordinator for one
// deployment. URL is required when Patroni is enabled; the rest
// have sensible defaults documented per-field.
//
// Single-slot mode (Mechanism 2): set `slot:` (or leave empty for
// the default `pg_hardstorage_<deployment>`); the slot is
// pinned to whichever member is the current leader, recreated
// after every Patroni failover.
//
// Multi-slot mode (Mechanism 3): set `slots:` to a list of
// {name, role} pairs. Each slot lives on the named role (leader
// or replica). The dual-slot resilience pattern is:
//
//	patroni:
//	  url: http://patroni:8008
//	  slots:
//	    - { name: pg_hardstorage_db1_primary, role: leader }
//	    - { name: pg_hardstorage_db1_replica, role: replica }
//
// Setting both `slot` and `slots` is invalid. Setting neither
// falls through to the legacy default-named single-slot.
type PatroniConfig struct {
	// URL is the Patroni REST base URL (e.g.
	// http://patroni-leader:8008). When empty the coordinator
	// is disabled for this deployment. Required when enabled.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`

	// User + Password configure HTTP basic-auth for Patroni
	// REST. Patroni's read-only endpoints (/cluster, /leader)
	// typically don't require auth; mutating endpoints
	// (switchover, restart) do. Empty means no auth header.
	User     string `yaml:"user,omitempty" json:"user,omitempty"`
	Password string `yaml:"password,omitempty" json:"password,omitempty"`

	PasswordFile string `yaml:"password_file,omitempty" json:"password_file,omitempty"`

	// Slot is the single-slot Mechanism 2 slot name. Empty
	// defaults to "pg_hardstorage_<deployment>". Mutually
	// exclusive with Slots.
	Slot string `yaml:"slot,omitempty" json:"slot,omitempty"`

	// Slots is the multi-slot Mechanism 3 spec. Each entry
	// names a slot + the cluster role it lives on
	// ("leader" or "replica"). Mutually exclusive with Slot.
	Slots []PatroniSlot `yaml:"slots,omitempty" json:"slots,omitempty"`

	// Interval is the Patroni poll cadence. Empty defaults to
	// patroni.DefaultFollowInterval (5s).
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty"`
}

// PatroniSlot is one entry in PatroniConfig.Slots.
type PatroniSlot struct {
	// Name is the physical replication slot name on the PG
	// server (e.g., "pg_hardstorage_db1_primary"). Required.
	Name string `yaml:"name" json:"name"`

	// Role is "leader" or "replica" — which cluster member
	// the slot lives on. Required.
	Role string `yaml:"role" json:"role"`
}

// IsEnabled reports whether Patroni is opt-in for this
// deployment. Used by the agent to decide whether to spawn a
// leader-follow goroutine.
func (p PatroniConfig) IsEnabled() bool { return p.URL != "" }

// SLOConfig holds the per-deployment RPO/RTO targets in seconds.
// Stored as duration-strings in YAML so operators write
// "rpo: 1h" / "rpo: 30m" / "rpo: 24h"; we parse to seconds at load
// time. The integer-seconds field is the canonical form for
// programmatic consumers (monitoring exporters, slo report bodies).
type SLOConfig struct {
	// RPO is the maximum acceptable lag in seconds between the
	// latest committed backup and now. Zero means "not declared".
	RPOSeconds int64 `yaml:"rpo_seconds,omitempty" json:"rpo_seconds,omitempty"`
	// RTO is the maximum acceptable restore time in seconds. Today
	// this is informational;+ correlates with verifier sandbox
	// restore timings.
	RTOSeconds int64 `yaml:"rto_seconds,omitempty" json:"rto_seconds,omitempty"`
}

// DeploymentSchedule lists per-task schedule specs.
type DeploymentSchedule struct {
	Backup      ScheduleSpec `yaml:"backup,omitempty"`
	Rotate      ScheduleSpec `yaml:"rotate,omitempty"`
	AuditAnchor ScheduleSpec `yaml:"audit_anchor,omitempty"`
}

// ScheduleSpec mirrors internal/schedule.Spec exactly; we redeclare
// it here so config doesn't pull internal/schedule into its public
// import surface (avoids a cycle when schedule wants to read config).
type ScheduleSpec struct {
	Every   string `yaml:"every,omitempty"   json:"every,omitempty"`
	DailyAt string `yaml:"daily_at,omitempty" json:"daily_at,omitempty"`
	At      string `yaml:"at,omitempty"      json:"at,omitempty"`
}

// IsZero reports whether nothing has been configured.
func (s ScheduleSpec) IsZero() bool {
	return s.Every == "" && s.DailyAt == "" && s.At == ""
}

// RetentionConfig is the operator-controlled retention policy.
// Forward-compat shape; v0.1 ignores all fields and uses hard defaults.
type RetentionConfig struct {
	Policy      string `yaml:"policy,omitempty"` // "gfs", "simple", "count"
	KeepDaily   int    `yaml:"keep_daily,omitempty"`
	KeepWeekly  int    `yaml:"keep_weekly,omitempty"`
	KeepMonthly int    `yaml:"keep_monthly,omitempty"`
	KeepYearly  int    `yaml:"keep_yearly,omitempty"`
	KeepFor     string `yaml:"keep_for,omitempty"` // duration string
	KeepFulls   int    `yaml:"keep_fulls,omitempty"`
}

// PathsConfig mirrors the paths-package overrides we accept from YAML.
type PathsConfig struct {
	// Root corresponds to PG_HARDSTORAGE_ROOT — when set, every domain
	// is resolved as a subdirectory of this prefix.
	Root string `yaml:"root,omitempty"`
}

// AirgapConfig is the operator-tunable side of the air-gap
// policy.  The Mode itself is set at the top level via the
// `airgapped` key (so a single line `airgapped: strict` flips
// the binary into air-gap mode).  This struct holds the
// per-policy details that apply once strict is in effect.
type AirgapConfig struct {
	// Allowlist holds host names (and host:port) the air-gap
	// policy permits even though they aren't loopback / RFC1918.
	// Useful for an in-perimeter FQDN that resolves to a public
	// IP via split-horizon DNS, or for a private VPC endpoint
	// behind a routable hostname.  Comparison is case-insensitive
	// on host; ports must match exactly when present.
	Allowlist []string `yaml:"allowlist,omitempty"`
}

// LLMConfig configures the LLM helper.  The whole stack speaks
// the OpenAI Chat Completions wire format; operators
// point Endpoint at any compatible service (api.openai.com,
// Azure, Ollama, vLLM, OpenRouter, ...).
type LLMConfig struct {
	// Provider is the registered provider name. + ships
	// "openai" (the production default) and "mock" (tests).
	// Empty defaults to "openai".
	Provider string `yaml:"provider,omitempty"`

	// Endpoint overrides the provider's default base URL.  Empty
	// uses the provider's default (api.openai.com for the openai
	// provider).  Examples:
	//
	//   endpoint: http://127.0.0.1:11434/v1   # Ollama
	//   endpoint: https://my-resource.openai.azure.com   # Azure
	//   endpoint: https://openrouter.ai/api/v1
	Endpoint string `yaml:"endpoint,omitempty"`

	// Model is the model id.  Provider-specific.  Empty uses the
	// provider's documented default ("gpt-4o-mini" for openai).
	Model string `yaml:"model,omitempty"`

	// APIKey carries the credential inline.  Discouraged for
	// production — use APIKeyFile or set OPENAI_API_KEY in the
	// environment instead.  When the file is world-readable the
	// CLI prints a warning at startup.
	APIKey string `yaml:"api_key,omitempty"`

	// APIKeyFile is a path to a file containing the API key
	// (one line, optional trailing newline).  The standard
	// production setup: a 0o600 file owned by the backup user
	// at /etc/pg_hardstorage/keyring/openai.key, referenced
	// from this field.
	APIKeyFile string `yaml:"api_key_file,omitempty"`

	// MaxTokens caps the model's response length per turn.
	// Zero uses the provider's default (4096 for openai).
	MaxTokens int `yaml:"max_tokens,omitempty"`

	// Privacy is the future privacy mode (strict / standard /
	// open / local-only).  Round-tripped today; enforcement
	// lands with the privacy-mode chunk.
	Privacy string `yaml:"privacy,omitempty"`

	// Extra holds provider-specific options that don't have
	// dedicated fields — currently `api_key_header` (Azure
	// requires "api-key" instead of "Authorization: Bearer").
	Extra map[string]any `yaml:"extra,omitempty"`
}

// SourceFile records one YAML file that contributed to the merged Config.
// doctor renders these so users see exactly which files were honored.
type SourceFile struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"` // "main" | "drop_in"
	ReadOK   bool   `json:"read_ok"`
	ParseErr string `json:"parse_error,omitempty"`
}

// LoadResult bundles the merged Config with the list of files we tried.
type LoadResult struct {
	Config      Config       `json:"config"`
	SourceFiles []SourceFile `json:"source_files"`
}

// Load reads pg_hardstorage.yaml plus every conf.d/*.yaml under the
// resolved Config path, merges them in lexicographic order, and returns
// the result. Missing files are not errors; parse errors are.
//
// Inline-YAML env-var fallback (issue #87): when the env var
// PG_HARDSTORAGE_CONFIG is set, its value is parsed as YAML and
// merged at the LOWEST precedence — `pg_hardstorage.yaml` and
// any conf.d/*.yaml override it.  This is the contract the
// docker-compose evaluation stack relies on (operators expect to
// configure single-binary containers by setting one env var
// instead of bind-mounting a file).  Empty / unset is a no-op.
func Load(p *paths.Paths) (*LoadResult, error) {
	if p == nil {
		return nil, errors.New("config: nil paths")
	}
	res := &LoadResult{}

	if envBody := os.Getenv("PG_HARDSTORAGE_CONFIG"); strings.TrimSpace(envBody) != "" {
		envCfg, envSF, err := loadBytes([]byte(envBody),
			"env:PG_HARDSTORAGE_CONFIG", "env")
		if err != nil {
			return nil, err
		}
		res.SourceFiles = append(res.SourceFiles, envSF)
		if envSF.ReadOK {
			res.Config = mergeConfig(res.Config, envCfg)
		}
	}

	// An explicit -c/--config file (via PG_HARDSTORAGE_CONFIG_FILE) is
	// authoritative: read exactly that file and skip the well-known
	// path + drop-in dir, so `-c staging.yaml` operates on staging.yaml
	// and nothing else. Previously the flag was silently ignored and
	// the tool always read <Config>/pg_hardstorage.yaml.
	if override := strings.TrimSpace(p.ConfigFileOverride); override != "" {
		cfg, sf, err := loadFile(override, "config_flag")
		if err != nil {
			return nil, err
		}
		res.SourceFiles = append(res.SourceFiles, sf)
		if sf.ReadOK {
			res.Config = mergeConfig(res.Config, cfg)
		}
		if err := validate(res.Config); err != nil {
			return nil, err
		}
		return res, nil
	}

	mainPath := filepath.Join(p.Config.Value, "pg_hardstorage.yaml")
	mainCfg, mainSF, err := loadFile(mainPath, "main")
	if err != nil {
		return nil, err
	}
	res.SourceFiles = append(res.SourceFiles, mainSF)
	if mainSF.ReadOK {
		res.Config = mergeConfig(res.Config, mainCfg)
	}

	dropInDir := p.ConfigDropIn.Value
	entries, err := os.ReadDir(dropInDir)
	if err == nil {
		// Apply in lexicographic order — later wins. 90-overrides.yaml
		// beats 10-base.yaml, mirroring how /etc/sysctl.d / sudoers.d
		// work. Operators name files with a numeric prefix to control
		// precedence.
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Name() < entries[j].Name()
		})
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !(filepath.Ext(name) == ".yaml" || filepath.Ext(name) == ".yml") {
				continue
			}
			full := filepath.Join(dropInDir, name)
			cfg, sf, err := loadFile(full, "drop_in")
			if err != nil {
				return nil, err
			}
			res.SourceFiles = append(res.SourceFiles, sf)
			if sf.ReadOK {
				res.Config = mergeConfig(res.Config, cfg)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("config: read drop-in dir %s: %w", dropInDir, err)
	}

	if err := validate(res.Config); err != nil {
		return nil, err
	}
	return res, nil
}

// loadBytes parses one YAML blob (from a file or from
// PG_HARDSTORAGE_CONFIG).  Shared by loadFile and Load's env-var
// path so the parsing semantics (strict KnownFields, EOF-tolerance
// for empty input) are identical regardless of source.
func loadBytes(body []byte, label, kind string) (Config, SourceFile, error) {
	sf := SourceFile{Path: label, Kind: kind}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		sf.ParseErr = err.Error()
		return Config{}, sf, fmt.Errorf("config: parse %s: %w", label, err)
	}
	sf.ReadOK = true
	return cfg, sf, nil
}

// loadFile reads one YAML file. A missing file produces ReadOK=false but
// no error; an unreadable or unparseable file is an error.
func loadFile(path, kind string) (Config, SourceFile, error) {
	sf := SourceFile{Path: path, Kind: kind}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, sf, nil
		}
		return Config{}, sf, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		sf.ParseErr = err.Error()
		return Config{}, sf, fmt.Errorf("config: parse %s: %w", path, err)
	}
	sf.ReadOK = true
	return cfg, sf, nil
}

// mergeConfig overlays b on top of a, with b winning for non-zero fields.
// At this point the Config struct is shallow enough for a hand-written
// merge to be the most legible option. We can switch to mergo or similar
// when the struct grows.
func mergeConfig(a, b Config) Config {
	if b.Schema != "" {
		a.Schema = b.Schema
	}
	if b.Paths.Root != "" {
		a.Paths.Root = b.Paths.Root
	}
	if b.LLM.Provider != "" {
		a.LLM.Provider = b.LLM.Provider
	}
	if b.LLM.Endpoint != "" {
		a.LLM.Endpoint = b.LLM.Endpoint
	}
	if b.LLM.Model != "" {
		a.LLM.Model = b.LLM.Model
	}
	if b.LLM.APIKey != "" {
		a.LLM.APIKey = b.LLM.APIKey
	}
	if b.LLM.APIKeyFile != "" {
		a.LLM.APIKeyFile = b.LLM.APIKeyFile
	}
	if b.LLM.MaxTokens != 0 {
		a.LLM.MaxTokens = b.LLM.MaxTokens
	}
	if b.LLM.Privacy != "" {
		a.LLM.Privacy = b.LLM.Privacy
	}
	if len(b.LLM.Extra) > 0 {
		// Drop-in Extra overlays the base map key-by-key (not a
		// wholesale replace) so a single 90-azure.yaml drop-in
		// can add api-version without stomping on other extras
		// the base config set.
		if a.LLM.Extra == nil {
			a.LLM.Extra = map[string]any{}
		}
		for k, v := range b.LLM.Extra {
			a.LLM.Extra[k] = v
		}
	}
	if b.Airgapped != "" {
		a.Airgapped = b.Airgapped
	}
	if len(b.Airgap.Allowlist) > 0 {
		// Same posture as sinks: drop-ins APPEND to the
		// allowlist. Operators who want a hard reset can
		// declare an empty list in a higher-precedence file
		// — but the typical case is "base file declares two
		// fixed entries, drop-in adds one more for a new
		// integration."
		a.Airgap.Allowlist = append(a.Airgap.Allowlist, b.Airgap.Allowlist...)
	}
	if len(b.Sinks) > 0 {
		// Drop-ins APPEND sinks rather than replace — common pattern
		// is to ship a base config with one slack sink and add an
		// audit-cef sink via a 90-audit.yaml drop-in. If an operator
		// genuinely wants to clear inherited sinks, they can declare
		// `sinks: []` in a higher-precedence drop-in (b.Sinks would
		// be a non-nil empty slice, which we honour as "replace").
		// In v0.1 we treat any present `sinks:` key as additive.
		a.Sinks = append(a.Sinks, b.Sinks...)
	}
	if len(b.Deployments) > 0 {
		// Deployments overlay by NAME — drop-in entry under name X
		// REPLACES the inherited entry under name X. This matches
		// operator intuition: "I want to override db1's schedule"
		// shouldn't merge two schedules accidentally.
		if a.Deployments == nil {
			a.Deployments = map[string]DeploymentConfig{}
		}
		for name, dep := range b.Deployments {
			if existing, ok := a.Deployments[name]; ok {
				a.Deployments[name] = mergeDeployment(existing, dep)
			} else {
				a.Deployments[name] = dep
			}
		}
	}
	return a
}

// validate enforces the schema contract. Empty schema is allowed for
// fresh installs / upgrades; a non-empty value MUST match Schema exactly.
//
// Per-deployment validation runs after the schema check so an
// operator with a misspelled `role: leadr` learns about it at
// `pg_hardstorage doctor` (or any command that loads the config)
// rather than 12 hours later when the WAL streamer first tries
// to consult Patroni.
func validate(c Config) error {
	if c.Schema != "" && c.Schema != Schema {
		return fmt.Errorf("config: schema %q is not supported; expected %q", c.Schema, Schema)
	}
	for name, dep := range c.Deployments {
		if err := validateDeployment(name, dep); err != nil {
			return err
		}
	}
	return nil
}

// validateDeployment runs the per-deployment shape checks. Today
// this covers Patroni slot configuration (Mechanism 2 vs
// Mechanism 3 mutual exclusivity, Slots[].Role values, duplicate
// names). Adding new checks here is the right place — the rest
// of the load path is shape-only and won't catch semantic
// errors.
func validateDeployment(name string, dep DeploymentConfig) error {
	if err := ValidDeploymentName(name); err != nil {
		return err
	}
	p := dep.Patroni
	// PatroniConfig is a value type — "no patroni configured"
	// is the zero-value (empty URL + no Slot/Slots). Skip
	// validation in that case so deployments without Patroni
	// load cleanly.
	if p.URL == "" && p.Slot == "" && len(p.Slots) == 0 {
		return nil
	}

	// Mechanism 2 (single Slot) vs Mechanism 3 (Slots) are
	// mutually exclusive — both populated is ambiguous and
	// almost certainly a copy-paste bug.
	if p.Slot != "" && len(p.Slots) > 0 {
		return fmt.Errorf("config: deployment %q: patroni.slot and patroni.slots are mutually exclusive (Mechanism 2 picks one, Mechanism 3 the other)", name)
	}

	// Mechanism 2 slot name must be a valid PG identifier — it
	// will be interpolated unquoted into CREATE_REPLICATION_SLOT
	// at use time. We catch shape errors here so the agent fails
	// at config-load (loud + early) rather than at first slot use
	// (a wedged hand-off path that's harder to recover from).
	if p.Slot != "" && !pg.ValidIdentifier(p.Slot) {
		return fmt.Errorf("config: deployment %q: patroni.slot %q is not a valid PG identifier "+
			"(letter/underscore start, then [a-z0-9_], ≤63 chars)", name, p.Slot)
	}

	// Per-slot validation for Mechanism 3.
	seen := make(map[string]struct{}, len(p.Slots))
	for i, s := range p.Slots {
		if s.Name == "" {
			return fmt.Errorf("config: deployment %q: patroni.slots[%d].name is required", name, i)
		}
		if !pg.ValidIdentifier(s.Name) {
			return fmt.Errorf("config: deployment %q: patroni.slots[%d].name %q is not a valid PG identifier "+
				"(letter/underscore start, then [a-z0-9_], ≤63 chars)", name, i, s.Name)
		}
		if _, dup := seen[s.Name]; dup {
			return fmt.Errorf("config: deployment %q: patroni.slots has duplicate name %q", name, s.Name)
		}
		seen[s.Name] = struct{}{}
		switch s.Role {
		case "leader", "replica":
			// ok
		case "":
			return fmt.Errorf("config: deployment %q: patroni.slots[%d] (%q): role is required (\"leader\" or \"replica\")", name, i, s.Name)
		default:
			return fmt.Errorf("config: deployment %q: patroni.slots[%d] (%q): role %q is not a valid value (expected \"leader\" or \"replica\")", name, i, s.Name, s.Role)
		}
	}
	return nil
}

// Marshal returns the YAML serialisation of cfg. Used by `init`
// to write the merged config back out as a canonical pg_hardstorage.yaml.
//
// The output preserves field order from the struct definition (yaml.v3
// emits in declaration order). Empty / zero-value fields with the
// `omitempty` YAML tag are dropped, so the file stays compact even
// for partial configs.
func Marshal(cfg *Config) ([]byte, error) {
	if cfg == nil {
		return nil, errors.New("config: marshal nil config")
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: marshal: %w", err)
	}
	// Add a small header comment so anyone opening the file knows
	// what it is and which schema version it targets.
	header := "# pg_hardstorage configuration\n# Schema: " + Schema + "\n# This file is managed by `pg_hardstorage init`. Hand edits are preserved.\n\n"
	return append([]byte(header), body...), nil
}

// IsConfigured reports whether at least one source file was successfully read.
// doctor uses this to distinguish "fresh install (run pg_hardstorage init)"
// from "config present and parsed".
func (r *LoadResult) IsConfigured() bool {
	for _, sf := range r.SourceFiles {
		if sf.ReadOK {
			return true
		}
	}
	return false
}

var validDeploymentNameRegexp = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,62}$`)

// ValidDeploymentName returns nil if name is a legal deployment
// identifier: leading letter, then letters / digits / underscore /
// hyphen, up to 63 characters total. Returns a descriptive error
// otherwise. Used at CLI parse time and when loading config.
func ValidDeploymentName(name string) error {
	if name == "" {
		return fmt.Errorf("config: deployment name is required")
	}
	if !validDeploymentNameRegexp.MatchString(name) {
		return fmt.Errorf("config: deployment name %q is invalid; must match [a-zA-Z][a-zA-Z0-9_-]{1,63}", name)
	}
	return nil
}

func mergeDeployment(existing, overlay DeploymentConfig) DeploymentConfig {
	if overlay.PGConnection != "" {
		existing.PGConnection = overlay.PGConnection
	}
	if overlay.Repo != "" {
		existing.Repo = overlay.Repo
	}
	if overlay.Tenant != "" {
		existing.Tenant = overlay.Tenant
	}
	if overlay.Schedule.Backup.Every != "" || overlay.Schedule.Backup.DailyAt != "" || overlay.Schedule.Backup.At != "" {
		existing.Schedule.Backup = overlay.Schedule.Backup
	}
	if overlay.Schedule.Rotate.Every != "" || overlay.Schedule.Rotate.DailyAt != "" || overlay.Schedule.Rotate.At != "" {
		existing.Schedule.Rotate = overlay.Schedule.Rotate
	}
	if overlay.Patroni.URL != "" {
		existing.Patroni.URL = overlay.Patroni.URL
	}
	if overlay.Patroni.User != "" {
		existing.Patroni.User = overlay.Patroni.User
	}
	if overlay.Patroni.Password != "" {
		existing.Patroni.Password = overlay.Patroni.Password
	}
	if overlay.Patroni.PasswordFile != "" {
		existing.Patroni.PasswordFile = overlay.Patroni.PasswordFile
	}
	if overlay.Patroni.Slot != "" {
		existing.Patroni.Slot = overlay.Patroni.Slot
	}
	if len(overlay.Patroni.Slots) > 0 {
		existing.Patroni.Slots = overlay.Patroni.Slots
	}
	return existing
}
