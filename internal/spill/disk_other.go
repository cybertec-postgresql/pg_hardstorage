//go:build !unix

package spill

// freeBytes: no portable free-space probe; the preflight is skipped.
func freeBytes(string) (int64, bool) { return 0, false }
