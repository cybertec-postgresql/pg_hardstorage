//go:build !mutation_sftp_no_keepalive

package sftp

// keepalive.go — a dead SFTP connection must become an ERROR, not a
// hang.
//
// Found by the storage fault soak: a worker sat 14 minutes inside
// pkg/sftp's sendPacket on a connection whose peer had silently gone
// away, and the only thing that ended the wait was the test binary's
// one-hour timeout. pkg/sftp takes no context and sets no deadlines,
// and ssh.ClientConfig.Timeout covers only the DIAL — after that, a
// NAT table entry expiring, a peer power-off, or a network partition
// leaves every outstanding and future operation blocked forever. In
// production that is a `wal fetch` inside restore_command hanging
// recovery indefinitely (PostgreSQL waits on the command, so even the
// strict signal tail cannot fire), a backup that never finishes and
// never fails, an archiver stalled while pg_wal fills the disk.
//
// The fix is the standard one (OpenSSH's ServerAliveInterval): an
// SSH-level keepalive request on a ticker. A reply proves the
// round-trip; a timeout or error counts a miss; enough consecutive
// misses close the connection — which unblocks every goroutine parked
// in sendPacket with a "connection closed" error that the caller's
// retry/refusal machinery can actually see. TCP keepalives alone are
// not enough: kernel defaults take over two hours to declare a peer
// dead.

import (
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// startKeepalive probes conn until stop closes, and tears the
// connection down after keepaliveMisses consecutive failed probes.
// Closing the connection is the point: it is the only lever that
// unblocks pkg/sftp operations already in flight.
func startKeepalive(conn *ssh.Client, cli *sftp.Client, stop <-chan struct{}) {
	// Snapshot the tuning on the CALLER's goroutine: tests shrink and
	// restore the package variables around each case, and a prober
	// that read them directly raced the restore (caught by -race).
	interval, timeout, missBudget := keepaliveInterval, keepaliveTimeout, keepaliveMisses
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		misses := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			// SendRequest blocks on the same dead connection it is
			// probing, so it runs in its own goroutine with a timer
			// deciding the verdict. A stalled probe goroutine is
			// unblocked by the eventual conn.Close and exits.
			done := make(chan error, 1)
			go func() {
				_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					misses++
				} else {
					misses = 0
				}
			case <-time.After(timeout):
				misses++
			case <-stop:
				return
			}
			if misses >= missBudget {
				// Dead peer: fail every parked and future operation
				// now, loudly, instead of hanging forever. TRANSPORT
				// FIRST: sftp.Client.Close writes on the same dead
				// connection (a blackholed write "succeeds", only the
				// reply is missing), so closing the client first can
				// itself block forever — the first version of this
				// teardown did exactly that, and the hang test caught
				// it. Killing the SSH transport unblocks everything,
				// including the client Close that follows.
				_ = conn.Close()
				_ = cli.Close()
				return
			}
		}
	}()
}
