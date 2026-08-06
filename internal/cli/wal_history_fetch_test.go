package cli

// wal_history_fetch_test.go — serving the timeline-history file PG
// needs to walk past a failover.
//
// After a failover the cluster is on a new timeline, and a PITR that
// crosses it needs `<tli>.history`. PG asks for it by its archive name
// — eight hex digits, `00000002.history` — via restore_command. Two
// places can hold it:
//
//   - the archive_command aux path, populated by `wal push`;
//   - the streaming follower's timeline store, keyed by DECIMAL tli
//     (`wal/<dep>/timelines/2.history`), populated on failover.
//
// A `wal stream`-only HA deployment has no archive_command at all, so
// the follower store is the ONLY copy. fetchAuxBody bridges the two,
// and historyRequestTLI is the hex→decimal step in the middle.
//
// Neither was named by any test. The failure is quiet in the worst way:
// `recovery_target_timeline = 'latest'` (our default) makes PG follow
// the highest timeline it can find history for, so a history file it
// cannot fetch does not fail the restore — it silently recovers along
// an OLDER timeline and promotes a database missing everything written
// after the failover. The operator asked for latest and got less, with
// no error anywhere.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

func TestHistoryRequestTLI(t *testing.T) {
	cases := []struct {
		name string
		want uint32
		ok   bool
	}{
		// PG always pads to eight hex digits.
		{"00000002.history", 2, true},
		{"00000001.history", 1, true},
		// Hex matters: timeline 16 is "10", not ten. Parsing this as
		// decimal would look up the wrong file and serve the wrong
		// branch point — worse than serving nothing.
		{"00000010.history", 16, true},
		{"0000000A.history", 10, true},
		{"0000ffff.history", 65535, true},
		// Not history requests.
		{"000000010000000000000001", 0, false},
		{"backup_label", 0, false},
		{".history", 0, false},
		{"", 0, false},
		{"zzzzzzzz.history", 0, false},
	}
	for _, tc := range cases {
		got, ok := historyRequestTLI(tc.name)
		if ok != tc.ok {
			t.Errorf("historyRequestTLI(%q) ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("historyRequestTLI(%q) = %d, want %d — the archive name is HEX; "+
				"reading it as decimal serves a different timeline's branch point",
				tc.name, got, tc.want)
		}
	}
}

// memPlugin is the smallest storage plugin that can answer Get.
type historyMemPlugin struct {
	storage.StoragePlugin
	objects map[string][]byte
	gets    []string
}

func (p *historyMemPlugin) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p.gets = append(p.gets, key)
	body, ok := p.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// TestFetchAuxBody_FallsBackToTheFollowerStore is the case a
// stream-only HA deployment depends on: nothing ever ran
// archive_command, so the aux path is empty and the follower's timeline
// store is the only copy.
func TestFetchAuxBody_FallsBackToTheFollowerStore(t *testing.T) {
	const deployment = "db1"
	const tli = 2
	want := []byte("1\t0/3000000\tno recovery target specified\n")

	sp := &historyMemPlugin{objects: map[string][]byte{
		timeline.Path(deployment, tli): want,
	}}

	auxKey := walsink.AuxiliaryFilePath(deployment, "00000002.history", walsink.AuxiliaryHistory)
	got, err := fetchAuxBody(context.Background(), sp, auxKey,
		walsink.AuxiliaryHistory, deployment, "00000002.history")
	if err != nil {
		t.Fatalf("fetchAuxBody: %v\n\n"+
			"A wal-stream-only deployment has no archive_command, so the follower's "+
			"timeline store is the ONLY copy of this file. Without it PG cannot walk past "+
			"the failover: with recovery_target_timeline='latest' it silently follows the "+
			"highest timeline it CAN resolve and promotes a database missing everything "+
			"written after the switchover.", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("served %q, want %q", got, want)
	}
}

// TestFetchAuxBody_PrefersTheArchivePath: when both hold a copy, the
// aux path wins and the follower store is not consulted.
func TestFetchAuxBody_PrefersTheArchivePath(t *testing.T) {
	const deployment = "db1"
	auxKey := walsink.AuxiliaryFilePath(deployment, "00000002.history", walsink.AuxiliaryHistory)

	sp := &historyMemPlugin{objects: map[string][]byte{
		auxKey:                       []byte("from-archive"),
		timeline.Path(deployment, 2): []byte("from-follower"),
	}}
	got, err := fetchAuxBody(context.Background(), sp, auxKey,
		walsink.AuxiliaryHistory, deployment, "00000002.history")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-archive" {
		t.Errorf("served %q, want the archive copy", got)
	}
	if len(sp.gets) != 1 {
		t.Errorf("consulted %d key(s) (%v); the archive hit should short-circuit",
			len(sp.gets), sp.gets)
	}
}

// TestFetchAuxBody_AbsentEverywhereStaysNotFound: the fallback must not
// convert a genuine miss into something else. `wal fetch` maps
// ErrNotFound to the exit code PG's restore_command treats as "that
// file does not exist", which is how PG decides to stop looking for
// further timelines rather than failing recovery.
func TestFetchAuxBody_AbsentEverywhereStaysNotFound(t *testing.T) {
	const deployment = "db1"
	sp := &historyMemPlugin{objects: map[string][]byte{}}
	auxKey := walsink.AuxiliaryFilePath(deployment, "00000009.history", walsink.AuxiliaryHistory)

	_, err := fetchAuxBody(context.Background(), sp, auxKey,
		walsink.AuxiliaryHistory, deployment, "00000009.history")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound — restore_command relies on the miss being "+
			"typed; an untyped failure reads as a broken archive and fails recovery "+
			"outright", err)
	}
}

// TestFetchAuxBody_NonHistoryDoesNotConsultTheTimelineStore: a
// `.backup` miss must not be answered from the timeline store.
func TestFetchAuxBody_NonHistoryDoesNotConsultTheTimelineStore(t *testing.T) {
	const deployment = "db1"
	sp := &historyMemPlugin{objects: map[string][]byte{
		timeline.Path(deployment, 2): []byte("history-content"),
	}}
	auxKey := walsink.AuxiliaryFilePath(deployment, "00000002.backup", walsink.AuxiliaryBackup)

	if _, err := fetchAuxBody(context.Background(), sp, auxKey,
		walsink.AuxiliaryBackup, deployment, "00000002.backup"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound; a .backup request must never be answered "+
			"with a timeline-history body", err)
	}
}
