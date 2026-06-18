// essentialfiles_integration_test.go — end-to-end coverage of the
// issue #84 fix.  Spins up a real PG testcontainer, deletes an
// always-required file (postgresql.auto.conf) from PGDATA while PG
// is running, takes a backup, and asserts the runner REFUSES to
// commit the manifest with a structured
// `backup.missing_essential_files` error — instead of silently
// producing an unrestorable backup the way pre-fix pg_hardstorage
// did.

//go:build integration

package runner_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestIntegration_EssentialFiles_AlwaysRequiredMissing_Refused
// reproduces issue #84's bug class against a live PG and pins the
// fix.  We delete postgresql.auto.conf from PGDATA — an always-
// required file regardless of the source PG's config-file layout —
// and assert that the runner refuses to commit the resulting
// manifest.
//
// Why postgresql.auto.conf and not postgresql.conf:
// testcontainers-go's `tcpostgres.WithConfigFile` mounts the
// repro config at an EXTERNAL path and starts PG with
// `-c config_file=...`, so `current_setting('config_file')` resolves
// to something outside PGDATA — meaning a missing
// postgresql.conf inside PGDATA would correctly NOT trip the gate
// (it lives elsewhere on this server).  postgresql.auto.conf is
// always written inside PGDATA by initdb, regardless of how
// config_file is overridden, so it's the right target.  The
// reporter's PG_HARDSTORAGE-on-Rocky setup did not redirect
// config_file, which is why postgresql.conf tripped the gate
// there.  Unit tests in essentialfiles_test.go cover the
// postgresql.conf-inside-PGDATA case explicitly.
func TestIntegration_EssentialFiles_AlwaysRequiredMissing_Refused(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 1. Control: a clean PG yields a clean backup.
	if _, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString:    srv.DSN,
		RepoURL:         repoURL,
		Deployment:      "db1",
		Signer:          signer,
		Verifier:        verifier,
		Fast:            true,
		IncludeManifest: true,
		OnEvent:         func(*output.Event) {},
	}); err != nil {
		t.Fatalf("control backup against a clean PG: %v", err)
	}

	// 2. Simulate the issue #84 source-data corruption inside the
	//    running container.  postgresql.auto.conf is always at the
	//    root of PGDATA; resolve PGDATA from the image (it moved on
	//    PG 18) rather than hardcoding the path.
	pgdata := srv.DataDir(t)
	rc, reader, err := srv.Container.Exec(ctx, []string{
		"rm", "-f", pgdata + "/postgresql.auto.conf",
	})
	if err != nil {
		t.Fatalf("container exec rm: %v", err)
	}
	if rc != 0 {
		out, _ := io.ReadAll(reader)
		t.Fatalf("container rm exited %d: %s", rc, string(out))
	}

	// 3. Take a second backup.  Pre-fix this silently succeeded;
	//    post-fix it must fail with the structured error.
	_, err = runner.Take(ctx, runner.TakeOptions{
		PGConnString:    srv.DSN,
		RepoURL:         repoURL,
		Deployment:      "db1",
		Signer:          signer,
		Verifier:        verifier,
		Fast:            true,
		IncludeManifest: true,
		OnEvent:         func(*output.Event) {},
	})

	if err == nil {
		t.Fatal("issue #84 regression: backup of a PGDATA missing postgresql.auto.conf was silently accepted")
	}

	// 4. The structured error must reference the missing file and
	//    carry the documented error code so scripts can branch.
	if !strings.Contains(err.Error(), "postgresql.auto.conf") {
		t.Errorf("error message %q does not name postgresql.auto.conf — operator has no actionable hint", err.Error())
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) {
		t.Fatalf("expected an output.Error wrap; got %T: %v", err, err)
	}
	if oerr.Code != "backup.missing_essential_files" {
		t.Errorf("error code = %q, want backup.missing_essential_files", oerr.Code)
	}
	if oerr.Suggestion == nil || oerr.Suggestion.Human == "" {
		t.Error("structured error should carry a Suggestion the CLI surfaces to the operator")
	}

	// 5. Underlying MissingEssentialFilesError should be in the
	//    error chain with postgresql.auto.conf in AlwaysRequired.
	var me *backup.MissingEssentialFilesError
	if !errors.As(err, &me) {
		t.Fatalf("MissingEssentialFilesError not in chain; got %v", err)
	}
	found := false
	for _, c := range me.AlwaysRequired {
		if c == "postgresql.auto.conf" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("MissingEssentialFilesError.AlwaysRequired = %v, want it to contain postgresql.auto.conf",
			me.AlwaysRequired)
	}
}
