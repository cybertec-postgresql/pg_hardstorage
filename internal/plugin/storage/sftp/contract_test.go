// Contract suite for the SFTP plugin.  Drives the shared
// invariant set against atmoz/sftp (an OpenSSH-based SFTP
// server image).  The plugin parses creds out of the URL
// (sftp://user:pass@host:port/path), so EnvForAgent is
// empty for this binding — there's nothing to set in the
// Go process env.
package sftp_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/sftp"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func openSFTPOnFresh(t *testing.T) storage.StoragePlugin {
	t.Helper()
	s := sink.UpForTest(t, "sftp")
	u, err := url.Parse(s.URL())
	if err != nil {
		t.Fatalf("parse sink URL %s: %v", s.URL(), err)
	}
	p := &sftp.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{
		URL:    u,
		Extras: s.Extras(),
	}); err != nil {
		t.Fatalf("sftp.Open(%s): %v", s.URL(), err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestSFTP_Contract(t *testing.T) {
	contract.Run(t, openSFTPOnFresh)
}

// TestSFTP_Contract_ParallelPuts exercises the
// IfNotExists clause under concurrent stress.  The SFTP
// plugin used to have a check-then-create race here —
// multiple writers could pass the Stat-then-rename
// sequence and end up with several "winners".  The race
// is now closed via OpenSSH's hardlink@openssh.com
// extension: write to a tmp file, then Link(tmp, full),
// which is atomic (POSIX link(2) fails with EEXIST when
// dst is present).  Servers without the extension fall
// back to the legacy race-y path; the agent's audit
// chain serialises manifest commits in production, so
// even on those servers the prod risk is bounded.
func TestSFTP_Contract_ParallelPuts(t *testing.T) {
	contract.ParallelPuts(t, openSFTPOnFresh, 8)
}

func TestSFTP_Contract_ParallelOverwrites(t *testing.T) {
	contract.ParallelOverwrites(t, openSFTPOnFresh, 8)
}
