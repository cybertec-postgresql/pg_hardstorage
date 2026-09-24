//go:build integration

package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Regression (H3): `partial dump` streams SQL to stdout by default (the
// help suggests piping it into psql), but the restore progress events
// and the Result summary went to stdout too, interleaved with the SQL.
// With SQL on stdout, stdout must be SQL only; events and the summary
// belong on stderr.
func TestIntegration_PartialDumpToStdoutIsSQLOnly(t *testing.T) {
	needLocalBin(t, "pg_dump")
	src := startLocalPGSource(t)
	src.SQL(t, "CREATE TABLE public.users(id int primary key, name text); INSERT INTO public.users VALUES (1,'alice'),(2,'bob')")
	w := newReadWorld(t)
	stubRestoreBin(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	b, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: src.DSN, RepoURL: w.repoURL, Deployment: "db1",
		Signer: w.signer, Verifier: w.verifier, Fast: true, IncludeWAL: true,
	})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			stdout, stderr, exit := runCLI(t, "partial", "dump", "db1",
				"--repo", w.repoURL, "--backup", b.BackupID, "--tables", "public.users", "-o", format)
			if exit != int(output.ExitOK) {
				t.Fatalf("exit=%d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
			}
			if !strings.Contains(stdout, "CREATE TABLE public.users") || !strings.Contains(stdout, "alice") {
				t.Fatalf("stdout lacks the dumped SQL:\n%s", stdout)
			}
			for _, noise := range []string{"pg_hardstorage.v1", "bytes_emitted", "partial dump", "\"schema\""} {
				if strings.Contains(stdout, noise) {
					t.Errorf("stdout (the SQL stream) contains %q — events/result leaked into the SQL:\n%s", noise, stdout)
				}
			}
			if !strings.Contains(stderr, "bytes_emitted") && !strings.Contains(stderr, "Bytes") {
				t.Errorf("the summary should be on stderr; stderr:\n%s", stderr)
			}
			if format == "json" {
				var v map[string]any
				dec := json.NewDecoder(strings.NewReader(stderr))
				if err := dec.Decode(&v); err != nil {
					t.Errorf("stderr is not the JSON result: %v\n%s", err, stderr)
				}
			}
		})
	}
}
