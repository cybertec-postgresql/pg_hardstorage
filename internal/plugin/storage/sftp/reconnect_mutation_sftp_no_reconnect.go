//go:build mutation_sftp_no_reconnect

package sftp

// conn — MUTATED variant: no reconnection, the exact pre-fix world
// (bug #25). After the keepalive tears down a dead connection, every
// subsequent operation fails on the same dead client forever: a
// days-long `wal stream` holding this handle stops archiving after
// one 70-second network stall and stays stopped until an operator
// restarts the process.

import (
	"time"

	"github.com/pkg/sftp"
)

func (p *Plugin) conn() (*sftp.Client, error) {
	cli, _, _, err := p.connGen()
	return cli, err
}

// connGen is mutated too: the read-guarded paths (Get/Stat/List) take
// their client from it, and a mutation that left them reconnecting
// would no longer be caught by the reconnect test.
func (p *Plugin) connGen() (*sftp.Client, uint64, time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, 0, 0, errClosed
	}
	return p.client, p.gen, readIdleTimeout, nil
}
