// rand.go — crypto/rand indirection for event-ID generation (overridable in tests).
package audit

import (
	cryptorand "crypto/rand"
)

// cryptoRandRead is the production source for newEventID's random
// component. Kept in its own file so the test_hooks file can pin
// the indirection without pulling crypto/rand into test source.
func cryptoRandRead(b []byte) (int, error) {
	return cryptorand.Read(b)
}
