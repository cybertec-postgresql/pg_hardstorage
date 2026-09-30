package compat_test

import (
	"strings"
	"testing"

	barmantranslate "github.com/cybertec-postgresql/pg_hardstorage/compat/barman/translate"
)

// A translated barman retention_policy must name its policy. The agent
// reads an empty retention.policy as GFS with the default 7/4/12/5
// buckets and ignores keep_for / keep_fulls, so "RECOVERY WINDOW OF
// 30 DAYS" and "REDUNDANCY 4" both silently became GFS.
func TestBarmanRetentionTranslatesWithPolicy(t *testing.T) {
	for _, tc := range []struct {
		in, policy, field string
	}{
		{"RECOVERY WINDOW OF 30 DAYS", "simple", "720h"},
		{"REDUNDANCY 4", "count", "4"},
	} {
		conf := "[db1]\nconninfo = host=db1 user=barman dbname=postgres\n" +
			"backup_directory = /var/lib/barman/db1\nretention_policy = " + tc.in + "\n"
		res, err := barmantranslate.Translate(strings.NewReader(conf))
		if err != nil {
			t.Fatal(err)
		}
		loaded := loadYAML(t, res.YAML)
		dep, ok := loaded.Config.Deployments["db1"]
		if !ok {
			t.Fatalf("%s: deployment missing:\n%s", tc.in, res.YAML)
		}
		r := dep.Retention
		if r.Policy != tc.policy {
			t.Fatalf("%s: retention.policy = %q, want %q (empty means default GFS):\n%s",
				tc.in, r.Policy, tc.policy, res.YAML)
		}
		if tc.policy == "simple" && r.KeepFor != tc.field {
			t.Errorf("%s: keep_for = %q, want %q", tc.in, r.KeepFor, tc.field)
		}
		if tc.policy == "count" && r.KeepFulls != 4 {
			t.Errorf("%s: keep_fulls = %d, want 4", tc.in, r.KeepFulls)
		}
	}
}
