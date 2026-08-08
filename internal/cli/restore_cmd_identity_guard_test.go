package cli

// restore_cmd_identity_guard_test.go — every restore_command
// generation site must arm the cluster-identity check.
//
// Source-level, in the TestStreamLoopUsesTheDecisions tradition: the
// unarmed builders (Build / BuildStandby) still exist as compatibility
// wrappers, so nothing stops a future call site — or a regressed
// existing one — from quietly generating an identity-blind
// restore_command. This guard enumerates the generator sites and
// requires the *WithIdentity spelling at each; a new site that greps
// in here must either arm the check or add itself to the exemption
// with a written reason.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryRestoreCommandSiteArmsTheIdentityCheck(t *testing.T) {
	root := repoRootForWalTest(t)
	sites := []struct{ file, want string }{
		{"internal/cli/restore.go", "walfetchcmd.BuildWithIdentity(bin, deployment, repoURL, sysID)"},
		{"internal/restore/recovery.go", "walfetchcmd.BuildWithIdentity(bin, deployment, repoURL, sysID)"},
		{"internal/standby/standby.go", "walfetchcmd.BuildStandbyWithIdentity(m.binPath, opts.Deployment, opts.RepoURL,"},
		{"internal/timetravel/timetravel.go", "walfetchcmd.BuildWithIdentity(m.binPath, opts.Deployment, opts.RepoURL,"},
		{"internal/agent/restore_executor.go", "walfetchcmd.BuildWithIdentity(bin, job.Deployment, repoURL,"},
		{"internal/restore/postverify/postverify.go", "walfetchcmd.BuildWithIdentity(pickAgentBinary(agentBinary), deployment, repoURL, seedSysID)"},
	}
	for _, site := range sites {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(site.file)))
		if err != nil {
			t.Fatalf("read %s: %v", site.file, err)
		}
		src := string(body)
		if !strings.Contains(src, site.want) {
			t.Errorf("%s no longer arms the identity check (want %q) — a foreign-lineage "+
				"segment would reach PostgreSQL and fail only mid-replay, cryptically",
				site.file, site.want)
		}
		// No site may ALSO carry an unarmed call: that would mean two
		// generators in one file, one of them identity-blind.
		for _, bare := range []string{"walfetchcmd.Build(", "walfetchcmd.BuildStandby("} {
			if strings.Contains(src, bare) {
				t.Errorf("%s contains the unarmed %s — arm it via the WithIdentity variant "+
					"or document why this restore_command may replay foreign WAL", site.file, bare)
			}
		}
	}
}
