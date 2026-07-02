package translate

import (
	"os"
	"strings"
	"testing"
)

func TestParse_SampleConf(t *testing.T) {
	f, err := os.Open("testdata/sample-pgbackrest.conf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := Parse(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := len(cfg.Sections), 3; got != want {
		t.Fatalf("section count: got %d want %d", got, want)
	}
	if cfg.Sections[0].Name != "global" {
		t.Fatalf("first section: got %q want global", cfg.Sections[0].Name)
	}
	if cfg.Sections[1].KV["pg1-host"] != "db1.example.com" {
		t.Fatalf("db1 pg1-host: got %q", cfg.Sections[1].KV["pg1-host"])
	}
}

func TestTranslate_RoundTrip(t *testing.T) {
	f, err := os.Open("testdata/sample-pgbackrest.conf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := Parse(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res, err := Translate(cfg)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	must := []string{
		"deployments:",
		// Deployments are emitted as a mapping keyed by name (config.Load
		// decodes `deployments:` as map[string]DeploymentConfig with
		// KnownFields(true)), not a `- name:` sequence item.
		"  db1:",
		"  db2:",
		`pg_connection: "postgres://pgbackup@db1.example.com:5432/postgres"`,
		`repo: "file:///var/lib/pgbackrest"`,
		`repo: "s3://acme-pg-backups/db2-prefix"`,
		"keep_full_count: 4",
		"keep_full_count: 2",
		"AES-256-GCM",
		"compress-type=lz4 ignored",
	}
	for _, s := range must {
		if !strings.Contains(res.YAML, s) {
			t.Errorf("YAML missing %q\n--- got ---\n%s", s, res.YAML)
		}
	}

	// Unmapped + warnings
	if len(res.Warnings) == 0 {
		t.Errorf("expected at least one warning")
	}
	gotUnmapped := strings.Join(res.Unmapped, "\n")
	for _, want := range []string{"log-level-console", "process-max", "backup-standby"} {
		if !strings.Contains(gotUnmapped, want) {
			t.Errorf("unmapped list missing %q\n--- got ---\n%s", want, gotUnmapped)
		}
	}
}

func TestTranslate_MalformedINI(t *testing.T) {
	r := strings.NewReader("orphan-key=val\n[s]\n")
	if _, err := Parse(r); err == nil {
		t.Fatalf("expected error for kv before section")
	}
}

func TestTranslate_EmptyConfig(t *testing.T) {
	r := strings.NewReader("# no sections\n")
	cfg, err := Parse(r)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res, err := Translate(cfg)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(res.YAML, "deployments:") {
		t.Errorf("YAML must still emit a deployments: header")
	}
}
