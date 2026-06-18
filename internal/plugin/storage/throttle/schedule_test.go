package throttle_test

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/throttle"
)

// TestSchedule_ParseSimple: a single-window expression parses into
// the expected Window + DefaultBPS=0.
func TestSchedule_ParseSimple(t *testing.T) {
	s, err := throttle.ParseSchedule("09:00-18:00=50mbps")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Windows) != 1 {
		t.Fatalf("Windows=%d, want 1", len(s.Windows))
	}
	w := s.Windows[0]
	if w.StartMinute != 9*60 || w.EndMinute != 18*60 {
		t.Errorf("times = %d-%d, want 540-1080", w.StartMinute, w.EndMinute)
	}
	// 50mbps = 50_000_000/8 = 6_250_000 bytes/sec.
	if w.BPS != 6_250_000 {
		t.Errorf("BPS=%d, want 6_250_000", w.BPS)
	}
	if len(w.Days) != 0 {
		t.Errorf("days should be empty (every day); got %v", w.Days)
	}
}

// TestSchedule_ParseWithDays: weekday range parses into the expected
// 5-element list.
func TestSchedule_ParseWithDays(t *testing.T) {
	s, err := throttle.ParseSchedule("Mon-Fri,09:00-18:00=50mbps")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	w := s.Windows[0]
	want := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}
	if len(w.Days) != len(want) {
		t.Fatalf("days = %v, want %v", w.Days, want)
	}
	for i, d := range want {
		if w.Days[i] != d {
			t.Errorf("days[%d] = %v, want %v", i, w.Days[i], d)
		}
	}
}

// TestSchedule_ParseMultiWindow: semicolon-separated windows are
// preserved in declaration order.
func TestSchedule_ParseMultiWindow(t *testing.T) {
	s, err := throttle.ParseSchedule(
		"Mon-Fri,09:00-18:00=50mbps;Sat-Sun,00:00-23:59=200mbps")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Windows) != 2 {
		t.Fatalf("Windows=%d, want 2", len(s.Windows))
	}
	if s.Windows[0].BPS != 6_250_000 {
		t.Errorf("first BPS = %d", s.Windows[0].BPS)
	}
	// 200mbps = 200_000_000/8 = 25_000_000 bytes/sec.
	if s.Windows[1].BPS != 25_000_000 {
		t.Errorf("second BPS = %d", s.Windows[1].BPS)
	}
}

// TestSchedule_ParseRateUnits: kbps and 0 (unbounded) parse correctly.
func TestSchedule_ParseRateUnits(t *testing.T) {
	cases := []struct {
		expr    string
		wantBPS int64
	}{
		{"00:00-23:59=500kbps", 62_500},  // 500_000/8
		{"00:00-23:59=0", 0},             // explicit unbounded
		{"00:00-23:59=1.5mbps", 187_500}, // 1.5_000_000/8
	}
	for _, c := range cases {
		s, err := throttle.ParseSchedule(c.expr)
		if err != nil {
			t.Errorf("parse %q: %v", c.expr, err)
			continue
		}
		if got := s.Windows[0].BPS; got != c.wantBPS {
			t.Errorf("%q: BPS=%d, want %d", c.expr, got, c.wantBPS)
		}
	}
}

// TestSchedule_ParseErrors: bad inputs surface a structured error.
func TestSchedule_ParseErrors(t *testing.T) {
	bad := []string{
		"",                           // empty
		"=50mbps",                    // missing times
		"09:00-18:00",                // missing rate
		"NotADay,09:00-18:00=50mbps", // bad weekday
		"09:00-18:00=50",             // rate missing unit
		"25:00-26:00=50mbps",         // bad hour
		"09:00-18:00=-1mbps",         // negative rate
		"09:60-18:00=50mbps",         // bad minute
	}
	for _, expr := range bad {
		if _, err := throttle.ParseSchedule(expr); err == nil {
			t.Errorf("ParseSchedule(%q) should error", expr)
		}
	}
}

// TestSchedule_BPSAt_NonWrapping: a 09:00-18:00 window applies in
// the middle of the day and not before/after.
func TestSchedule_BPSAt_NonWrapping(t *testing.T) {
	s, _ := throttle.ParseSchedule("09:00-18:00=50mbps")
	at := func(h, m int) time.Time {
		return time.Date(2026, 4, 30, h, m, 0, 0, time.UTC)
	}
	cases := []struct {
		t       time.Time
		wantBPS int64
	}{
		{at(8, 30), 0},          // before window
		{at(9, 0), 6_250_000},   // window start (inclusive)
		{at(13, 0), 6_250_000},  // mid-day
		{at(17, 59), 6_250_000}, // just before end
		{at(18, 0), 0},          // window end (exclusive)
		{at(23, 0), 0},          // after window
	}
	for _, c := range cases {
		if got := s.BPSAt(c.t); got != c.wantBPS {
			t.Errorf("BPSAt(%s) = %d, want %d", c.t.Format("15:04"), got, c.wantBPS)
		}
	}
}

// TestSchedule_BPSAt_WrappingWindow: a 22:00-06:00 window covers
// late evening AND early morning.
func TestSchedule_BPSAt_WrappingWindow(t *testing.T) {
	s, _ := throttle.ParseSchedule("22:00-06:00=10mbps")
	at := func(h, m int) time.Time {
		return time.Date(2026, 4, 30, h, m, 0, 0, time.UTC)
	}
	cases := []struct {
		hour    int
		min     int
		wantBPS int64
	}{
		{21, 59, 0},
		{22, 0, 1_250_000}, // 10mbps → 1.25MB/s
		{23, 30, 1_250_000},
		{0, 0, 1_250_000},
		{5, 59, 1_250_000},
		{6, 0, 0},
		{12, 0, 0},
	}
	for _, c := range cases {
		if got := s.BPSAt(at(c.hour, c.min)); got != c.wantBPS {
			t.Errorf("BPSAt(%02d:%02d) = %d, want %d",
				c.hour, c.min, got, c.wantBPS)
		}
	}
}

// TestSchedule_BPSAt_DayFilter: a Mon-Fri window only matches on
// weekdays.
func TestSchedule_BPSAt_DayFilter(t *testing.T) {
	s, _ := throttle.ParseSchedule("Mon-Fri,09:00-18:00=50mbps")
	// 2026-04-30 is a Thursday; 2026-05-02 is a Saturday.
	weekday := time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC)
	weekend := time.Date(2026, 5, 2, 13, 0, 0, 0, time.UTC)
	if got := s.BPSAt(weekday); got != 6_250_000 {
		t.Errorf("weekday BPS=%d, want 6_250_000", got)
	}
	if got := s.BPSAt(weekend); got != 0 {
		t.Errorf("weekend BPS=%d, want 0 (no match)", got)
	}
}

// TestSchedule_BPSAt_FirstMatchWins: with multiple overlapping
// windows, the first declared wins.
func TestSchedule_BPSAt_FirstMatchWins(t *testing.T) {
	s, _ := throttle.ParseSchedule(
		"00:00-23:59=10mbps;09:00-18:00=50mbps")
	at := time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC)
	// Both windows match; first (10mbps) wins.
	if got := s.BPSAt(at); got != 1_250_000 {
		t.Errorf("BPSAt = %d, want 1_250_000 (first-match)", got)
	}
}

// TestSchedule_String_RoundTrip: a parsed schedule renders back to
// a parseable form (equivalent semantics).
func TestSchedule_String_RoundTrip(t *testing.T) {
	cases := []string{
		"09:00-18:00=50mbps",
		"Mon-Fri,09:00-18:00=50mbps",
		"Mon-Fri,09:00-18:00=50mbps;Sat-Sun,00:00-23:59=200mbps",
	}
	for _, expr := range cases {
		s, err := throttle.ParseSchedule(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		rendered := s.String()
		// Re-parse the rendered form.
		s2, err := throttle.ParseSchedule(rendered)
		if err != nil {
			t.Errorf("re-parse %q from %q failed: %v", rendered, expr, err)
			continue
		}
		if !equivalentSchedule(s, s2) {
			t.Errorf("round-trip mismatch:\n  original: %q\n  rendered: %q", expr, rendered)
		}
	}
}

func equivalentSchedule(a, b *throttle.Schedule) bool {
	if len(a.Windows) != len(b.Windows) {
		return false
	}
	for i := range a.Windows {
		w1, w2 := a.Windows[i], b.Windows[i]
		if w1.StartMinute != w2.StartMinute || w1.EndMinute != w2.EndMinute {
			return false
		}
		if w1.BPS != w2.BPS {
			return false
		}
		if len(w1.Days) != len(w2.Days) {
			return false
		}
		for j := range w1.Days {
			if w1.Days[j] != w2.Days[j] {
				return false
			}
		}
	}
	return true
}

// TestThrottle_ScheduleDrivesRateAtPut: a Put inside a capped window
// throttles; a Put outside (unbounded) does not. We use a fake
// clock to flip between them deterministically.
func TestThrottle_ScheduleDrivesRateAtPut(t *testing.T) {
	clock := newFakeClock()
	// Capped 09:00-18:00, unbounded otherwise.
	sched, err := throttle.ParseSchedule("09:00-18:00=50kbps")
	if err != nil {
		t.Fatal(err)
	}
	// 50kbps = 6_250 bytes/sec. Burst sized to 6KB so we don't
	// silently absorb the test payload.
	tr := throttle.New(fsBackend(t), 0,
		throttle.WithSchedule(sched),
		throttle.WithBurst(6*1024),
		throttle.WithChunkSize(1024),
		throttle.WithClock(clock.Now, clock.Sleep))

	// Step 1: position the clock OUTSIDE the window (06:00 UTC).
	// Put a 100KB body — should not throttle (rate=0).
	clock.setNow(time.Date(2026, 4, 30, 6, 0, 0, 0, time.UTC))
	body := bytes.Repeat([]byte{1}, 100*1024)
	if _, err := tr.Put(context.Background(), "k.outside",
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("outside-window Put: %v", err)
	}
	if got := clock.totalSleep(); got > 100*time.Millisecond {
		t.Errorf("outside-window Put should not throttle; slept %s", got)
	}

	// Step 2: move clock INSIDE the window (13:00 UTC).
	// Put another 100KB body — should throttle to ~50kbps.
	beforeIn := clock.totalSleep()
	clock.setNow(time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC))
	if _, err := tr.Put(context.Background(), "k.inside",
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("inside-window Put: %v", err)
	}
	insideSleep := clock.totalSleep() - beforeIn
	// 100KB - 6KB burst = 94KB at 6.25KB/s ≈ 15s. Allow generous
	// tolerance for chunk granularity.
	if insideSleep < 13*time.Second || insideSleep > 18*time.Second {
		t.Errorf("inside-window Put should throttle ~15s; slept %s", insideSleep)
	}
}

// TestThrottle_ScheduleNeverWraps_OutsideAlways: with a schedule
// that's currently unbounded, the Throttle is NOT
// unconditionallyUnbounded — the wrapping reader is in place so
// future capped windows take effect without a fresh Throttle.
//
// This is observable by using the throttle's `Region()` (a thin
// pass-through) — we just care that the wrapper is functional and
// transparent right now.
func TestThrottle_ScheduleNeverWraps_OutsideAlways(t *testing.T) {
	clock := newFakeClock()
	clock.setNow(time.Date(2026, 4, 30, 6, 0, 0, 0, time.UTC)) // outside
	sched, _ := throttle.ParseSchedule("09:00-18:00=50kbps")
	tr := throttle.New(fsBackend(t), 0,
		throttle.WithSchedule(sched),
		throttle.WithClock(clock.Now, clock.Sleep))

	// A small Put outside the window should pass through cleanly.
	body := []byte("schedule outside the window")
	if _, err := tr.Put(context.Background(), "k",
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Errorf("Put: %v", err)
	}
	if clock.totalSleep() > 100*time.Millisecond {
		t.Errorf("outside-window Put should not throttle; slept %s",
			clock.totalSleep())
	}
}

// TestThrottle_ScheduleConcurrent: race-detector check that the
// schedule integration is goroutine-safe.
func TestThrottle_ScheduleConcurrent(t *testing.T) {
	clock := newFakeClock()
	clock.setNow(time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC))
	sched, _ := throttle.ParseSchedule("09:00-18:00=10mbps")
	tr := throttle.New(fsBackend(t), 0,
		throttle.WithSchedule(sched),
		throttle.WithClock(clock.Now, clock.Sleep))

	const N = 16
	var done atomic.Int32
	for i := 0; i < N; i++ {
		go func(i int) {
			defer done.Add(1)
			body := []byte(strings.Repeat("x", 100))
			_, _ = tr.Put(context.Background(),
				"k-"+timeStampHex(i),
				bytes.NewReader(body),
				storage.PutOptions{ContentLength: int64(len(body))})
		}(i)
	}
	for done.Load() < N {
		time.Sleep(10 * time.Millisecond)
	}
}

func timeStampHex(i int) string {
	const hex = "0123456789abcdef"
	var buf [4]byte
	for j := 0; j < 4; j++ {
		buf[j] = hex[(i>>(j*4))&0xf]
	}
	return string(buf[:])
}
