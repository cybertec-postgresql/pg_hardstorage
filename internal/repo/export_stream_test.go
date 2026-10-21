package repo

// SetReplicateStreamThresholdForTest lowers the size above which Replicate
// streams a manifest, so small test manifests exercise the streamed path.
func SetReplicateStreamThresholdForTest(n int64) (restore func()) {
	old := replicateStreamThreshold
	replicateStreamThreshold = n
	return func() { replicateStreamThreshold = old }
}
