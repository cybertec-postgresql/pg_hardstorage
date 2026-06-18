package restore_test

import "os"

// writeAt is a tiny helper to write a fixture file. Pulled out so the
// checkpoint test file's only os.WriteFile call lives in one spot.
func writeAt(path string, body []byte) error {
	return os.WriteFile(path, body, 0o600)
}
