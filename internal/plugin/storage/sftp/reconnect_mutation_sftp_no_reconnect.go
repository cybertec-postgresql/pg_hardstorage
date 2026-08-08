//go:build mutation_sftp_no_reconnect

package sftp

// conn — MUTATED variant: no reconnection, the exact pre-fix world
// (bug #25). After the keepalive tears down a dead connection, every
// subsequent operation fails on the same dead client forever: a
// days-long `wal stream` holding this handle stops archiving after
// one 70-second network stall and stays stopped until an operator
// restarts the process.

import (
	"github.com/pkg/sftp"
)

func (p *Plugin) conn() (*sftp.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errClosed
	}
	return p.client, nil
}
