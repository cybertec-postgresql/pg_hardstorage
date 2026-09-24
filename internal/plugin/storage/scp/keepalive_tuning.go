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

	// redialMinInterval bounds how often a failed reconnect is retried.
	redialMinInterval = 5 * time.Second
)
