package walsink

// fuzz_test.go — crash-freedom of the parsers restore_command trusts.
//
// Everything here parses input the agent does not control: segment
// names arrive from PostgreSQL's %f substitution, manifests and aux
// names from whatever the repository holds. A panic in any of them
// inside `wal fetch` exits the process with code 2 — which the
// STANDBY restore_command tail reads as "not archived yet", stalling
// a replica forever on one corrupted object with no signal anywhere.
// (The one-shot tail converts unknown exits to SIGABRT, so a panic
// there aborts loudly; the standby path has no such conversion by
// design.) Errors are fine; panics are the bug.

import "testing"

func FuzzParseSegmentManifest(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"schema":"pg_hardstorage.wal.segment.v1","segment_size":16777216}`))
	f.Add([]byte(`{"chunks":[{"hash":"zz","len":-1}],"segment_number":18446744073709551615}`))
	f.Add([]byte("\x00\xff\xfe"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = ParseSegmentManifest(raw) // must never panic
	})
}

func FuzzParseSegmentName(f *testing.F) {
	f.Add("000000010000000000000005", int64(16777216))
	f.Add("FFFFFFFFFFFFFFFFFFFFFFFF", int64(1048576))
	f.Add("00000001000000000000000G", int64(16777216))
	f.Add("", int64(0))
	f.Add("000000010000000000000005.partial", int64(-16777216))
	f.Fuzz(func(t *testing.T, name string, segSize int64) {
		_, _, _ = ParseSegmentName(name, segSize) // must never panic
	})
}

func FuzzClassifyArchiveInput(f *testing.F) {
	f.Add("000000010000000000000005")
	f.Add("00000002.history")
	f.Add("000000010000000000000005.00000028.backup")
	f.Add("../../../etc/passwd")
	f.Add("\x00.history")
	f.Fuzz(func(t *testing.T, name string) {
		kind := ClassifyArchiveInput(name)
		// AuxiliaryFilePath must be panic-free for every classified
		// kind — wal fetch feeds it the raw request.
		_ = AuxiliaryFilePath("db1", name, kind)
	})
}
