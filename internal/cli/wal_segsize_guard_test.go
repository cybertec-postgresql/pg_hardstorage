package cli

// Streaming into a deployment with the wrong wal_segment_size renames
// its WAL.
//
// Segment size determines segment NAMES as well as their length — PG
// packs 4 GiB / size segments per log-id — so mixing two sizes in one
// lineage produces names that collide or skip, and point-in-time
// recovery breaks. It is the same class of damage the system_identifier
// guard beside it prevents.
//
// The live size is always probed from the server now (probeSegmentSize
// fails rather than assume one), so a mismatch here means the ARCHIVE
// is at the wrong size — WAL an older build chopped at an assumed 16 MiB
// when its probe could not connect — or a different cluster admitted
// under --allow-system-identifier-change. Either way, streaming on would
// mix two sizes in one lineage.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// plantSegmentManifestSized is plantSegmentManifest with an explicit
// wal_segment_size, so a fixture can model a non-default cluster.
func plantSegmentManifestSized(t *testing.T, sp storage.StoragePlugin, deployment, sysID string, tli uint32, seg uint64, segSize int64) {
	t.Helper()
	name := walsink.SegmentFileName(tli, seg, segSize)
	m := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       deployment,
		SystemIdentifier: sysID,
		Timeline:         tli,
		SegmentNumber:    seg,
		SegmentName:      name,
		StartLSN:         "0/1000000",
		EndLSN:           "0/2000000",
		SegmentSize:      segSize,
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatalf("marshal segment manifest: %v", err)
	}
	key := walsink.SegmentPath(deployment, tli, name)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatalf("plant segment manifest: %v", err)
	}
}

func segGuardRepo(t *testing.T) (context.Context, storage.StoragePlugin) {
	t.Helper()
	ctx := context.Background()
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatalf("repo open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return ctx, sp
}

const (
	segGuard16 = 16 << 20
	segGuard64 = 64 << 20
)

// The exact shape of the probeSegmentSize fallback bug: the cluster is
// 64 MiB, the probe failed, and we are about to stream at the 16 MiB
// default.
func TestGuardSegmentSize_RefusesADifferentSizeThanTheArchive(t *testing.T) {
	ctx, sp := segGuardRepo(t)
	plantSegmentManifestSized(t, sp, "db1", "7000000000000000001", 1, 5, segGuard64)

	err := guardSegmentSize(ctx, sp, "wal stream", "db1", segGuard16)
	if err == nil {
		t.Fatal("streaming at 16 MiB into an archive written at 64 MiB was allowed.\n\n" +
			"Segment size determines segment NAMES, so the new segments would collide " +
			"with or skip past the existing ones and PITR across the boundary breaks.")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "preflight.wal_segment_size_changed" {
		t.Fatalf("expected preflight.wal_segment_size_changed; got %v", err)
	}
	// Both sizes must appear, or the operator cannot tell which is wrong.
	for _, want := range []string{"16MB", "64MB", "db1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
	if err.(*output.Error).Suggestion == nil {
		t.Error("no suggestion: the operator is told what is wrong but not what to do")
	}
}

// The remedy must be one the operator can apply. guardSegmentSize runs
// only for `wal stream`, which has no --wal-segment-size flag (that is
// `wal push`'s); telling the operator to pass it sends them to an
// "unknown flag" error in the middle of an incident.
func TestGuardSegmentSize_RemedyNamesNoNonexistentFlag(t *testing.T) {
	ctx, sp := segGuardRepo(t)
	plantSegmentManifestSized(t, sp, "db1", "7000000000000000001", 1, 5, segGuard64)

	err := guardSegmentSize(ctx, sp, "wal stream", "db1", segGuard16)
	var oerr *output.Error
	if !errors.As(err, &oerr) {
		t.Fatalf("expected an *output.Error; got %v", err)
	}
	text := oerr.Message
	if oerr.Suggestion != nil {
		text += "\n" + oerr.Suggestion.Human + "\n" + oerr.Suggestion.Command
	}
	if strings.Contains(text, "--wal-segment-size") {
		t.Errorf("`wal stream` has no --wal-segment-size flag, but the refusal tells the "+
			"operator to pass one:\n%s", text)
	}
}

func TestGuardSegmentSize_MatchingSizePasses(t *testing.T) {
	ctx, sp := segGuardRepo(t)
	plantSegmentManifestSized(t, sp, "db1", "7000000000000000001", 1, 5, segGuard64)
	if err := guardSegmentSize(ctx, sp, "wal stream", "db1", segGuard64); err != nil {
		t.Errorf("a matching segment size must pass: %v", err)
	}
}

// Fail-open cases: nothing to contradict, so nothing to refuse. Same
// posture as every other pre-flight on this path — a guard that blocks
// a first-ever stream would be worse than the gap it closes.
func TestGuardSegmentSize_FailsOpenWithNothingArchived(t *testing.T) {
	ctx, sp := segGuardRepo(t)
	if err := guardSegmentSize(ctx, sp, "wal stream", "fresh-deployment", segGuard64); err != nil {
		t.Errorf("a deployment with no archived WAL must not be blocked: %v", err)
	}
}

func TestGuardSegmentSize_InvalidLiveSizeIsLeftToTheCaller(t *testing.T) {
	ctx, sp := segGuardRepo(t)
	plantSegmentManifestSized(t, sp, "db1", "7000000000000000001", 1, 5, segGuard16)
	// 3 MiB is not a power of two in range; probeSegmentSize already
	// refuses it with a better message, so this guard must not shadow
	// that with a confusing "size changed" complaint.
	if err := guardSegmentSize(ctx, sp, "wal stream", "db1", 3<<20); err != nil {
		t.Errorf("an invalid live size is the caller's refusal to make: %v", err)
	}
}

// A different DEPLOYMENT's archive must not be consulted.
func TestGuardSegmentSize_ScopedToTheDeployment(t *testing.T) {
	ctx, sp := segGuardRepo(t)
	plantSegmentManifestSized(t, sp, "other", "7000000000000000001", 1, 5, segGuard64)
	if err := guardSegmentSize(ctx, sp, "wal stream", "db1", segGuard16); err != nil {
		t.Errorf("another deployment's segment size leaked into db1's guard: %v", err)
	}
}

// The refactor that gave both guards one shared walk must not have
// changed what the system-identifier guard sees.
func TestFirstSegmentManifestWhere_PredicateSelectsIndependently(t *testing.T) {
	ctx, sp := segGuardRepo(t)
	plantSegmentManifestSized(t, sp, "db1", "7000000000000000001", 1, 5, segGuard64)

	sys, found, err := deploymentRecordedSysID(ctx, sp, "db1")
	if err != nil || !found || sys != "7000000000000000001" {
		t.Errorf("sysid lookup = (%q,%v,%v)", sys, found, err)
	}
	size, found, err := deploymentRecordedSegSize(ctx, sp, "db1")
	if err != nil || !found || size != segGuard64 {
		t.Errorf("segsize lookup = (%d,%v,%v), want %d", size, found, err, int64(segGuard64))
	}
}

// A guard that is never called is worth nothing, and both WAL-stream
// source guards are exactly that shape: pure functions whose unit tests
// pass just as happily when the call site is deleted.
//
// The authoritative call site is verifyStreamSource, which streamAttempt
// runs on EVERY attempt: a check made once at startup is skipped when
// PostgreSQL is unreachable then, and the reconnect loop reaches whatever
// cluster the DSN resolves to later. There is no unit-testable seam (the
// path needs a live PostgreSQL), so this asserts the wiring at the source
// level; TestIntegration_WalStream_SysIDGuardSurvivesUnreachableStartup
// and ..._SegSizeProbedOnReconnect drive it end to end.
func TestWalStream_CallsItsPreflightGuards(t *testing.T) {
	src, err := os.ReadFile("wal.go")
	if err != nil {
		t.Fatalf("read wal.go: %v", err)
	}
	body := string(src)
	fnBody := func(name string) string {
		start := strings.Index(body, "func "+name+"(")
		if start < 0 {
			t.Fatalf("%s not found; this guard needs updating, not deleting", name)
		}
		end := strings.Index(body[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("could not delimit %s", name)
		}
		return body[start : start+end]
	}

	verify := fnBody("verifyStreamSource")
	for _, guard := range []string{"guardSystemIdentifier(", "checkSysIDContinuity(", "probeSegmentSize(", "guardSegmentSize("} {
		if !strings.Contains(verify, guard) {
			t.Errorf("verifyStreamSource does not call %s\n\n"+
				"Streaming then proceeds without that check on every reconnect: a foreign "+
				"cluster's WAL, or a wrong wal_segment_size, enters the lineage unchallenged.", guard)
		}
	}

	attempt := fnBody("streamAttempt")
	vIdx := strings.Index(attempt, "verifyStreamSource(")
	if vIdx < 0 {
		t.Fatal("streamAttempt does not call verifyStreamSource: the source guards run at most " +
			"once, at startup, and are skipped for good when PostgreSQL is unreachable then")
	}
	// ...and before anything is written or streamed for the cluster.
	for _, later := range []string{"captureStreamTimelineHistory(", "ensureSlot(", "walsink.New(", "replication.Stream("} {
		if i := strings.Index(attempt, later); i >= 0 && i < vIdx {
			t.Errorf("streamAttempt calls %s before verifyStreamSource; the guards must run first", later)
		}
	}
}
