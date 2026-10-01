package scp

// Keepalive / reconnect tuning — the sftp plugin's values
// (sftp/keepalive_tuning.go). Package variables, not constants, so
// the tests can shrink them to milliseconds; production values give a
// worst-case dead-peer detection latency of interval + misses*timeout
// ≈ 70s.

import "time"

var (
	keepaliveInterval = 30 * time.Second
	keepaliveTimeout  = 20 * time.Second
	keepaliveMisses   = 2

	// readIdleTimeout bounds a single data-plane SCP READ (a Get's
	// stdout drain) that makes no progress. The keepalive covers a
	// DEAD peer (control-channel probes fail → close). It does NOT
	// cover a LIVE peer whose control channel still answers
	// keepalives while one data-channel READ is wedged — the
	// Sept-30 campaign's 1h TestFaultSoak stdout stall (io.Copy →
	// channel.Read → sync.Cond.Wait). x/crypto/ssh sets no deadline
	// after the dial, and the peer's keepalive replies keep the
	// socket actively receiving, so a TCP read deadline would never
	// fire: the only lever that unblocks a parked channel.Read is
	// closing the transport. A healthy read returns in milliseconds,
	// so a no-progress window this large is unambiguously wedged.
	// Package var so tests shrink it to milliseconds; 0 disables.
	readIdleTimeout = 60 * time.Second

	// redialMinInterval bounds how often a failed reconnect is
	// retried.
	redialMinInterval = 5 * time.Second
)
