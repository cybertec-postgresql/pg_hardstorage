//go:build mutation_sftp_no_keepalive

package sftp

// startKeepalive — MUTATED variant: no keepalive exists, the exact
// pre-fix world (bug #23). ssh.ClientConfig.Timeout covers only the
// dial; once the peer goes silently away (NAT expiry, power-off,
// partition), every pkg/sftp operation blocks forever — a wal fetch
// inside restore_command hangs recovery indefinitely, a backup never
// finishes and never fails, an archiver stalls while pg_wal fills.

import (
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func startKeepalive(conn *ssh.Client, cli *sftp.Client, stop <-chan struct{}) {}
