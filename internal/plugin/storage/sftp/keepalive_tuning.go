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

	// redialMinInterval bounds how often a failed reconnect is
	// retried (reconnect.go); shared here so tests build under the
	// no-reconnect mutant too.
	redialMinInterval = 5 * time.Second
)
