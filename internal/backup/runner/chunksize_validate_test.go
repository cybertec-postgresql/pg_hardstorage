package runner

import (
	"strings"
	"testing"
)

// The runner enforces the chunk-size bounds itself: it is a library entry
// point too, and a 1-byte average would turn a backup into one object per
// handful of bytes.
func TestValidateOptions_ChunkAvgSizeBounds(t *testing.T) {
	for _, tc := range []struct {
		avg     int
		invalid bool
	}{
		{0, false}, {16 << 10, false}, {1 << 20, false}, {16 << 20, false},
		{1, true}, {16<<10 - 1, true}, {16<<20 + 1, true}, {-1, true},
	} {
		o := &TakeOptions{PGConnString: "host=x", RepoURL: "file:///x", Deployment: "d", ChunkAvgSize: tc.avg}
		err := validateOptions(o)
		got := err != nil && strings.Contains(err.Error(), "ChunkAvgSize")
		if got != tc.invalid {
			t.Errorf("ChunkAvgSize=%d: invalid=%v (err=%v), want invalid=%v", tc.avg, got, err, tc.invalid)
		}
	}
}
