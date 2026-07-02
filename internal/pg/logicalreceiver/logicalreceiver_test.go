package logicalreceiver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// stubSink is a minimal Sink for the white-box validation tests —
// Stream's argument checks return before any sink method is called.
type stubSink struct{}

func (stubSink) OnRecord(context.Context, Record) error { return nil }
func (stubSink) SyncedLSN() pglogrepl.LSN               { return 0 }
func (stubSink) Flush(context.Context) error            { return nil }

// flushErrSink is a Sink whose Flush fails with a fixed error.
type flushErrSink struct{ err error }

func (flushErrSink) OnRecord(context.Context, Record) error { return nil }
func (flushErrSink) SyncedLSN() pglogrepl.LSN               { return 0 }
func (s flushErrSink) Flush(context.Context) error          { return s.err }

// TestFinalCommit_SurfacesFlushError pins poor-error-handling audit #3:
// the shutdown flush no longer swallows its error (it was `_ =
// sink.Flush(...)`), so a failed final commit is reported up rather than
// looking like a clean exit. finalCommit is the extracted, testable core
// of Stream's exit defer.
func TestFinalCommit_SurfacesFlushError(t *testing.T) {
	want := errors.New("disk full")
	err := finalCommit(flushErrSink{err: want})
	if err == nil {
		t.Fatal("finalCommit must surface a failed final flush; got nil")
	}
	if !errors.Is(err, want) {
		t.Errorf("error should wrap the flush cause for errors.Is; got %v", err)
	}
	if !strings.Contains(err.Error(), "final flush on shutdown") {
		t.Errorf("error should name the shutdown flush; got %v", err)
	}
}

// TestFinalCommit_NilOnSuccess: a clean final flush returns nil so Stream
// keeps whatever it was already returning (e.g. context.Canceled).
func TestFinalCommit_NilOnSuccess(t *testing.T) {
	if err := finalCommit(stubSink{}); err != nil {
		t.Errorf("finalCommit on a clean flush must return nil; got %v", err)
	}
}

// TestCreateLogicalSlot_Validation locks the argument guards: a nil
// connection and an empty slot name must fail fast with a clear error
// rather than panicking on a nil-pointer deref deeper in pglogrepl.
func TestCreateLogicalSlot_Validation(t *testing.T) {
	ctx := context.Background()

	if err := CreateLogicalSlot(ctx, nil, "s", "pgoutput"); err == nil {
		t.Error("nil connection must error")
	} else if !strings.Contains(err.Error(), "nil connection") {
		t.Errorf("nil-conn error should name the cause; got %v", err)
	}

	// Empty name with a non-nil (zero-value) conn: the name guard runs
	// before any method call on the connection, so a zero *pg.Conn is
	// safe here.
	if err := CreateLogicalSlot(ctx, &pg.Conn{}, "", "pgoutput"); err == nil {
		t.Error("empty slot name must error")
	} else if !strings.Contains(err.Error(), "empty slot name") {
		t.Errorf("empty-name error should name the cause; got %v", err)
	}
}

// TestDropLogicalSlot_Validation — a nil connection must error, not
// panic.
func TestDropLogicalSlot_Validation(t *testing.T) {
	if err := DropLogicalSlot(context.Background(), nil, "s"); err == nil {
		t.Error("nil connection must error")
	}
}

// TestStream_Validation sweeps Stream's three argument guards in the
// order Stream checks them: nil conn, nil sink, empty slot.
func TestStream_Validation(t *testing.T) {
	ctx := context.Background()

	if err := Stream(ctx, nil, StreamOptions{Slot: "s"}, stubSink{}); err == nil {
		t.Error("nil connection must error")
	} else if !strings.Contains(err.Error(), "nil connection") {
		t.Errorf("got %v", err)
	}

	if err := Stream(ctx, &pg.Conn{}, StreamOptions{Slot: "s"}, nil); err == nil {
		t.Error("nil sink must error")
	} else if !strings.Contains(err.Error(), "nil sink") {
		t.Errorf("got %v", err)
	}

	if err := Stream(ctx, &pg.Conn{}, StreamOptions{Slot: ""}, stubSink{}); err == nil {
		t.Error("empty slot name must error")
	} else if !strings.Contains(err.Error(), "empty slot name") {
		t.Errorf("got %v", err)
	}
}

// TestIsDuplicateObject covers the idempotency predicate that lets
// `logical add` be re-run against an existing slot without churn.
func TestIsDuplicateObject(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sqlstate 42710", errors.New("ERROR (SQLSTATE 42710)"), true},
		{"already exists phrase", errors.New(`replication slot "s" already exists`), true},
		{"unrelated error", errors.New("connection refused"), false},
		{"slot missing is not a dup", errors.New(`replication slot "s" does not exist`), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDuplicateObject(c.err); got != c.want {
				t.Errorf("isDuplicateObject(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestMessageAsCopyData — only pgproto3.CopyData unwraps to its
// payload; any other backend message reports ok=false so the receive
// loop skips it.
func TestMessageAsCopyData(t *testing.T) {
	payload := []byte{0x77, 0x01, 0x02}
	if got, ok := messageAsCopyData(&pgproto3.CopyData{Data: payload}); !ok {
		t.Error("CopyData must unwrap with ok=true")
	} else if string(got) != string(payload) {
		t.Errorf("payload = %v, want %v", got, payload)
	}

	if _, ok := messageAsCopyData(&pgproto3.NoticeResponse{}); ok {
		t.Error("NoticeResponse must report ok=false")
	}
	if _, ok := messageAsCopyData(nil); ok {
		t.Error("nil message must report ok=false")
	}
}

// TestControlMessageAction pins bug #26: a mid-stream ErrorResponse or
// CopyDone from the walsender must NOT be swallowed. Previously every
// non-CopyData message was ignored, so an ERROR re-blocked the loop in
// ReceiveMessage forever. controlMessageAction is the extracted, pure
// classifier that decides the loop's action.
func TestControlMessageAction(t *testing.T) {
	// ErrorResponse -> ctrlError with a descriptive, SQLSTATE-bearing err.
	act, err := controlMessageAction(&pgproto3.ErrorResponse{
		Severity: "ERROR", Code: "XX000", Message: "boom",
	})
	if act != ctrlError {
		t.Fatalf("ErrorResponse action = %v, want ctrlError", act)
	}
	if err == nil || !strings.Contains(err.Error(), "XX000") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("ErrorResponse error should carry SQLSTATE+message; got %v", err)
	}

	// CopyDone -> ctrlDone, clean shutdown (no error).
	if act, err := controlMessageAction(&pgproto3.CopyDone{}); act != ctrlDone || err != nil {
		t.Errorf("CopyDone action=%v err=%v, want ctrlDone/nil", act, err)
	}

	// NoticeResponse -> ctrlIgnore (keep streaming).
	if act, err := controlMessageAction(&pgproto3.NoticeResponse{}); act != ctrlIgnore || err != nil {
		t.Errorf("NoticeResponse action=%v err=%v, want ctrlIgnore/nil", act, err)
	}

	// CopyData -> ctrlNone (fall through to payload handling).
	if act, err := controlMessageAction(&pgproto3.CopyData{Data: []byte{0x77}}); act != ctrlNone || err != nil {
		t.Errorf("CopyData action=%v err=%v, want ctrlNone/nil", act, err)
	}
}

// TestUint64FromBytes — the XLogData-frame helper must big-endian
// decode 8 bytes and degrade to 0 on a short slice rather than
// panicking on an out-of-range index.
func TestUint64FromBytes(t *testing.T) {
	full := []byte{0, 0, 0, 0, 0, 0, 0x01, 0x00}
	if got := uint64FromBytes(full); got != 256 {
		t.Errorf("uint64FromBytes(%v) = %d, want 256", full, got)
	}
	if got := uint64FromBytes([]byte{1, 2, 3}); got != 0 {
		t.Errorf("short slice must yield 0, got %d", got)
	}
	if got := uint64FromBytes(nil); got != 0 {
		t.Errorf("nil slice must yield 0, got %d", got)
	}
}
