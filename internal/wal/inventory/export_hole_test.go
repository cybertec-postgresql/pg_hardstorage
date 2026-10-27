package inventory

// SetHoleScanMaxForTest lowers the hole-scan bitmap bound; the returned
// func restores it.
func SetHoleScanMaxForTest(n uint64) func() {
	old := holeScanMaxSegments
	holeScanMaxSegments = n
	return func() { holeScanMaxSegments = old }
}
