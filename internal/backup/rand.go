// rand.go — crypto/rand.Read indirection so callers avoid the import for one-liners.
package backup

import "crypto/rand"

// readRand wraps crypto/rand.Read so callers don't need to import it
// just for one-line uses. Returns the same (n, err) the standard
// library does.
func readRand(b []byte) (int, error) {
	return rand.Read(b)
}
