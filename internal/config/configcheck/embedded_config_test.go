package configcheck_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config/configcheck"
)

func TestScrub_FindsInventedKeysInsideEmbeddedConfig(t *testing.T) {
	// The exact shape the sidecar chart shipped: a Helm values file
	// whose `config:` literal carries the pg_hardstorage config.
	body := "```yaml\n" +
		"# my-values.yaml\n" +
		"config: |\n" +
		"  deployments:\n" +
		"    db1:\n" +
		"      repo: s3://acme\n" +
		"      schedule:\n" +
		"        full:        \"0 2 * * 0\"\n" +
		"        incremental: \"0 2 * * 1-6\"\n" +
		"\n" +
		"persistence:\n" +
		"  enabled: true\n" +
		"```"
	findings := configcheck.Scrub(body)
	if len(findings) == 0 {
		t.Fatal("invented keys inside an embedded config: block went undetected")
	}
	var keys []string
	for _, f := range findings {
		keys = append(keys, f.Key)
	}
	got := strings.Join(keys, ",")
	if !strings.Contains(got, "full") || !strings.Contains(got, "incremental") {
		t.Errorf("flagged %q, want both full and incremental", got)
	}
}

func TestScrub_IgnoresNonConfigLiteralBlocks(t *testing.T) {
	// A shell script under `command: |` must not be mistaken for config.
	body := "```yaml\n" +
		"command: |\n" +
		"  set -e\n" +
		"  deployments_are_not_here=1\n" +
		"```"
	if f := configcheck.Scrub(body); len(f) != 0 {
		t.Errorf("non-config literal block flagged: %+v", f)
	}
}
