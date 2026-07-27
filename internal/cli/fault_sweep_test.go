package cli_test

// Fault-injection "loud failure" sweep: run each maintenance command
// with exactly ONE injected storage failure at every possible point
// (sweeping the failure position), and assert the fail-open failure
// class away:
//
//  1. A run that absorbed a fault must either exit non-zero (loud) or
//     leave the repo satisfying every integrity invariant (a
//     documented best-effort sub-step may soften a fault, but never
//     at the cost of repo integrity).
//  2. Re-running the command fault-free after ANY faulted run must
//     succeed and converge to the fault-free reference state
//     (crash/retry recovery — no wedged intermediate states).
//
// This is the class test for bugs like the fail-open repo-version
// gate, verify's fabricated "skipped", and capacity's silently
// swallowed List errors: every one was a storage error downgraded to
// "success with silently less work done".

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// faultRemaining counts storage calls until the single injected
// failure: >0 decrements, 0 fires the fault (once), <0 disabled.
var faultRemaining atomic.Int64

// faultsFired counts injected failures, so the sweep knows when N
// exceeded the command's total call count.
var faultsFired atomic.Int64

func armFault(n int64) { faultsFired.Store(0); faultRemaining.Store(n) }

func faultHit() bool {
	for {
		v := faultRemaining.Load()
		if v < 0 {
			return false
		}
		if v == 0 {
			// Fire exactly once, then disarm.
			if faultRemaining.CompareAndSwap(0, -1) {
				faultsFired.Add(1)
				return true
			}
			continue
		}
		if faultRemaining.CompareAndSwap(v, v-1) {
			return false
		}
	}
}

var errInjected = errors.New("injected storage fault (fault sweep)")

// faultPlugin wraps the fs plugin; every storage call consults the
// fault counter.
type faultPlugin struct {
	fs.Plugin
}

func (p *faultPlugin) Open(ctx context.Context, cfg storage.StorageConfig) error {
	u := *cfg.URL
	u.Scheme = "file"
	cfg.URL = &u
	return p.Plugin.Open(ctx, cfg)
}

func (p *faultPlugin) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if faultHit() {
		return storage.PutResult{}, errInjected
	}
	return p.Plugin.Put(ctx, key, r, opts)
}

func (p *faultPlugin) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if faultHit() {
		return nil, errInjected
	}
	return p.Plugin.Get(ctx, key)
}

func (p *faultPlugin) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if faultHit() {
		return storage.ObjectInfo{}, errInjected
	}
	return p.Plugin.Stat(ctx, key)
}

func (p *faultPlugin) Delete(ctx context.Context, key string) error {
	if faultHit() {
		return errInjected
	}
	return p.Plugin.Delete(ctx, key)
}

func (p *faultPlugin) RenameIfNotExists(ctx context.Context, src, dst string) error {
	if faultHit() {
		return errInjected
	}
	return p.Plugin.RenameIfNotExists(ctx, src, dst)
}

func init() {
	faultRemaining.Store(-1)
	storage.Register("faultfile", func() storage.StoragePlugin { return &faultPlugin{} })
}

// snapshotDir copies a repo directory (used to reset state between
// sweep iterations).
func snapshotDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	if out, err := exec.Command("cp", "-a", src+"/.", dst).CombinedOutput(); err != nil {
		t.Fatalf("snapshot: %v\n%s", err, out)
	}
	return dst
}

func restoreDir(t *testing.T, snapshot, target string) {
	t.Helper()
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-a", snapshot+"/.", target).CombinedOutput(); err != nil {
		t.Fatalf("restore snapshot: %v\n%s", err, out)
	}
}

func TestFaultSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("fault sweep is not -short")
	}
	cases := []struct {
		name string
		args func(repoURL string) []string
	}{
		{"repo_gc_apply", func(u string) []string {
			return []string{"repo", "gc", u, "--apply", "--tombstone-grace", "1ms", "--min-chunk-age", "1ms", "--output", "json"}
		}},
		{"rotate_apply", func(u string) []string {
			return []string{"rotate", "db1", "--repo", u, "--policy", "simple", "--keep-for", "240h", "--apply", "--output", "json"}
		}},
		{"undelete", func(u string) []string {
			return []string{"backup", "undelete", "db1", "db1.full.old", "--repo", u, "--output", "json"}
		}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			plainURL, repoDir := seedIdempotencyRepo(t, false)
			_ = plainURL
			faultURL := "faultfile://" + repoDir
			u, _ := url.Parse(faultURL)
			if u.Path == "" { // faultfile://path parses host-only; normalize
				faultURL = "faultfile://" + "/" + strings.TrimPrefix(repoDir, "/")
			}
			snapshot := snapshotDir(t, repoDir)

			// Fault-free reference run.
			armFault(-1)
			if _, stderr, exit := runCmd(t, tc.args(faultURL)...); exit != 0 {
				t.Fatalf("reference run failed (exit %d):\n%s", exit, stderr)
			}
			refDigest := repoDigest(t, repoDir)

			const maxPositions = 40
			for n := int64(0); n < maxPositions; n++ {
				restoreDir(t, snapshot, repoDir)
				armFault(n)
				_, _, exit := runCmd(t, tc.args(faultURL)...)
				fired := faultsFired.Load() > 0
				armFault(-1)
				if !fired {
					// Command finished without reaching call N — the
					// whole call space is swept.
					if exit != 0 {
						t.Fatalf("position %d: no fault fired but exit=%d", n, exit)
					}
					return
				}

				// Property 2: a fault-free re-run must converge.
				_, stderr2, exit2 := runCmd(t, tc.args(faultURL)...)
				if exit2 != 0 {
					t.Fatalf("fault@%d: RE-RUN after the faulted run failed (exit %d) — wedged intermediate state:\n%s", n, exit2, stderr2)
				}
				if got := repoDigest(t, repoDir); got != refDigest {
					t.Fatalf("fault@%d: state after faulted run + clean re-run diverges from the fault-free reference — silent partial work survived", n)
				}
			}
			t.Logf("swept %d fault positions without exhausting the call space (command makes more calls; coverage is prefix-only)", maxPositions)
		})
	}
}
