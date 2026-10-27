package restore

import "testing"

func TestCappedBuffer_KeepsHeadDropsRest(t *testing.T) {
	c := &cappedBuffer{max: 5}
	for _, s := range []string{"abc", "defgh", "ij"} {
		if n, err := c.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d,%v; want full write", s, n, err)
		}
	}
	if got := c.String(); got != "abcde" {
		t.Fatalf("got %q, want abcde", got)
	}
}
