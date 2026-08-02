// container_contract_test.go — the full storage-contract suite for the
// scp plugin against a CONTAINERISED OpenSSH server.
//
// contract_test.go runs the same suite against the host's
// /usr/sbin/sshd. That binding skips wherever sshd isn't installed —
// most CI images, most containers, macOS dev boxes — and pins the
// suite's behaviour to whatever OpenSSH version and sshd_config the
// host happens to ship. So the backend's only real-server coverage was
// simultaneously the least reproducible thing in the suite, and on a
// host without sshd it silently ran zero assertions.
//
// This binding uses the testkit's ssh-exec runtime (a pinned Alpine +
// OpenSSH image, exec permitted, throwaway per-instance keypair), which
// is the same footing every other backend's contract suite has.
//
//go:build integration

package scp_test

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func requireDockerSCP(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("PG_HARDSTORAGE_DEMAND_DOCKER") == "1" {
			t.Fatalf("docker not on PATH but PG_HARDSTORAGE_DEMAND_DOCKER=1: %v", err)
		}
		t.Skip("docker not on PATH; skipping containerised scp contract suite")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("PG_HARDSTORAGE_DEMAND_DOCKER") == "1" {
			t.Fatalf("docker daemon unreachable but PG_HARDSTORAGE_DEMAND_DOCKER=1: %v", err)
		}
		t.Skip("docker daemon not reachable; skipping containerised scp contract suite")
	}
}

// startSCPContainer brings up the ssh-exec runtime and returns a
// factory that opens the scp plugin against it.
func startSCPContainer(t *testing.T) (rt sink.Runtime, open func(t *testing.T) storage.StoragePlugin) {
	t.Helper()
	requireDockerSCP(t)

	rt, err := sink.New("ssh-exec")
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Up(context.Background()); err != nil {
		t.Fatalf("ssh-exec container: %v", err)
	}
	t.Cleanup(func() { _ = rt.Down(context.Background()) })

	u, err := url.Parse(rt.URL())
	if err != nil {
		t.Fatal(err)
	}
	extras := rt.Extras()

	return rt, func(t *testing.T) storage.StoragePlugin {
		t.Helper()
		p := &scp.Plugin{}
		if err := p.Open(context.Background(), storage.StorageConfig{
			URL:    u,
			Extras: extras,
		}); err != nil {
			t.Fatalf("scp.Open against container: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
}

// TestSCP_Contract_Container runs the shared invariant set — including
// the atomic `ln -T` single-winner commit the shared-DEK mint, backup
// lease and audit chain all depend on — against a real containerised
// sshd.
func TestSCP_Contract_Container(t *testing.T) {
	_, open := startSCPContainer(t)
	contract.Run(t, func(t *testing.T) storage.StoragePlugin { return open(t) })
}
