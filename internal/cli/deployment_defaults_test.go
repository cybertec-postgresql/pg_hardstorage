package cli

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

func TestResolveDeploymentDefaults(t *testing.T) {
	deps := map[string]config.DeploymentConfig{
		"mytest": {PGConnection: "postgres://cfg@host/db", Repo: "file:///cfg/repo"},
		"norepo": {PGConnection: "postgres://cfg@host/db"},
		"empty":  {},
	}

	cases := []struct {
		name             string
		deployment       string
		pgIn, repoIn     string
		wantPG, wantRepo string
	}{
		{"fills both from config", "mytest", "", "", "postgres://cfg@host/db", "file:///cfg/repo"},
		{"explicit pg wins, repo filled", "mytest", "postgres://flag@h/d", "", "postgres://flag@h/d", "file:///cfg/repo"},
		{"explicit repo wins, pg filled", "mytest", "", "file:///flag/repo", "postgres://cfg@host/db", "file:///flag/repo"},
		{"both explicit unchanged", "mytest", "postgres://flag@h/d", "file:///flag/repo", "postgres://flag@h/d", "file:///flag/repo"},
		{"config missing repo field", "norepo", "", "", "postgres://cfg@host/db", ""},
		{"config empty fields", "empty", "", "", "", ""},
		{"unknown deployment unchanged", "ghost", "", "", "", ""},
		{"empty deployment name unchanged", "", "", "file:///flag/repo", "", "file:///flag/repo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPG, gotRepo := resolveDeploymentDefaults(tc.deployment, tc.pgIn, tc.repoIn, deps)
			if gotPG != tc.wantPG {
				t.Errorf("pgConn = %q, want %q", gotPG, tc.wantPG)
			}
			if gotRepo != tc.wantRepo {
				t.Errorf("repoURL = %q, want %q", gotRepo, tc.wantRepo)
			}
		})
	}
}

func TestResolveDeploymentDefaults_NilDeps(t *testing.T) {
	pg, repo := resolveDeploymentDefaults("mytest", "", "", nil)
	if pg != "" || repo != "" {
		t.Errorf("nil deps must leave inputs untouched; got pg=%q repo=%q", pg, repo)
	}
}
