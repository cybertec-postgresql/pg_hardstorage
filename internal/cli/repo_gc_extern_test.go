package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func TestRepoGC_RequiresURL(t *testing.T) {
	_, _, exit := runCmd(t, "repo", "gc", "--output", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse(2)", exit)
	}
}

func TestRepoGC_DryRun_ListsButDoesntDelete(t *testing.T) {
	repoURL := initRepoForTest(t)

	// Drop a chunk into the CAS that no manifest references.
	_, sp, _ := repo.Open(context.Background(), repoURL)
	cas := casdefault.New(sp)
	info, err := cas.PutChunk(context.Background(), []byte("orphaned-by-design"))
	if err != nil {
		t.Fatal(err)
	}
	sp.Close()

	out, _, exit := runCmd(t,
		"repo", "gc", repoURL,
		// Disable the chunk-age floor: this test writes the orphan
		// milliseconds ago, and the default 24h floor (which guards
		// in-flight backups) would otherwise protect it.
		"--min-chunk-age", "0",
		"--output", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		`"dry_run": true`,
		`"orphan_count": 1`,
		`"bytes_reclaimable":`,
		info.Hash.String(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// Confirm the chunk is still there.
	_, sp2, _ := repo.Open(context.Background(), repoURL)
	defer sp2.Close()
	if has, _ := casdefault.New(sp2).HasChunk(context.Background(), info.Hash); !has {
		t.Errorf("dry-run unexpectedly deleted the chunk")
	}
}

func TestRepoGC_Apply_DeletesOrphans(t *testing.T) {
	repoURL := initRepoForTest(t)

	_, sp, _ := repo.Open(context.Background(), repoURL)
	cas := casdefault.New(sp)
	info, _ := cas.PutChunk(context.Background(), []byte("doomed"))
	sp.Close()

	out, _, exit := runCmd(t,
		"repo", "gc", repoURL, "--apply",
		// See TestRepoGC_DryRun: disable the age floor so the
		// just-written orphan is eligible.
		"--min-chunk-age", "0",
		"--output", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	for _, want := range []string{
		`"applied": 1`,
		`"bytes_reclaimed":`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// Now gone.
	_, sp2, _ := repo.Open(context.Background(), repoURL)
	defer sp2.Close()
	if has, _ := casdefault.New(sp2).HasChunk(context.Background(), info.Hash); has {
		t.Errorf("--apply should have deleted the orphan chunk")
	}
}

// TestRepoGC_ChunkAgeFloor_ProtectsFreshChunk: by default a
// just-written unreferenced chunk is NOT reaped — it could belong to
// an in-flight backup that hasn't committed its manifest yet.
func TestRepoGC_ChunkAgeFloor_ProtectsFreshChunk(t *testing.T) {
	repoURL := initRepoForTest(t)

	_, sp, _ := repo.Open(context.Background(), repoURL)
	info, _ := casdefault.New(sp).PutChunk(context.Background(), []byte("fresh-in-flight"))
	sp.Close()

	out, _, exit := runCmd(t,
		"repo", "gc", repoURL, "--apply",
		"--output", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	if !strings.Contains(out, `"orphan_count": 0`) {
		t.Errorf("default age floor should protect the fresh chunk; out:\n%s", out)
	}

	// The chunk must survive.
	_, sp2, _ := repo.Open(context.Background(), repoURL)
	defer sp2.Close()
	if has, _ := casdefault.New(sp2).HasChunk(context.Background(), info.Hash); !has {
		t.Errorf("--apply reaped a fresh chunk despite the default age floor")
	}
}

// TestRepoGC_Apply_SweepsStaleTempManifest: a staging file left by an
// interrupted commit is reported and (with the floor disabled, since
// the test writes it fresh) removed by --apply.
func TestRepoGC_Apply_SweepsStaleTempManifest(t *testing.T) {
	repoURL := initRepoForTest(t)

	_, sp, _ := repo.Open(context.Background(), repoURL)
	tmpKey := "manifests/db1/backups/b1/manifest.json.tmp.deadbeefcafe"
	body := `{"files":[]}`
	if _, err := sp.Put(context.Background(), tmpKey, strings.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	sp.Close()

	out, _, exit := runCmd(t,
		"repo", "gc", repoURL, "--apply",
		"--min-chunk-age", "0",
		"--output", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	for _, want := range []string{
		`"stale_temp_count": 1`,
		`"stale_temp_deleted": 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// The staging file must be gone.
	_, sp2, _ := repo.Open(context.Background(), repoURL)
	defer sp2.Close()
	if _, err := sp2.Stat(context.Background(), tmpKey); err == nil {
		t.Errorf("--apply should have removed the stale staging file")
	}
}

func TestRepoGC_AcceptsRepoFlag(t *testing.T) {
	// Same as the positional form, but via --repo. Both shapes must
	// reach runRepoGC with the same URL.
	repoURL := initRepoForTest(t)
	_, _, exit := runCmd(t,
		"repo", "gc", "--repo", repoURL,
		"--output", "json",
	)
	if exit != int(output.ExitOK) {
		t.Errorf("--repo flag form should work; exit = %d", exit)
	}
}

func TestRepoGC_RejectsConflictingRepoSources(t *testing.T) {
	_, _, exit := runCmd(t,
		"repo", "gc", "file:///foo",
		"--repo", "file:///bar",
		"--output", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("conflicting positional + --repo should exit ExitMisuse(2); got %d", exit)
	}
}

// TestRepoGC_Apply_WarnsWhenFloorDisabled: hardening for data-loss
// path #3. `--apply` with a disabled chunk-age floor (--min-chunk-age
// 0, which the CLI maps to the negative "disable" sentinel) must emit
// a SeverityWarning safety_floor_disabled event, so an operator who
// disarmed the in-flight-backup / undelete guard sees it loudly
// instead of silently reaping young chunks.
func TestRepoGC_Apply_WarnsWhenFloorDisabled(t *testing.T) {
	repoURL := initRepoForTest(t)
	out, errb, exit := runCmd(t,
		"repo", "gc", repoURL, "--apply", "--min-chunk-age", "0", "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", exit, out, errb)
	}
	all := out + errb
	for _, want := range []string{"safety_floor_disabled", "min-chunk-age"} {
		if !strings.Contains(all, want) {
			t.Errorf("expected %q in gc output:\nstdout=%s\nstderr=%s", want, out, errb)
		}
	}
}

// TestRepoGC_Apply_NoWarnWithDefaultFloors: the default (24h) floors
// must NOT trigger the warning — only an explicit opt-out does.
func TestRepoGC_Apply_NoWarnWithDefaultFloors(t *testing.T) {
	repoURL := initRepoForTest(t)
	out, errb, exit := runCmd(t,
		"repo", "gc", repoURL, "--apply", "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", exit, out, errb)
	}
	if strings.Contains(out+errb, "safety_floor_disabled") {
		t.Errorf("default floors must not warn:\nstdout=%s\nstderr=%s", out, errb)
	}
}
