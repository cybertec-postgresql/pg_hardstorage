//go:build !mutation_sftp_no_reconnect

package sftp

// reconnect.go — a torn-down connection heals on the next operation.
//
// The keepalive (keepalive.go) turns a dead peer into an error instead
// of a hang by closing the connection. That is the right call for the
// operations parked on it — but it must not be a life sentence for the
// HANDLE: a `wal stream` holds one storage plugin for days, and the
// CLI opens the repo once, outside the retry loop. Without
// reconnection, one 70-second network stall left every subsequent
// attempt failing on the same dead client until an operator restarted
// the process — archiving stopped while the primary kept writing.
//
// conn is the gate every operation passes through: it hands back the
// live client, or — after a keepalive teardown — re-dials with the
// parameters Open stored. Redials are rate-limited so an ongoing
// outage produces one dial attempt per interval, not a stampede from
// every caller.

import (
	"fmt"
	"time"

	"github.com/pkg/sftp"
)

func (p *Plugin) conn() (*sftp.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.cfgssh == nil {
		// Closed, or never opened: there is nothing to reconnect TO —
		// Open is where dial parameters come from.
		return nil, errClosed
	}
	if p.client != nil && !p.dead {
		return p.client, nil
	}
	if since := time.Since(p.lastRedial); since < redialMinInterval {
		return nil, fmt.Errorf("sftp: connection lost %s ago; next reconnect attempt in %s",
			since.Round(time.Second), (redialMinInterval - since).Round(time.Second))
	}
	p.lastRedial = time.Now()
	if err := p.dialLocked(); err != nil {
		return nil, fmt.Errorf("sftp: reconnect: %w", err)
	}
	return p.client, nil
}
