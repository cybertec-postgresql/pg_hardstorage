package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestAgent_ControlPlane_NoDeployments_Refuses pins that a control-plane
// agent with an empty deployment list refuses to start. It advertises
// nothing it can run, so polling would at best be a no-op and -- before
// the control plane treated an empty set as "match nothing" -- claimed
// and failed every deployment's jobs.
func TestAgent_ControlPlane_NoDeployments_Refuses(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", configDir)
	writeAgentConfig(t, configDir, "schema: pg_hardstorage.config.v1\n")

	// Bounded: the pre-fix agent entered its poll loop and never
	// returned on its own.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetContext(ctx)
	root.SetArgs([]string{"agent", "--control-plane", "http://127.0.0.1:1", "--output", "json"})
	exit := cli.Run(root)
	if ctx.Err() != nil {
		t.Fatalf("agent ran until the test deadline instead of refusing (exit=%d)", exit)
	}
	if exit != int(output.ExitError) {
		t.Errorf("exit = %d, want %d\nstdout=%s\nstderr=%s", exit, output.ExitError, out.String(), errb.String())
	}
	if !strings.Contains(out.String()+errb.String(), "config.no_deployments") {
		t.Errorf("want config.no_deployments; stdout=%s stderr=%s", out.String(), errb.String())
	}
}
