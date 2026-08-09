package cli

// wal_fetch_fuzz_test.go — crash-freedom of wal fetch's own request
// parsers. See walsink's fuzz_test.go for the stakes: a panic here
// exits 2, which a STANDBY's restore_command tail reads as "not
// archived yet" — one malformed request name and the replica stalls
// silently forever.

import "testing"

func FuzzParseSegmentNameForFetch(f *testing.F) {
	f.Add("000000010000000000000005")
	f.Add("gggggggggggggggggggggggg")
	f.Add("")
	f.Add("000000010000000000000005extra")
	f.Fuzz(func(t *testing.T, name string) {
		_, _, _ = parseSegmentNameForFetch(name)
	})
}

func FuzzHistoryRequestTLI(f *testing.F) {
	f.Add("00000002.history")
	f.Add(".history")
	f.Add("FFFFFFFFF.history")
	f.Add("00000002.history.history")
	f.Fuzz(func(t *testing.T, name string) {
		_, _ = historyRequestTLI(name)
	})
}

func FuzzSafeAuxBasename(f *testing.F) {
	f.Add("00000002.history")
	f.Add("../escape")
	f.Add("a/b")
	f.Add("\x00")
	f.Fuzz(func(t *testing.T, name string) {
		_ = safeAuxBasename(name)
	})
}
