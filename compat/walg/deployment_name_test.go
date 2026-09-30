package walg

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/compat/walg/translate"
)

// The runtime shim and `compat translate --from walg` must derive the
// SAME deployment name from the same environment: the translator
// writes pg_hardstorage.yaml, the shim then addresses the repo by the
// name it derives. When they diverged (db.prod.internal at runtime vs
// db_prod_internal in the YAML), config and repo split; a Unix-socket
// PGHOST produced an empty runtime name and archive_command failed on
// every segment.
func TestDeploymentName_MatchesTranslator(t *testing.T) {
	for _, env := range []map[string]string{
		{"PGHOST": "/var/run/postgresql"},
		{"PGHOST": "/tmp"},
		{"PGHOST": "db.prod.internal"},
		{"PGHOST": "db.prod.internal:5432"},
		{"PGHOST": "10.0.0.5"},
		{"PGHOST": "db1"},
		{},
		{"PG_HARDSTORAGE_DEPLOYMENT": "prod-db", "PGHOST": "db.example.com"},
		{"PG_HARDSTORAGE_DEPLOYMENT": "prod.db"},
	} {
		e := walgEnv{pgHost: env["PGHOST"], deployment: env["PG_HARDSTORAGE_DEPLOYMENT"]}
		runtime := e.deploymentName()
		if runtime == "" {
			t.Errorf("%v: empty runtime deployment name", env)
			continue
		}
		var src strings.Builder
		for k, v := range env {
			src.WriteString(k + "=" + v + "\n")
		}
		src.WriteString("WALG_FILE_PREFIX=/srv/wal\n")
		parsed, err := translate.Parse(strings.NewReader(src.String()))
		if err != nil {
			t.Fatal(err)
		}
		res, err := translate.Translate(parsed)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.YAML, "\n  "+runtime+":\n") {
			t.Errorf("%v: runtime name %q not the translator's deployment key:\n%s", env, runtime, res.YAML)
		}
	}
}
