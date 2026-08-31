package tarsink_test

// pgproto3 documents its read buffer explicitly:
//
//	// Receive receives a message from the backend. The returned
//	// message is only valid until the next call to Receive.
//
// basebackup's drainMultiplexed takes `payload := m.Data[1:]` straight
// off that message and hands it to Sink.OnTablespaceData. So every byte
// of every base backup arrives in a slice that the NEXT network read
// overwrites in place.
//
// The shipped sink is safe for two reasons that are easy to lose:
// bytes.Buffer.Write copies (the manifest path), and io.Pipe.Write
// blocks until the parser has consumed the bytes (the tablespace path).
// Neither is stated at the Sink interface, and a sink that queued the
// slice for asynchronous work — the natural way to add parallelism —
// would hash and store whatever the next read happened to leave there.
// The backup would succeed, the chunks would be self-consistent, and
// the corruption would surface as an unusable datadir at restore.
//
// The chunker has exactly this hazard and pins it with
// TestIter_DataReusesBuffer plus a loud comment. This is the same guard
// for the path that carries the whole cluster.

import (
	"bytes"
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/basebackup"
)

// TestSink_DoesNotRetainCallerBytes drives the sink the way the drain
// does — one slice, reused and overwritten between calls — and asserts
// the archived content is what was ORIGINALLY passed, not what the
// buffer held afterwards.
func TestSink_DoesNotRetainCallerBytes(t *testing.T) {
	sink, cas := newSinkAndCAS(t)
	ctx := context.Background()

	want := buildTar(t, []fileSpec{
		{name: "base/1/2619", body: bytes.Repeat([]byte("A"), 5000)},
		{name: "base/1/1259", body: bytes.Repeat([]byte("B"), 5000)},
		{name: "global/pg_control", body: bytes.Repeat([]byte("C"), 8192)},
	})

	if err := sink.OnTablespaceStart(0, basebackup.TablespaceInfo{OID: 1663}); err != nil {
		t.Fatal(err)
	}
	// One scratch buffer, reused for every frame and scribbled over
	// after each call — precisely pgproto3's contract.
	const frame = 777
	scratch := make([]byte, frame)
	for off := 0; off < len(want); off += frame {
		end := off + frame
		if end > len(want) {
			end = len(want)
		}
		n := copy(scratch, want[off:end])
		if err := sink.OnTablespaceData(0, scratch[:n]); err != nil {
			t.Fatalf("OnTablespaceData at offset %d: %v", off, err)
		}
		// The next Receive would overwrite the buffer. Do it now: if
		// the sink kept a reference instead of consuming the bytes,
		// what it archives is this garbage.
		for i := range scratch {
			scratch[i] = 0xFF
		}
	}
	if err := sink.OnTablespaceEnd(0); err != nil {
		t.Fatal(err)
	}

	files := sink.Files(0)
	if len(files) == 0 {
		t.Fatal("no files parsed; the fixture did not drive the sink")
	}
	for _, f := range files {
		var got []byte
		for _, ref := range f.Chunks {
			body, err := cas.GetChunkBytes(ctx, ref.Hash)
			if err != nil {
				t.Fatalf("%s: chunk %s: %v", f.Path, ref.Hash, err)
			}
			got = append(got, body...)
		}
		if bytes.Contains(got, bytes.Repeat([]byte{0xFF}, 64)) {
			t.Fatalf("%s archived a run of 0xFF — the sink retained the caller's slice past "+
				"OnTablespaceData and stored what the next network read overwrote it with. "+
				"The backup would succeed with self-consistent chunks and fail only at "+
				"restore, as an unusable datadir.", f.Path)
		}
	}
}

// The manifest path takes the same aliased slice.
func TestSink_ManifestDoesNotRetainCallerBytes(t *testing.T) {
	sink, _ := newSinkAndCAS(t)

	want := []byte(`{"PostgreSQL-Backup-Manifest-Version":1,"Files":[]}`)
	if err := sink.OnTablespaceStart(basebackup.ManifestSinkIndex,
		basebackup.TablespaceInfo{}); err != nil {
		t.Fatal(err)
	}
	scratch := make([]byte, len(want))
	copy(scratch, want)
	if err := sink.OnTablespaceData(basebackup.ManifestSinkIndex, scratch); err != nil {
		t.Fatal(err)
	}
	for i := range scratch {
		scratch[i] = 0x00
	}
	if err := sink.OnTablespaceEnd(basebackup.ManifestSinkIndex); err != nil {
		t.Fatal(err)
	}

	if got := sink.ManifestBytes(); !bytes.Equal(got, want) {
		t.Fatalf("manifest = %q, want %q — the manifest buffer aliased the caller's slice "+
			"instead of copying, so it holds whatever the next network read wrote", got, want)
	}
}
