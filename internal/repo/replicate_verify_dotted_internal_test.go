package repo

import "testing"

// isWALSegmentManifestKey must recognise a committed segment manifest under a
// deployment whose name contains ".json.tmp." — else replica-verify skips it
// and can report "consistent" against a replica missing that segment.
func TestIsWALSegmentManifestKey_DottedDeployment(t *testing.T) {
	committed := "wal/db.json.tmp.x/00000001/000000010000000000000001.json"
	if !isWALSegmentManifestKey(committed) {
		t.Fatalf("committed segment %q not recognised — replica-verify would skip it (false 'consistent')", committed)
	}
	temp := "wal/db1/00000001/000000010000000000000001.json.tmp.abc"
	if isWALSegmentManifestKey(temp) {
		t.Fatalf("staging temp %q wrongly recognised as a committed segment", temp)
	}
}
