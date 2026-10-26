package backup

// SetMaxCheckMissingListed lowers the missing-hash list cap for a test.
func SetMaxCheckMissingListed(n int) (restore func()) {
	old := maxCheckMissingListed
	maxCheckMissingListed = n
	return func() { maxCheckMissingListed = old }
}
