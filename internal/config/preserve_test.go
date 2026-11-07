package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const annotated = `# Production backups. Owner: dba@example.org
schema: ` + Schema + `

deployments:
  # primary cluster, do not touch without a change ticket
  db1:
    pg_connection: "postgres://backup@db1/postgres"   # via pgbouncer
    repo: s3://bucket/db1
    retention:
      keep_monthly: 60   # legal: 5 years
      keep_daily: 30
  # reporting replica
  db2:
    pg_connection: postgres://backup@db2/postgres
    repo: s3://bucket/db2
`

func mustLoad(t *testing.T, src string) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// An edit must leave the operator's comments, key order, quoting and
// indentation alone, and still write exactly the edited config.
func TestMarshalPreserving_KeepsCommentsAcrossAnEdit(t *testing.T) {
	cfg := mustLoad(t, annotated)
	d := cfg.Deployments["db1"]
	d.Retention.KeepDaily = Keep(14)
	cfg.Deployments["db1"] = d
	cfg.Deployments["db3"] = DeploymentConfig{PGConnection: "postgres://backup@db3/postgres", Repo: "s3://bucket/db3"}
	delete(cfg.Deployments, "db2")

	out, err := MarshalPreserving(cfg, []byte(annotated))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"# Production backups. Owner: dba@example.org",
		"# primary cluster, do not touch without a change ticket",
		`pg_connection: "postgres://backup@db1/postgres" # via pgbouncer`,
		"keep_monthly: 60 # legal: 5 years",
		"      keep_daily: 14\n", // 2-space indentation kept, value updated
		"  db3:\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"db2", "reporting replica", "keep_daily: 30"} {
		if strings.Contains(got, gone) {
			t.Errorf("output still has %q:\n%s", gone, got)
		}
	}
	// keep_monthly stays ahead of keep_daily, as the operator wrote it.
	if strings.Index(got, "keep_monthly") > strings.Index(got, "keep_daily") {
		t.Errorf("key order was rewritten:\n%s", got)
	}
	if !yamlEqual(*mustLoad(t, got), *cfg) {
		t.Fatalf("written file does not decode to the edited config:\n%s", got)
	}
}

// A no-op write of an annotated file changes nothing semantically and
// loses no comment.
func TestMarshalPreserving_NoOpKeepsEveryComment(t *testing.T) {
	cfg := mustLoad(t, annotated)
	out, err := MarshalPreserving(cfg, []byte(annotated))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(annotated, "\n") {
		if i := strings.Index(line, "#"); i >= 0 && !strings.Contains(string(out), strings.TrimSpace(line[i:])) {
			t.Errorf("comment %q lost:\n%s", line[i:], out)
		}
	}
}

// Removing a list element must not slide the neighbours' comments onto
// the wrong items.
func TestMarshalPreserving_SequenceCommentsFollowTheirItems(t *testing.T) {
	const src = "schema: " + Schema + "\ndeployments:\n  db1:\n    repo: file:///r\n    residency:\n      - eu # frankfurt\n      - us # ashburn\n      - ap # tokyo\n"
	cfg := mustLoad(t, src)
	d := cfg.Deployments["db1"]
	d.Residency = []string{"us", "ap"}
	cfg.Deployments["db1"] = d
	out, err := MarshalPreserving(cfg, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "- us # ashburn") || !strings.Contains(got, "- ap # tokyo") || strings.Contains(got, "frankfurt") {
		t.Fatalf("sequence comments misplaced:\n%s", got)
	}
}

// Anchors can make the merged tree say something other than the config
// (an alias would follow its edited anchor). The written file must
// decode to the config regardless — comments are the lesser loss.
func TestMarshalPreserving_AliasNeverChangesTheMeaning(t *testing.T) {
	const src = "schema: " + Schema + "\ndeployments:\n  db1: &base\n    repo: file:///r1\n    tenant: a\n  db2: *base\n"
	cfg := mustLoad(t, src)
	d := cfg.Deployments["db1"]
	d.Tenant = "changed"
	cfg.Deployments["db1"] = d
	out, err := MarshalPreserving(cfg, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := mustLoad(t, string(out))
	if !yamlEqual(*got, *cfg) {
		t.Fatalf("file decodes to %+v, want %+v\n%s", got.Deployments, cfg.Deployments, out)
	}
	if got.Deployments["db2"].Tenant != "a" {
		t.Fatalf("editing db1 leaked into db2 through the alias:\n%s", out)
	}
}

// New and unparseable files get the canonical layout with its header.
func TestMarshalPreserving_FallsBackToMarshal(t *testing.T) {
	cfg := mustLoad(t, annotated)
	plain, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for name, existing := range map[string]string{"new file": "", "blank": " \n", "not yaml": "a: [unclosed", "not a mapping": "- 1\n"} {
		out, err := MarshalPreserving(cfg, []byte(existing))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(out) != string(plain) {
			t.Errorf("%s: got\n%s\nwant the plain marshal", name, out)
		}
	}
	// The header must not promise more than the writer does.
	if strings.Contains(string(plain), "Hand edits are preserved") {
		t.Error("header still claims hand edits are preserved wholesale")
	}
}
