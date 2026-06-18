// Package scenario parses + represents a *.scenario.yaml file.
//
// A scenario is the testkit's top-level executable unit: which topology
// to bring up, which load file to run, what steps to take, what to
// assert. Same KnownFields posture as the load parser — typos surface
// loudly.
package scenario

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/assert"
)

// SchemaScenario is the YAML schema string. 24-month back-compat
// commitment via the same major-version contract as everything else.
const SchemaScenario = "pg_hardstorage.scenario.v1"

// Scenario is the parsed YAML.
type Scenario struct {
	Schema      string `yaml:"schema"`
	Name        string `yaml:"name"`
	Tier        string `yaml:"tier,omitempty"`
	Description string `yaml:"description,omitempty"`

	Topology Topology `yaml:"topology"`
	// Sink is the storage backend the scenario's repo lives
	// in.  Empty defaults to file:// under the artefact dir
	// (legacy behaviour); a `kind:` value brings up an
	// emulator container (MinIO for s3-minio, Azurite for
	// azurite, ...).  See internal/testkit/sink for the set.
	Sink   SinkSpec `yaml:"sink,omitempty"`
	Agents []Agent  `yaml:"agents,omitempty"`
	Load   LoadRef  `yaml:"load,omitempty"`
	Steps  []Step   `yaml:"steps"`

	// Asserts run AFTER all steps. Per-step asserts live inside
	// each Step block.
	AssertsRaw yaml.Node          `yaml:"asserts,omitempty"`
	Asserts    []assert.Assertion `yaml:"-"`

	Cleanup Cleanup `yaml:"cleanup,omitempty"`
}

// Topology declares which infrastructure provider to use.
type Topology struct {
	Provider     string `yaml:"provider"` // local-docker | testcontainers | kind | k8s-remote | ssh-inventory | cloud-vms | firecracker
	ClusterName  string `yaml:"cluster_name,omitempty"`
	Operator     string `yaml:"operator,omitempty"` // cnpg | zalando | crunchy
	PGVersion    string `yaml:"pg_version,omitempty"`
	Replicas     int    `yaml:"replicas,omitempty"`
	Filesystem   string `yaml:"filesystem,omitempty"`
	Patroni      string `yaml:"patroni,omitempty"`
	InventoryRef string `yaml:"inventory,omitempty"` // path to test/inventory/*.yaml
	// Image overrides the topology's default container image.
	// Honoured by the local-docker provider (any tag testcontainers
	// can `docker run` with the same wait-strategy as the official
	// postgres image).  Used by L4 scenarios that need a non-PG-PID-1
	// runtime (pg_upgrade) or both PG majors (cross-major upgrade).
	// Empty falls back to the provider's default
	// (`postgres:<PGVersion>`).
	Image string `yaml:"image,omitempty"`

	// ExtraGUCs are postgresql.conf settings to apply at server
	// start via `postgres -c <name>=<value>`.  These override the
	// topology's default GUCs AND cannot be reset later by
	// `ALTER SYSTEM` (per PG's PGC_S_ARGV precedence over
	// PGC_S_FILE) — exactly what scenarios exercising
	// out-of-the-box bad-config postures need.  Concretely:
	// L3_incremental_summarize_wal_flip sets
	// `summarize_wal: "off"` here to prove the agent fails
	// cleanly when the GUC is off at backup time.
	// Honoured by local-docker; ignored by topologies that
	// don't own the postmaster (k8s-remote, ssh-inventory).
	ExtraGUCs map[string]string `yaml:"extra_gucs,omitempty"`
}

// Agent declares one pg_hardstorage agent to bring up.
type Agent struct {
	On      string         `yaml:"on"`
	Version string         `yaml:"version,omitempty"`
	Config  map[string]any `yaml:"config,omitempty"`
}

// LoadRef references a load YAML file.
type LoadRef struct {
	File string `yaml:"file"`
}

// SinkSpec selects the storage backend the scenario's repo
// uses.  Empty Kind keeps file:// (legacy); a non-empty Kind
// must match an entry in internal/testkit/sink.SinkImages.
type SinkSpec struct {
	// Kind is the sink registry key — "s3-minio", "azurite",
	// "gcs-fake", "sftp".  Empty = file:// (no sink container).
	Kind string `yaml:"kind,omitempty"`
}

// Step is one declarative action. Kind names which action; the rest
// of the fields are kind-specific. Same single-key-map UnmarshalYAML
// trick as load.Operation.
type Step struct {
	Kind string `yaml:"-"`

	// take_backup
	Deployment string `yaml:"deployment,omitempty"`
	Type       string `yaml:"type,omitempty"`

	// IncrementalFrom names the parent backup an incremental
	// backup should anchor against.  Resolution order:
	//
	//   "$LAST_BACKUP"            — the most recent take_backup
	//                                in this scenario (whatever
	//                                kind: full or incremental)
	//   "<name>"                  — a previous take_backup that
	//                                set `name: <name>`
	//
	// Anything else is rejected at runner time with a
	// usage.bad_scenario error.  The runner translates the
	// resolved backup ID into the `--incremental-from` flag on
	// the underlying `pg_hardstorage backup` shell-out.
	// Requires PG 17+ on the source (the agent enforces).
	IncrementalFrom string `yaml:"incremental_from,omitempty"`

	// run_load
	Duration string `yaml:"duration,omitempty"`

	// seed
	TargetGB int `yaml:"target_gb,omitempty"`

	// wal_stream — reuses the inject Action field below as the
	// "start" | "stop" verb.

	// inject
	InjectKind string `yaml:"kind,omitempty"`
	Target     string `yaml:"target,omitempty"`
	MidOp      string `yaml:"mid_op,omitempty"`
	AtProgress int    `yaml:"at_progress,omitempty"`
	Signal     int    `yaml:"signal,omitempty"`
	// Action, when non-empty, is the raw inject-registry action
	// string (e.g. "signal(target=pg_random, sig=15)").  Wins over
	// the convenience fields (kind / target / signal) — operators
	// who need a primitive that isn't in the convenience-mapping
	// switch can drop down to the raw form.
	Action string `yaml:"action,omitempty"`
	// HealWindow lets an `inject` step block briefly after the
	// fault Apply returns, before the recovery callback runs and
	// the next step starts.  Mirrors the validate orchestrator's
	// soak-time HealWindow.  Empty defaults to the inject step's
	// default heal window (currently 0; future: a per-fault
	// recommended pause).
	HealWindow string `yaml:"heal_window,omitempty"`

	// restore
	To           string `yaml:"to,omitempty"`
	ToCheckpoint string `yaml:"to_checkpoint,omitempty"`
	Source       string `yaml:"source,omitempty"`
	// ToLSN is the PITR target LSN.  Either a literal LSN
	// ("0/3000050") or a $name reference to a previously
	// captured LSN (e.g. "$post_load" — the runner resolves
	// that to whatever value `capture_lsn: { name: post_load }`
	// stored in scenario state).  Maps to `pg_hardstorage
	// restore --to-lsn` on the CLI.
	ToLSN string `yaml:"to_lsn,omitempty"`

	// TablespaceMappings forwards `--tablespace-mapping=<src>=<dst>`
	// to `pg_hardstorage restore`.  Each entry is the literal
	// argument format the CLI accepts (`<source-loc>=<target-loc>`).
	// Repeatable on the CLI; each entry in this slice becomes one
	// repetition.  Resolution of $-placeholders applies (so a
	// scenario can use $ARTEFACT_DIR to land remapped tablespaces
	// inside its own per-scenario temp tree).
	TablespaceMappings []string `yaml:"tablespace_mappings,omitempty"`

	// capture_lsn — Name is the scenario-state key under
	// which the captured LSN is stored, referenceable as
	// "$Name" in subsequent restore steps' to_lsn.
	Name string `yaml:"name,omitempty"`

	// capture_lsn — optional checksum query run against the
	// live cluster at capture time.  The same SQL re-runs
	// against the restored cluster in
	// `assert_restored_match`, and the two results must be
	// byte-equal for the scenario to pass.  Must return a
	// single TEXT column in a single row (e.g.
	// "select count(*) || '|' || sum(abalance)::text from
	//  pgbench_accounts").
	ChecksumQuery string `yaml:"checksum_query,omitempty"`

	// assert_restored_match — when Strict is true, the step
	// hard-fails when host pg_ctl isn't found (and the
	// verification therefore can't run).  Default false:
	// the step soft-skips, which keeps minimal CI hosts
	// happy but creates the exact gap that let issue #7
	// ship.  CI scenarios that MUST verify byte-equality
	// flip Strict to true.
	Strict bool `yaml:"strict,omitempty"`

	// assert_restored_match — ReadyTimeout caps how long to wait
	// for the restored sandbox cluster to finish WAL replay and
	// accept connections. A duration string ("180s", "25m");
	// empty defaults to 180s. A streaming scenario that replays
	// tens of GiB of post-backup WAL (e.g. a VACUUM FULL) needs
	// far more — recovery is single-threaded.
	ReadyTimeout string `yaml:"ready_timeout,omitempty"`

	// restored_load — the "does PG actually start and serve
	// traffic" gate.  Boots the restored datadir in a
	// postgres:<version> sandbox and runs pgbench against
	// it.  Reuses Name (link to capture_lsn / restore step)
	// and Duration (load-test wall time, default 30s).
	//
	// PgbenchClients defaults to 4.  PgbenchTPSFloor (default
	// 0 = any TPS) refuses if pgbench reports below the
	// floor — pin a non-zero value to catch silent
	// degradation.  PgbenchErrorTolerance (default 0)
	// refuses if any transaction fails; bump if the workload
	// is expected to produce duplicate-key etc.
	PgbenchClients        int     `yaml:"pgbench_clients,omitempty"`
	PgbenchTPSFloor       float64 `yaml:"pgbench_tps_floor,omitempty"`
	PgbenchErrorTolerance int     `yaml:"pgbench_error_tolerance,omitempty"`

	// corrupt_repo_object — surgical mutation of stored repo
	// state followed by an assertion that the next operation
	// detects the corruption.  RepoTarget selects what to
	// mutate; Mutation picks how.  Offset is the byte
	// position for flip_bit_at; ignored for truncate /
	// overwrite_zeros.  ExpectErrorPrefix is the structured-
	// error code the next operation MUST surface for the
	// step to pass; empty means "best effort, just mutate".
	RepoTarget        string `yaml:"repo_target,omitempty"`
	Mutation          string `yaml:"mutation,omitempty"`
	Offset            int64  `yaml:"offset,omitempty"`
	ExpectErrorPrefix string `yaml:"expect_error_prefix,omitempty"`

	// compat_archive — exercises the legacy-tool compat shims
	// (pg-hardstorage-pgbackrest / pg-hardstorage-barman /
	// pg-hardstorage-barman-wal-archive) end-to-end.  The step
	// generates a synthetic 16 MiB WAL segment + matching
	// `.backup` companion, runs the named shim's archive-push
	// inside a target OS container, asserts the manifests
	// landed in the repo, then round-trips via archive-get
	// (pgbackrest) or native `wal fetch` (barman).
	//
	// Shim is one of: "pgbackrest", "barman", "barman-wal-archive".
	// OSImage is a docker image that runs the shim — pgbackrest
	// + barman shims are static Go binaries so any Linux base
	// works (debian:12, ubuntu:24.04, rockylinux:9, opensuse/leap:15).
	// Empty OSImage runs on the host.
	//
	// Deployment is the stanza / server name the shim sees.
	// Fixture selects the synthetic input:
	//   - "segment"             — 16 MiB WAL segment only
	//   - "segment_plus_backup" — segment + .backup companion (issue #10)
	//   - "history"             — timeline-history file
	//
	// CompatSink (yaml: `sink:`) selects an alternate storage
	// backend: a sink runtime kind (s3-minio, tls-minio, ...).
	// When non-empty, the step boots that runtime, takes its
	// URL as the repo target, plumbs AWS_CA_BUNDLE / cert
	// trust into the in-container shim, and tears the runtime
	// down at step exit.  Empty falls back to the legacy
	// file:// path under artefactDir/compat-repo.
	Shim       string `yaml:"shim,omitempty"`
	OSImage    string `yaml:"os_image,omitempty"`
	Fixture    string `yaml:"fixture,omitempty"`
	CompatSink string `yaml:"sink,omitempty"`

	// drop_slot — optional explicit slot name to drop.  Empty
	// defaults to the deployment's canonical slot name
	// (`pg_hardstorage_<deployment>` with hyphens
	// underscored, matching the wal stream CLI).
	Slot string `yaml:"slot,omitempty"`

	// cli_run — shells out to the pg_hardstorage binary with
	// the supplied args and asserts on exit code + output.
	// The escape hatch for end-to-end testing CLI surfaces
	// the runner doesn't have a dedicated step for: hold
	// add/list/remove, kms rotate/shred/verify, audit
	// verify-chain / anchor, recovery readiness / drill,
	// verify, classify, anomaly, etc.
	//
	// Args is the argv after the binary name.  Tokens may
	// reference scenario state via these placeholders:
	//
	//   $REPO          — the resolved repo URL (sink-driven
	//                    or file:// fallback).  Same string
	//                    take_backup uses.
	//   $DEPLOYMENT    — the scenario's primary deployment
	//                    name (auto-resolved from the first
	//                    Deployment-bearing step).
	//   $LAST_BACKUP   — the most recent take_backup's
	//                    backup ID; empty before any
	//                    take_backup runs (cli_run errors).
	//   $ARTEFACT_DIR  — the scenario's artefact directory.
	//   $AGENT_BIN     — the resolved pg_hardstorage path.
	//
	// ExpectExit defaults to 0; set to a non-nil int (use
	// pointer semantic via raw YAML int — present means set)
	// to require a specific non-zero exit for negative-path
	// tests like "delete must fail because hold is active".
	// Empty / absent in YAML means "must exit 0".
	//
	// ExpectStdoutContains / ExpectStderrContains are
	// substring matches (not regex — keep the schema simple).
	// Multiple substrings would be a future extension.
	//
	// Timeout defaults to 60s if empty; parse error fails
	// the step with a clear pointer at the field.
	Args                 []string `yaml:"args,omitempty"`
	ExpectExit           *int     `yaml:"expect_exit,omitempty"`
	ExpectStdoutContains string   `yaml:"expect_stdout_contains,omitempty"`
	ExpectStderrContains string   `yaml:"expect_stderr_contains,omitempty"`
	Timeout              string   `yaml:"timeout,omitempty"`

	// Env carries extra environment variables to set on the
	// cli_run child process — primarily for compat-shim
	// scenarios that need PGPASSWORD (the pgBackRest, WAL-G,
	// and Barman shims all use libpq's environment-based
	// credential resolution, not a --password flag).  Values
	// honour the same $-placeholder set as args.
	Env map[string]string `yaml:"env,omitempty"`

	// L4 upgrade/compat scenarios — fields shared by
	// swap_binary, synthesize_manifest, write_repo_marker,
	// os_pkg_upgrade, pg_upgrade, swap_pg_minor.

	// HostPath: source path on the host (e.g. "bin/pg_hardstorage").
	HostPath string `yaml:"host_path,omitempty"`

	// CellPath: destination path INSIDE the target container
	// (e.g. "/usr/local/bin/pg_hardstorage").
	CellPath string `yaml:"cell_path,omitempty"`

	// Container: override the auto-resolved container name
	// (default: scenario's primary PG cell).  Empty = primary.
	Container string `yaml:"container,omitempty"`

	// RepoMarkerVersion: for write_repo_marker — the version
	// string written into _repo_version.json.  Picks the
	// "format" the marker advertises (e.g. "v1.0", "v1.1").
	RepoMarkerVersion string `yaml:"repo_marker_version,omitempty"`

	// ManifestSchemaVersion: for synthesize_manifest — the
	// schema_version the synthetic manifest claims (e.g.
	// "v0.8", "v0.9", or "v0.7-corrupt-no-version").
	ManifestSchemaVersion string `yaml:"manifest_schema_version,omitempty"`

	// ManifestRelPath: for synthesize_manifest — the relative
	// path inside the repo where the synthetic manifest lands
	// (default "manifests/synthetic-<schema>.json").
	ManifestRelPath string `yaml:"manifest_rel_path,omitempty"`

	// PkgManager: for os_pkg_upgrade — "apt" | "dnf" | "zypper"
	// | "pacman" | "auto" (auto-detect from /etc/os-release).
	PkgManager string `yaml:"pkg_manager,omitempty"`

	// Packages: for os_pkg_upgrade — explicit package list.
	// Empty = upgrade glibc/openssl/systemd (the canonical
	// "soak under OS bump" set).
	Packages []string `yaml:"packages,omitempty"`

	// PgFromVersion / PgToVersion: for pg_upgrade — source
	// and target PG majors.  Cell must have BOTH installed
	// (testbed image with pg_upgrade-capable layout).
	PgFromVersion string `yaml:"pg_from_version,omitempty"`
	PgToVersion   string `yaml:"pg_to_version,omitempty"`

	// PgSuperuser: the PG superuser pg_upgrade authenticates
	// as.  Defaults to "postgres" — the official image's
	// initdb default.  Set explicitly for testbed images that
	// pin a different superuser via POSTGRES_USER (e.g. the
	// scenario-runner local-docker provider sets it to
	// "testkit", which means pg_upgrade -U postgres errors
	// with `role "postgres" does not exist`).
	PgSuperuser string `yaml:"pg_superuser,omitempty"`

	// PgMinorPackage: for swap_pg_minor — the new PG package
	// version to install (e.g. "postgresql-17=17.6-1.pgdg22.04+1").
	// Format is package-manager-specific.
	PgMinorPackage string `yaml:"pg_minor_package,omitempty"`

	// Statement: for the `sql` step — a raw SQL statement run
	// against the current primary, in autocommit.  The escape
	// hatch for a DDL/DML the load-file op vocabulary doesn't
	// cover (VACUUM FULL, an ad-hoc CREATE TABLE) at a precise
	// point BETWEEN steps — unlike the scenario `load:` file,
	// which is force-applied once at scenario start.
	Statement string `yaml:"statement,omitempty"`

	// assert / assert_matches_checkpoint
	AssertsRaw yaml.Node          `yaml:"-"`
	Asserts    []assert.Assertion `yaml:"-"`
}

// UnmarshalYAML decodes the single-key-map step shape:
//
//   - take_backup: { deployment: db1, type: full }
//   - run_load:    { duration: 10m }
//   - inject:      { kind: agent_kill, signal: 9 }
//   - assert:      [ { count_exact: ... } ]
func (s *Step) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode || len(node.Content) != 2 {
		return fmt.Errorf("step: expected single-key map, got %d entries", len(node.Content)/2)
	}
	key := node.Content[0].Value
	val := node.Content[1]
	s.Kind = key

	// `assert:` takes a sequence; everything else takes a mapping.
	if key == "assert" || key == "assert_matches_checkpoint" {
		if val.Kind != yaml.SequenceNode {
			// assert_matches_checkpoint can be a scalar mapping too;
			// for assert we want the sequence form.
			if val.Kind == yaml.MappingNode {
				type stepPayload Step
				var p stepPayload
				if err := val.Decode(&p); err != nil {
					return err
				}
				p.Kind = key
				*s = Step(p)
				return nil
			}
			return fmt.Errorf("step %s: expected sequence, got %v", key, val.Kind)
		}
		list, err := assert.ParseList(val)
		if err != nil {
			return err
		}
		s.Asserts = list
		return nil
	}

	if val.Kind == yaml.ScalarNode && val.Value == "" {
		return nil
	}
	if val.Kind != yaml.MappingNode {
		return fmt.Errorf("step %s: expected mapping value, got %v", key, val.Kind)
	}

	type stepPayload Step
	var p stepPayload
	if err := val.Decode(&p); err != nil {
		return fmt.Errorf("step %s: %w", key, err)
	}
	p.Kind = key
	*s = Step(p)
	return nil
}

// Cleanup describes what to do when the scenario finishes.
type Cleanup struct {
	OnSuccess string `yaml:"on_success,omitempty"` // tear_down | keep
	OnFailure string `yaml:"on_failure,omitempty"` // tear_down | keep_for: <duration>
}

// FromFile reads + parses a scenario file.
func FromFile(path string) (*Scenario, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("scenario: read %s: %w", path, err)
	}
	return Parse(body)
}

// Parse decodes YAML bytes.
func Parse(body []byte) (*Scenario, error) {
	var s Scenario
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("scenario: parse: %w", err)
	}
	if s.Schema != SchemaScenario {
		return nil, fmt.Errorf("scenario: schema %q is not supported; want %q", s.Schema, SchemaScenario)
	}
	if s.Name == "" {
		return nil, fmt.Errorf("scenario: name is required")
	}
	if s.Topology.Provider == "" {
		return nil, fmt.Errorf("scenario: topology.provider is required")
	}
	if len(s.Steps) == 0 {
		return nil, fmt.Errorf("scenario: at least one step is required")
	}
	if list, err := assert.ParseList(&s.AssertsRaw); err != nil {
		return nil, err
	} else {
		s.Asserts = list
	}
	return &s, nil
}
