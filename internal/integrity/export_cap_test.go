package integrity

// SetMaxRecordedChunkFailures lowers the failure cap for a test and returns
// the function that restores it.
func SetMaxRecordedChunkFailures(n int) (restore func()) {
	old := maxRecordedChunkFailures
	maxRecordedChunkFailures = n
	return func() { maxRecordedChunkFailures = old }
}
