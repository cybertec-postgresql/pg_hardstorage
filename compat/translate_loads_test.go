// translate_loads_test.go — every translator's output must LOAD.
//
// The three `compat translate` translators each render a
// pg_hardstorage.yaml from a legacy config. internal/config decodes
// with KnownFields(true), so a single invented key makes the whole
// translated file unloadable — and the failure lands on the operator
// at migration time, after they have already piped the output into
// /etc/pg_hardstorage/pg_hardstorage.yaml.
//
// Three separate rounds of that shipped: the pgbackrest translator
// emitted `keep_full_count` and an `encryption:` block, the barman
// translator emitted `slot:` / `parallelism:` / `compression:` /
// `backup:`, and the WAL-G translator emitted
// `encryption.{kek_ref,passphrase_env}` — none of which exist in
// config.DeploymentConfig. Each was found by hand, downstream.
//
// This test closes that loop: it feeds each translator a fixture
// exercising its full mapping surface and pushes the rendered YAML
// through the real loader. Adding a mapping that emits a key the
// schema does not have is now a test failure here, not a migration
// surprise.
package compat_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	barmantranslate "github.com/cybertec-postgresql/pg_hardstorage/compat/barman/translate"
	pgbackresttranslate "github.com/cybertec-postgresql/pg_hardstorage/compat/pgbackrest/translate"
	walgtranslate "github.com/cybertec-postgresql/pg_hardstorage/compat/walg/translate"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// loadYAML runs one rendered document through the production loader,
// exactly as `pg_hardstorage lint` and every command do.
func loadYAML(t *testing.T, yaml string) *config.LoadResult {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
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
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("translated YAML does not load — the translator emitted a key the schema does not have:\n%v\n\n--- rendered ---\n%s", err, yaml)
	}
	return res
}

// emitsKey reports whether the rendered YAML contains an actual
// mapping key `name:` at any indent — ignoring comment lines, so a
// note that merely mentions the key does not count.
func emitsKey(yaml, name string) bool {
	for _, line := range strings.Split(yaml, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == name+":" || strings.HasPrefix(trimmed, name+": ") {
			return true
		}
	}
	return false
}

// pgbackrestFixture exercises every mapped pgBackRest key, including
// the retention and cipher paths that produced the invalid
// `keep_full_count` / `encryption:` output.
const pgbackrestFixture = `
[global]
repo1-path=/srv/backups
repo1-type=file
repo1-cipher-type=aes-256-cbc
repo1-cipher-pass=hunter2
compress-type=lz4
retention-full=4
log-level-console=info
process-max=4

[db1]
pg1-host=10.0.0.10
pg1-port=5432
pg1-user=replicator
pg1-database=postgres

[db2]
pg1-host=10.0.0.11
pg1-user=replicator
`

func TestPgbackrestTranslateOutputLoads(t *testing.T) {
	cfg, err := pgbackresttranslate.Parse(strings.NewReader(pgbackrestFixture))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	res, err := pgbackresttranslate.Translate(cfg)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	loaded := loadYAML(t, res.YAML)

	for _, name := range []string{"db1", "db2"} {
		d, ok := loaded.Config.Deployments[name]
		if !ok {
			t.Fatalf("deployment %q missing from loaded config", name)
		}
		if d.PGConnection == "" {
			t.Errorf("%s: pg_connection did not survive the round trip", name)
		}
		if d.Repo == "" {
			t.Errorf("%s: repo did not survive the round trip", name)
		}
	}

	// retention-full=4 must arrive as keep_fulls, not be silently
	// dropped into the loader's default of 14.
	if got := loaded.Config.Deployments["db1"].Retention.KeepFulls; got != 4 {
		t.Errorf("retention-full=4 → keep_fulls: got %d, want 4", got)
	}
	if got := loaded.Config.Deployments["db1"].Retention.Policy; got != "count" {
		t.Errorf("retention policy: got %q, want %q", got, "count")
	}

	// repo1-type=file is pgBackRest's own spelling for a local repo;
	// it must map, not be reported as unsupported.
	if got := loaded.Config.Deployments["db1"].Repo; !strings.HasPrefix(got, "file://") {
		t.Errorf("repo1-type=file → repo: got %q, want a file:// URL", got)
	}
	for _, u := range res.Unmapped {
		if strings.Contains(u, "repo1-type") && strings.Contains(u, "unsupported") {
			t.Errorf("repo1-type=file reported unsupported, but the product supports file:// repos: %q", u)
		}
	}

	// The cipher path must set the flat kek_ref, never an
	// `encryption:` block.
	if got := loaded.Config.Deployments["db1"].KEKRef; got == "" {
		t.Error("repo1-cipher-type=aes-256-cbc did not produce a kek_ref")
	}
	if emitsKey(res.YAML, "encryption") {
		t.Error("translator emitted an `encryption:` block; DeploymentConfig has a flat kek_ref and no encryption key")
	}
}

// TestPgbackrestTranslateFlagsUntranslatableStanza covers the
// single-node `pg1-path` shape: it cannot yield a connection, and the
// resulting deployment is inoperable. That has to be visible in the
// rendered file, not only on stderr — stdout is the mode operators
// pipe.
func TestPgbackrestTranslateFlagsUntranslatableStanza(t *testing.T) {
	const fixture = `
[global]
repo1-path=/srv/backups

[db1]
pg1-path=/var/lib/pgsql/data
`
	cfg, err := pgbackresttranslate.Parse(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	res, err := pgbackresttranslate.Translate(cfg)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(res.YAML, "UNTRANSLATED") {
		t.Errorf("a stanza with no derivable pg_connection rendered silently:\n%s", res.YAML)
	}
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "UNTRANSLATED") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning for the untranslatable stanza; warnings=%q", res.Warnings)
	}
	// It must still load — the operator needs `lint` to reach the
	// deployment-level check rather than dying on a parse error.
	loadYAML(t, res.YAML)
}

// barmanFixture exercises the keys that rendered `slot:`,
// `parallelism:`, `compression:` and `backup:` — none of which are
// DeploymentConfig fields.
const barmanFixture = `
[barman]
barman_user = barman
log_file = /var/log/barman/barman.log

[db1]
description = "primary"
conninfo = host=10.0.0.10 user=barman dbname=postgres
backup_directory = /srv/barman/db1
retention_policy = RECOVERY WINDOW OF 7 DAYS
minimum_redundancy = 2
compression = gzip
slot_name = barman_db1
create_slot = auto
parallel_jobs = 4
immediate_checkpoint = true
check_timeout = 30
retry_times = 3
`

func TestBarmanTranslateOutputLoads(t *testing.T) {
	res, err := barmantranslate.Translate(strings.NewReader(barmanFixture))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	loaded := loadYAML(t, res.YAML)

	d, ok := loaded.Config.Deployments["db1"]
	if !ok {
		t.Fatal("deployment db1 missing from loaded config")
	}
	if d.PGConnection == "" {
		t.Error("conninfo did not survive the round trip")
	}
	if d.Repo == "" {
		t.Error("backup_directory did not survive the round trip")
	}
	if d.Retention.KeepFor == "" {
		t.Error("retention_policy did not survive the round trip")
	}

	// None of these are schema keys; emitting any of them makes the
	// file unloadable.
	for _, bad := range []string{"slot", "parallelism", "compression", "backup", "encryption"} {
		if emitsKey(res.YAML, bad) {
			t.Errorf("translator emitted %q, which is not a DeploymentConfig field", bad+":")
		}
	}
}

// walgFixture uses the dotted FQDN that production WAL-G installs
// almost always carry in PGHOST, plus the encryption env that drove
// the invalid `encryption:` block.
const walgFixture = `
PGHOST=db.prod.internal
PGPORT=5432
PGUSER=postgres
PGDATABASE=postgres
WALG_GS_PREFIX=gs://backups/prod
WALG_LIBSODIUM_KEY=0123456789abcdef
WALG_COMPRESSION_METHOD=brotli
`

func TestWalgTranslateOutputLoads(t *testing.T) {
	env, err := walgtranslate.Parse(strings.NewReader(walgFixture))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	res, err := walgtranslate.Translate(env)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	loaded := loadYAML(t, res.YAML)

	// The name must have been sanitized — "db.prod.internal" is not a
	// legal deployment name, and emitting it verbatim is what made the
	// loader reject the file.
	if _, bad := loaded.Config.Deployments["db.prod.internal"]; bad {
		t.Fatal("dotted FQDN was emitted verbatim as a deployment name")
	}
	if len(loaded.Config.Deployments) != 1 {
		t.Fatalf("want exactly one deployment, got %d", len(loaded.Config.Deployments))
	}
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "PG_HARDSTORAGE_DEPLOYMENT") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("the derived/sanitized deployment name was not explained; warnings=%q", res.Warnings)
	}

	if emitsKey(res.YAML, "encryption") {
		t.Error("translator emitted an `encryption:` block; DeploymentConfig has a flat kek_ref")
	}
	for name, d := range loaded.Config.Deployments {
		if d.KEKRef == "" {
			t.Errorf("%s: WALG_LIBSODIUM_KEY did not produce a kek_ref", name)
		}
		if d.Repo == "" {
			t.Errorf("%s: WALG_GS_PREFIX did not produce a repo", name)
		}
	}
}
