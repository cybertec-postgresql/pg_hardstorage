package cli

import (
	"testing"
	"time"
)

// TestBuildRecovery_UsesProvidedTargetTimeVerbatim pins the fix for
// the PITR time-target drift bug: runRestore parses --to ONCE and
// threads the resulting instant into buildRecovery, which must arm
// recovery_target_time as exactly that instant — never re-deriving it
// from opts.toTime.
//
// The regression this guards: buildRecovery used to call
// naturaltime.Parse(opts.toTime, time.Now()) itself while the seed
// resolver parsed the same string against time.Now().UTC(). For a
// bare-clock "today/yesterday HH:MM" expression naturaltime honours
// the reference's zone, so on a non-UTC host the seed backup was
// selected for a different instant than the target written to
// postgresql.auto.conf. We feed buildRecovery a sentinel instant that
// could not be the parse of opts.toTime and assert it comes back out
// untouched.
func TestBuildRecovery_UsesProvidedTargetTimeVerbatim(t *testing.T) {
	// A fixed instant deliberately unrelated to "today 09:00": if
	// buildRecovery re-parsed opts.toTime it would land on today's
	// date, never on 2026-04-27.
	want := time.Date(2026, 4, 27, 9, 42, 0, 0,
		time.FixedZone("oddball", 5*3600+30*60)).UTC()

	opts := restoreOpts{
		deployment: "db1",
		repoURL:    "file:///tmp/repo",
		toTime:     "today 09:00", // raw string must NOT be re-parsed
		toAction:   "pause",
		toTimeline: "latest",
	}

	r, err := buildRecovery(opts, want)
	if err != nil {
		t.Fatalf("buildRecovery: %v", err)
	}
	if r == nil {
		t.Fatal("buildRecovery returned nil for a time target")
	}
	if !r.TargetTime.Equal(want) {
		t.Errorf("TargetTime = %v; want %v — buildRecovery must arm the "+
			"instant the caller already parsed, not re-derive it from opts.toTime",
			r.TargetTime, want)
	}
	if r.TargetLSN != "" || r.TargetName != "" {
		t.Errorf("only TargetTime should be set; got LSN=%q name=%q",
			r.TargetLSN, r.TargetName)
	}
}

// TestBuildRecovery_TimeTargetIgnoresUnparseableString is the
// companion guard: because buildRecovery no longer parses opts.toTime,
// a toTime string that naturaltime could never parse must NOT cause an
// error here — the caller already validated and parsed it. This proves
// the parse truly moved out of buildRecovery (a re-introduced parse
// would reject "garbage" and fail this test).
func TestBuildRecovery_TimeTargetIgnoresUnparseableString(t *testing.T) {
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	opts := restoreOpts{
		deployment: "db1",
		repoURL:    "file:///tmp/repo",
		toTime:     "this is not a time", // never reaches a parser
		toAction:   "pause",
		toTimeline: "latest",
	}
	r, err := buildRecovery(opts, want)
	if err != nil {
		t.Fatalf("buildRecovery should not parse opts.toTime: %v", err)
	}
	if !r.TargetTime.Equal(want) {
		t.Errorf("TargetTime = %v; want %v", r.TargetTime, want)
	}
}

// TestBuildRecovery_NonTimeTargetsUnaffected confirms the targetTime
// parameter is inert for LSN/name targets and the no-target case.
func TestBuildRecovery_NonTimeTargetsUnaffected(t *testing.T) {
	base := restoreOpts{
		deployment: "db1",
		repoURL:    "file:///tmp/repo",
		toAction:   "pause",
		toTimeline: "latest",
	}
	// A bogus targetTime must be ignored when no time target is set.
	bogus := time.Date(1999, 9, 9, 9, 9, 9, 0, time.UTC)

	t.Run("lsn target", func(t *testing.T) {
		o := base
		o.toLSN = "0/3000028"
		r, err := buildRecovery(o, bogus)
		if err != nil {
			t.Fatal(err)
		}
		if r.TargetLSN != "0/3000028" {
			t.Errorf("TargetLSN = %q", r.TargetLSN)
		}
		if !r.TargetTime.IsZero() {
			t.Errorf("TargetTime should be zero for an LSN target; got %v", r.TargetTime)
		}
	})

	t.Run("name target", func(t *testing.T) {
		o := base
		o.toName = "before-the-incident"
		r, err := buildRecovery(o, bogus)
		if err != nil {
			t.Fatal(err)
		}
		if r.TargetName != "before-the-incident" {
			t.Errorf("TargetName = %q", r.TargetName)
		}
		if !r.TargetTime.IsZero() {
			t.Errorf("TargetTime should be zero for a name target; got %v", r.TargetTime)
		}
	})

	t.Run("no target", func(t *testing.T) {
		r, err := buildRecovery(base, bogus)
		if err != nil {
			t.Fatal(err)
		}
		if r != nil {
			t.Errorf("buildRecovery should return nil when no PITR target is set; got %+v", r)
		}
	})
}
