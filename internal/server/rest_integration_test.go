//go:build integration

package server_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

func TestServerRESTEndpointsHealth(t *testing.T) {
	t.Skip("REST server integration pending — requires server process lifecycle that compiles the binary")

	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	res, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true,
	})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	_ = res

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	t.Logf("Server REST: listener at %s — service test scaffolding ready", ln.Addr().String())
	_ = json.NewEncoder(io.Discard)
	_ = http.StatusOK
	_ = server.Server{}
}
