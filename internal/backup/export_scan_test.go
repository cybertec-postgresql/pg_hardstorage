package backup

import "io"

// ScanTopLevelStringsForTest exposes scanTopLevelStrings to backup_test.
func ScanTopLevelStringsForTest(r io.Reader, want ...string) (map[string]string, error) {
	return scanTopLevelStrings(r, want...)
}
