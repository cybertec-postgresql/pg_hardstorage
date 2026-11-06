package audit

import "io"

// VerifyBundleWithLimits runs the production verifier with the given
// bounds in place of the Max* constants.
func VerifyBundleWithLimits(r io.Reader, entries int, entryBytes, totalBytes int64) (*BundleManifest, error) {
	return verifyBundle(r, VerifyBundleOptions{}, bundleLimits{entries: entries, entryBytes: entryBytes, totalBytes: totalBytes})
}

// DefaultBundleLimits returns the bounds VerifyBundle runs with.
func DefaultBundleLimits() (entries int, entryBytes, totalBytes int64) {
	return defaultBundleLimits.entries, defaultBundleLimits.entryBytes, defaultBundleLimits.totalBytes
}
