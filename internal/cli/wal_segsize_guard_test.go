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
// probeSegmentSize deliberately falls back to the 16 MiB default when
// its probe query fails, so a flaky pre-flight never blocks a valid
// stream. That is a reasonable trade for a CONNECT failure (streaming
// would fail anyway), but on a cluster built with
// `initdb --wal-segsize 64MB` it silently produces exactly the
// mis-named segments that the same function's invalid-value branch
// refuses to produce, saying "Refusing to stream rather than mis-name
// segments". This guard is the check that notices, whatever the wrong
// size came from — a bad probe or a --wal-segment-size flag that does
// not match the cluster.

import (
	"bytes"
	"context"
	"errors"
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
