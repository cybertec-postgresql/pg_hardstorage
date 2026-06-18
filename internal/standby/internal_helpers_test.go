package standby_test

import "os"

// osWriteFile is a tiny indirection so the test file's only call to
// os.WriteFile lives in one spot — keeps imports tidy when other
// tests grow.
func osWriteFile(path string, body []byte, mode os.FileMode) error {
	return os.WriteFile(path, body, mode)
}
