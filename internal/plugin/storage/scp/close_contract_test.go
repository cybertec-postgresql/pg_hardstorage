package scp_test

// close_contract_test.go — a successful read must Close cleanly.
//
// io.Closer's contract is that Close returns nil when nothing went
// wrong. scp's reader returned io.EOF instead: reap() waits for the
// remote `cat` to finish, and calling Close on an already-finished
// ssh.Session yields io.EOF, which was passed straight through.
//
// `defer rc.Close()` hides this, which is why it survived from the
// v1.0.0 release. A caller doing the correct thing —
//
//	b, rerr := io.ReadAll(rc)
//	cerr := rc.Close()
//	if rerr != nil || cerr != nil { ...treat the read as failed... }
//
// — throws away perfectly good bytes. The read-race soak does exactly
// that and reported ZERO complete reads on scp across a 10-second
// budget while sftp managed 19,115. Every one of those reads had
// succeeded; the verdict was wrong, not the data.
//
// Pinned here rather than left to the soak: the soak needs Docker and
// minutes of budget, and this is a one-line contract that any backend
// can be held to cheaply.

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func TestSCP_CloseReturnsNilAfterASuccessfulRead(t *testing.T) {
	requireSSHD(t)
	sp := openSCPOnFresh(t)
	ctx := context.Background()

	body := bytes.Repeat([]byte("x"), 8192)
	if _, err := sp.Put(ctx, "close-contract.bin", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put: %v", err)
	}

	for i := 0; i < 3; i++ {
		rc, err := sp.Get(ctx, "close-contract.bin")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		got, rerr := io.ReadAll(rc)
		cerr := rc.Close()

		if rerr != nil {
			t.Fatalf("read %d: %v", i, rerr)
		}
		if len(got) != len(body) {
			t.Fatalf("read %d: got %d bytes, want %d", i, len(got), len(body))
		}
		if cerr != nil {
			t.Errorf("read %d: Close returned %v after reading all %d bytes.\n\n"+
				"io.Closer requires nil on success. A caller that checks Close — the "+
				"correct way to use an io.ReadCloser — discards a complete, correct read. "+
				"io.EOF here means the ssh session had already finished, which after "+
				"reap() is the normal case rather than a failure.",
				i, cerr, len(got))
		}
	}
}

// TestSCP_CloseStillReportsARealFailure guards the other direction: the
// fix must not swallow a genuine error. A read of a key that vanishes
// mid-stream, or a session that dies, still has to surface.
func TestSCP_CloseStillReportsARealFailure(t *testing.T) {
	requireSSHD(t)
	sp := openSCPOnFresh(t)
	ctx := context.Background()

	// Reading a key that does not exist must fail at Get, not silently
	// produce an empty successful read.
	if _, err := sp.Get(ctx, "definitely-absent.bin"); err == nil {
		t.Fatal("Get of an absent key returned no error; a caller cannot tell an empty " +
			"object from a missing one")
	}
}
