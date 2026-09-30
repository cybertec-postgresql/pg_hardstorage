package llmprovider

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// stallReader is the safety valve on a streaming LLM response: it
// distinguishes a server that has gone away from one that is merely
// slow. Getting that distinction wrong is expensive in both
// directions, and both directions have already happened here —
//
//   - too tight, and a working answer is thrown away mid-stream. A
//     2-minute budget, calibrated against an idle endpoint whose
//     largest gap was 0.7s, discarded 192 of 194 answers from a busy
//     one.
//   - too loose, and a dead socket hangs the command indefinitely.
//
// The queue wait before the first byte and a gap after it are
// different phenomena with different legitimate durations, which is
// why there are two budgets. These tests pin that split, because it
// is not obvious from reading the struct and a future simplification
// back to one timer would silently reintroduce the failure.

// blockingReader yields data, then blocks until released.
type blockingReader struct {
	data     string
	released chan struct{}
	once     sync.Once
	pos      int
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if b.pos < len(b.data) {
		n := copy(p, b.data[b.pos:])
		b.pos += n
		return n, nil
	}
	<-b.released // block until the test says otherwise
	return 0, io.EOF
}

func (b *blockingReader) release() { b.once.Do(func() { close(b.released) }) }

// TestStallReaderFirstByteBudgetIsSeparate is the regression for the
// bug that lost 192 of 194 answers: a request can sit queued, silent,
// for far longer than any reasonable mid-stream gap. The first-byte
// budget must govern that wait — not the (much shorter) stall budget.
func TestStallReaderFirstByteBudgetIsSeparate(t *testing.T) {
	br := &blockingReader{released: make(chan struct{})}
	defer br.release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Generous first-byte budget, tiny stall budget. Nothing has
	// arrived yet, so the generous one must apply.
	sr := newStallReader(br, 2*time.Second, 20*time.Millisecond, cancel)
	defer sr.Stop()

	if got := sr.Budget(); got != 2*time.Second {
		t.Fatalf("before any byte the first-byte budget must apply: got %s", got)
	}

	// The Read must be IN FLIGHT for this to test anything: the budget
	// is chosen inside Read, so a test that only inspects the reader
	// without reading asserts nothing. (An earlier version of this
	// test did exactly that, and passed with the fix reverted.)
	done := make(chan struct{})
	go func() { buf := make([]byte, 16); _, _ = sr.Read(buf); close(done) }()

	// Well past the stall budget, nowhere near the first-byte budget.
	time.Sleep(300 * time.Millisecond)
	if sr.Stalled() {
		t.Error("watchdog fired on the SHORT budget while waiting for the first byte — " +
			"this is the failure that discarded 192 of 194 answers from a queued endpoint")
	}
	if ctx.Err() != nil {
		t.Error("context cancelled during a legitimate queue wait")
	}
	br.release()
	<-done
}

// TestStallReaderSwitchesToStallBudgetAfterFirstByte pins the other
// half: once bytes flow, the server is generating for us
// specifically, so silence means death and the tight budget applies.
func TestStallReaderSwitchesToStallBudgetAfterFirstByte(t *testing.T) {
	br := &blockingReader{data: "data: hello\n\n", released: make(chan struct{})}
	defer br.release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sr := newStallReader(br, time.Hour, 100*time.Millisecond, cancel)
	defer sr.Stop()

	buf := make([]byte, 64)
	if n, err := sr.Read(buf); err != nil || n == 0 {
		t.Fatalf("first read: n=%d err=%v", n, err)
	}
	if got := sr.Budget(); got != 100*time.Millisecond {
		t.Fatalf("after the first byte the stall budget must apply: got %s", got)
	}

	// The next Read blocks. The tight budget should fire despite the
	// first-byte budget being an hour.
	done := make(chan struct{})
	go func() { _, _ = sr.Read(buf); close(done) }()

	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("stall budget did not fire after the stream went silent mid-answer")
	}
	if !sr.Stalled() {
		t.Error("Stalled() must report the watchdog fired, so the error can name the cause")
	}
	br.release()
	<-done
}

// TestStallReaderQuietWhileDataFlows: a stream that keeps delivering
// must never be cut off, however long it runs in total. Total
// duration is the caller's ctx to own, not the watchdog's.
func TestStallReaderQuietWhileDataFlows(t *testing.T) {
	pr, pw := io.Pipe()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Stall budget much shorter than the total run.
	sr := newStallReader(pr, time.Second, 150*time.Millisecond, cancel)
	defer sr.Stop()

	go func() {
		for i := 0; i < 12; i++ { // ~600ms total, 50ms apart
			_, _ = pw.Write([]byte("data: tick\n\n"))
			time.Sleep(50 * time.Millisecond)
		}
		_ = pw.Close()
	}()

	n, err := io.Copy(io.Discard, sr)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("steady stream was interrupted: %v", err)
	}
	if n == 0 {
		t.Fatal("no data read")
	}
	if sr.Stalled() {
		t.Error("watchdog fired on a stream that never stopped delivering")
	}
}

// TestStallReaderTimerFiresDuringRead models the ONLY concurrency
// that exists in production: one goroutine reading (bufio.Scanner
// pulls sequentially) while the timer's AfterFunc callback fires on
// another, writing `fired` and calling cancel — racing the reader's
// write to `started`.
//
// An earlier draft of this test called Read from four goroutines at
// once and the detector duly reported a race — inside
// strings.Reader, which is not safe for concurrent use. That was the
// test's bug, not the reader's: nothing in the provider ever reads
// concurrently, so the scenario could not occur and the finding was
// noise. Modelling the real shape is what makes the detector's
// verdict mean something.
func TestStallReaderTimerFiresDuringRead(t *testing.T) {
	for i := 0; i < 50; i++ {
		br := &blockingReader{data: "data: x\n\n", released: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())

		// Budgets short enough that the timer fires while the second
		// Read is blocked.
		sr := newStallReader(br, time.Millisecond, time.Millisecond, cancel)

		buf := make([]byte, 16)
		_, _ = sr.Read(buf) // consumes the data, flips `started`

		done := make(chan struct{})
		go func() { _, _ = sr.Read(buf); close(done) }() // blocks; timer fires under it

		<-ctx.Done()
		_ = sr.Stalled()
		_ = sr.Budget()
		br.release()
		<-done
		sr.Stop()
		cancel()
	}
}
