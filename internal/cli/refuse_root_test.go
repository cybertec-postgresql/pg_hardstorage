package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRefuseRoot_NonRootIsNoop is the happy-path check: an
// agent running as a regular user (any uid != 0) must pass the
// gate without complaint.  Production never lands here because
// the binary literally cannot start as root after this change,
// but a unit test still needs to confirm the gate is permissive
// for the legitimate posture.
func TestRefuseRoot_NonRootIsNoop(t *testing.T) {
	withGeteuid(t, 1000, func() {
		if err := refuseRoot(&cobra.Command{Use: "backup"}, nil); err != nil {
			t.Fatalf("refuseRoot under uid 1000 returned %v; want nil", err)
		}
	})
}

// TestRefuseRoot_RootIsRefused: the load-bearing assertion.
// euid 0 + any subcommand except the allow-list returns a
// structured usage.refused_as_root error that wraps
// output.ErrUsage so the dispatcher emits ExitMisuse (2).
func TestRefuseRoot_RootIsRefused(t *testing.T) {
	withGeteuid(t, 0, func() {
		err := refuseRoot(&cobra.Command{Use: "backup"}, nil)
		if err == nil {
			t.Fatal("refuseRoot under uid 0 returned nil; want refused")
		}
		var se *output.Error
		if !errors.As(err, &se) {
			t.Fatalf("err is not *output.Error: %T %v", err, err)
		}
		if se.Code != "usage.refused_as_root" {
			t.Errorf("code = %q, want usage.refused_as_root", se.Code)
		}
		if !errors.Is(err, output.ErrUsage) {
			t.Error("err should wrap output.ErrUsage so dispatcher maps to ExitMisuse")
		}
		if !strings.Contains(se.Message, "pgbackup") {
			t.Errorf("message should mention pgbackup remediation; got %q", se.Message)
		}
	})
}

// TestRefuseRoot_AllowsVersion: even under uid 0, the `version`
// subcommand is allowed.  Packaging postinst scripts shell out
// to `pg_hardstorage version` for sanity checks at install
// time, and those run as root by definition (dpkg / rpm
// scriptlet context).
func TestRefuseRoot_AllowsVersion(t *testing.T) {
	withGeteuid(t, 0, func() {
		if err := refuseRoot(&cobra.Command{Use: "version"}, nil); err != nil {
			t.Errorf("refuseRoot blocked `version` under uid 0: %v", err)
		}
	})
}

// TestRefuseRoot_AllowsVersionSubcommand: a subcommand of an
// allow-listed command inherits the allow-list — `version
// --json` would dispatch a child of `version` and must still
// pass.
func TestRefuseRoot_AllowsVersionSubcommand(t *testing.T) {
	withGeteuid(t, 0, func() {
		parent := &cobra.Command{Use: "version"}
		child := &cobra.Command{Use: "json-shape"}
		parent.AddCommand(child)
		if err := refuseRoot(child, nil); err != nil {
			t.Errorf("refuseRoot blocked allow-listed parent's child: %v", err)
		}
	})
}

// TestRefuseRoot_RootBlocksAgent: the most consequential
// rejection — the agent subcommand under uid 0 must fail.
// Production deployments run the agent via systemd (User=pgbackup)
// or a k8s pod with runAsNonRoot: true; any path that lands at
// agent-as-root is a posture bug we want surfaced immediately.
func TestRefuseRoot_RootBlocksAgent(t *testing.T) {
	withGeteuid(t, 0, func() {
		err := refuseRoot(&cobra.Command{Use: "agent"}, nil)
		if err == nil {
			t.Fatal("agent under uid 0 must be refused")
		}
		var se *output.Error
		if !errors.As(err, &se) || se.Code != "usage.refused_as_root" {
			t.Errorf("err = %v; want usage.refused_as_root", err)
		}
	})
}

// withGeteuid swaps the package-level geteuid seam for the
// duration of fn, restoring the previous value on exit.  Lets
// us test both the root and non-root branches in a single
// process without actually fork+setuid'ing.
func withGeteuid(t *testing.T, uid int, fn func()) {
	t.Helper()
	prev := geteuid
	geteuid = func() int { return uid }
	t.Cleanup(func() { geteuid = prev })
	fn()
}
