package sftp

// Keepalive tuning, shared by the real and mutated keepalive
// variants (untagged file so tests build under both). Package
// variables, not constants, so the hang test can shrink them to
// milliseconds; production values give a worst-case dead-peer
// detection latency of interval + misses*timeout ≈ 70s.

import "time"

var (
	keepaliveInterval = 30 * time.Second
	keepaliveTimeout  = 20 * time.Second
	keepaliveMisses   = 2

	// readIdleTimeout bounds a single data-plane SFTP READ (a Get
	// stream, a Stat/List round-trip) that makes no progress. The
	// keepalive covers a DEAD peer (control-channel probes fail →
	// close). It does NOT cover a LIVE peer whose control channel
	// still answers keepalives while one data-channel READ is wedged
	// (the Sept-30 campaign's 10–14m buffer.Read stalls — a `wal
	// fetch` inside restore_command hanging recovery). pkg/sftp and
	// x/crypto/ssh set no deadline after the dial, and the peer's
	// keepalive replies keep the socket actively receiving, so a TCP
	// read deadline would never fire: the only lever that unblocks a
	// parked buffer.Read is closing the transport (readguard.go). A
	// healthy read returns in milliseconds, so a no-progress window
	// this large is unambiguously wedged. Package var so tests shrink
	// it to milliseconds; 0 disables the guard.
	readIdleTimeout = 60 * time.Second

	// redialMinInterval bounds how often a failed reconnect is
	// retried (reconnect.go); shared here so tests build under the
	// no-reconnect mutant too.
	redialMinInterval = 5 * time.Second
)
