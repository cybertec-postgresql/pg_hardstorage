// Package version exposes build-time version metadata.
//
// Values are populated at link time via -ldflags -X (see Makefile).
package version

var (
	// Version is the human-readable release tag, e.g. "v0.1.0" or "dev".
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "none"
	// Date is the UTC timestamp at which the binary was built (RFC 3339).
	Date = "unknown"
)
