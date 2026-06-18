package server_test

import "os"

// osWriteFile is a tiny indirection so the test file's only call to
// os.WriteFile lives in one spot.
func osWriteFile(path string, body []byte) error {
	return os.WriteFile(path, body, 0o600)
}
