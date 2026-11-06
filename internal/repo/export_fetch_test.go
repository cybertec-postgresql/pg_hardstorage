package repo

import (
	"testing"
	"time"
)

// SetChunkReadBackoffForTest replaces the pause between retries of a
// broken chunk read for the duration of t.
func SetChunkReadBackoffForTest(t testing.TB, f func(int) time.Duration) {
	old := chunkReadBackoff
	chunkReadBackoff = f
	t.Cleanup(func() { chunkReadBackoff = old })
}
