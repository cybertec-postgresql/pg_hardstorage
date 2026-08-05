package cli_test

// Repo model checker: a seeded random-operation harness over the REAL
// CLI command paths (rotate, gc, undelete, hold, replicate) plus
// library-planted backups, asserting global repo invariants after
// EVERY step. This is the failure-class test for operation
// interactions — most of the corruption-hunt bugs (undelete over a
// dead parent, holds wedging retention, dedup-vs-GC, replica
// manifests over missing chunks) were sequences of individually-
// correct operations whose interaction corrupted the repo. Single-op
// regression tests can't see those; this harness explores the
// sequence space.
//
// Style mirrors the chaos soak: CI runs fixed seeds (deterministic),
// and PGHS_MODELCHECK_SEED / PGHS_MODELCHECK_OPS opt into deeper
// randomized runs whose seed is logged for exact replay.
//
// Invariants after every operation, checked from REALITY (the repo),
// never from what the model hoped happened:
//
//	I1  every LIVE manifest's chunks are all present (restorable);
//	I2  every live incremental's ancestor chain is fully live;
//	I3  the audit hash chain verifies end-to-end;
//	I4  every manifest present at the REPLICA has all its chunks
//	    at the replica (a replica must never lie about restorability).

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

type modelWorld struct {
	t        *testing.T
	rng      *rand.Rand
	repoURL  string
	repoDir  string
	dstURL   string // replica repo
	sp       storage.StoragePlugin
	dstSP    storage.StoragePlugin
	store    *backup.ManifestStore
	signer   *backup.Signer
	verifier *backup.Verifier

	seq     int      // backup-id uniquifier
	ids     []string // every backup ID ever planted (live or not)
	history []string // op log for failure reports
}

func newModelWorld(t *testing.T, seed int64) *modelWorld {
	t.Helper()
	// Per-test config dir: these fixtures generate keystore material
	// (signing keys, encryption KEK); leaking a kek.bin into the shared
	// ambient config dir flips OTHER tests' "no KEK present" branches.
	_ = configDir(t)
	repoURL := initRepoForTest(t)
	dstURL := initRepoForTest(t)

	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	_, dstSP, err := repo.Open(context.Background(), dstURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dstSP.Close() })

	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	signer, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		t.Fatal(err)
	}
	return &modelWorld{
		t: t, rng: rand.New(rand.NewSource(seed)),
		repoURL: repoURL, repoDir: strings.TrimPrefix(repoURL, "file://"),
		dstURL: dstURL,
		sp:     sp, dstSP: dstSP,
		store:  backup.NewManifestStore(sp),
		signer: signer, verifier: verifier,
	}
}

func (w *modelWorld) log(format string, args ...any) {
	w.history = append(w.history, fmt.Sprintf(format, args...))
}

// liveManifests reads the CURRENT live set from the repo.
func (w *modelWorld) liveManifests() map[string]*backup.Manifest {
	w.t.Helper()
	out := map[string]*backup.Manifest{}
	for m, err := range w.store.List(context.Background(), "db1", w.verifier) {
		if err != nil {
			w.fail("list live manifests: %v", err)
		}
		if m != nil {
			out[m.BackupID] = m
		}
	}
	return out
}

func (w *modelWorld) fail(format string, args ...any) {
	w.t.Helper()
	w.t.Fatalf("MODEL VIOLATION: "+format+"\n--- op history (%d ops) ---\n%s",
		append(args, len(w.history), strings.Join(w.history, "\n"))...)
}

// plantBackup commits a manifest with 1..3 chunks. Roughly half the
// chunk bodies reuse earlier content (dedup pressure — the dedup-vs-GC
// class needs shared chunks), half are fresh. Chunk files are
// backdated so gc's age floor never protects them by accident.
func (w *modelWorld) plantBackup(incremental bool) {
	w.t.Helper()
	parent := ""
	if incremental {
		// Parent: a random LIVE manifest; if none, plant a full instead.
		live := w.liveManifests()
		var candidates []string
		for id := range live {
			candidates = append(candidates, id)
		}
		if len(candidates) == 0 {
			incremental = false
		} else {
			parent = candidates[w.rng.Intn(len(candidates))]
		}
	}

	w.seq++
	cas := casdefault.New(w.sp)
	nChunks := 1 + w.rng.Intn(3)
	var files []backup.FileEntry
	var offset int64
	var chunks []backup.ChunkRef
	for c := 0; c < nChunks; c++ {
		var body []byte
		if w.rng.Intn(2) == 0 && w.seq > 1 {
			body = []byte(fmt.Sprintf("shared-content-%d", w.rng.Intn(5)))
		} else {
			body = []byte(fmt.Sprintf("unique-content-%d-%d", w.seq, c))
		}
		info, err := cas.PutChunk(context.Background(), body)
		if err != nil {
			w.fail("plant chunk: %v", err)
		}
		chunks = append(chunks, backup.ChunkRef{Hash: info.Hash, Offset: offset, Len: int64(len(body))})
		offset += int64(len(body))
	}
	files = append(files, backup.FileEntry{Path: "data/f", Size: offset, Mode: 0o600, Chunks: chunks})

	btype := backup.BackupTypeFull
	if incremental {
		btype = backup.BackupTypeIncremental
	}
	id := fmt.Sprintf("db1.%s.%04d", btype, w.seq)
	ts := time.Now().UTC().Add(-time.Duration(1000-w.seq) * time.Minute)
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: id, Deployment: "db1", Tenant: "default",
		Type: btype, ParentBackupID: parent,
		PGVersion: 17, SystemIdentifier: "7000000000000000001",
		StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
		StartedAt: ts.Add(-time.Minute), StoppedAt: ts,
		BackupLabel: "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:       files,
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		w.fail("plant commit %s: %v", id, err)
	}
	w.ids = append(w.ids, id)
	w.log("plant %s parent=%q chunks=%d", id, parent, nChunks)

	// Backdate every chunk so the gc age floor is never the reason a
	// chunk survives — the model wants gc's REFERENCE logic on trial.
	old := time.Now().Add(-48 * time.Hour)
	_ = filepath.WalkDir(filepath.Join(w.repoDir, "chunks"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			_ = os.Chtimes(path, old, old)
		}
		return nil
	})
}

func (w *modelWorld) randomID() string {
	if len(w.ids) == 0 {
		return "db1.full.none"
	}
	return w.ids[w.rng.Intn(len(w.ids))]
}

// step runs one random operation through the REAL CLI paths.
func (w *modelWorld) step() {
	switch w.rng.Intn(10) {
	case 0, 1: // 20%: new full
		w.plantBackup(false)
	case 2, 3: // 20%: new incremental
		w.plantBackup(true)
	case 4: // delete (cascade half the time)
		id := w.randomID()
		args := []string{"backup", "delete", "db1", id, "--repo", w.repoURL, "--output", "json"}
		if w.rng.Intn(2) == 0 {
			args = append(args, "--cascade")
		}
		_, _, exit := runCmd(w.t, args...)
		w.log("delete %s cascade=%v -> exit=%d", id, strings.Contains(strings.Join(args, " "), "cascade"), exit)
	case 5: // undelete
		id := w.randomID()
		_, _, exit := runCmd(w.t, "backup", "undelete", "db1", id, "--repo", w.repoURL, "--output", "json")
		w.log("undelete %s -> exit=%d", id, exit)
	case 6: // hold add or remove
		id := w.randomID()
		if w.rng.Intn(2) == 0 {
			_, _, exit := runCmd(w.t, "hold", "add", "db1", id, "--repo", w.repoURL, "--reason", "model", "--output", "json")
			w.log("hold add %s -> exit=%d", id, exit)
		} else {
			_, _, exit := runCmd(w.t, "hold", "remove", "db1", id, "--repo", w.repoURL, "--output", "json")
			w.log("hold remove %s -> exit=%d", id, exit)
		}
	case 7: // rotate --apply (aggressive or lax at random)
		keepFor := "1ms"
		if w.rng.Intn(2) == 0 {
			keepFor = "240h"
		}
		_, _, exit := runCmd(w.t, "rotate", "db1", "--repo", w.repoURL,
			"--policy", "simple", "--keep-for", keepFor, "--apply", "--output", "json")
		w.log("rotate keep-for=%s -> exit=%d", keepFor, exit)
	case 8: // gc --apply with floors tiny: reference logic on trial
		_, _, exit := runCmd(w.t, "repo", "gc", w.repoURL, "--apply",
			"--tombstone-grace", "1ms", "--min-chunk-age", "1ms", "--output", "json")
		w.log("gc -> exit=%d", exit)
	case 9: // replicate to the second repo
		_, _, exit := runCmd(w.t, "repo", "replicate", "--from", w.repoURL, "--to", w.dstURL, "--output", "json")
		w.log("replicate -> exit=%d", exit)
	}
}

// checkInvariants validates I1..I4 from repo reality.
func (w *modelWorld) checkInvariants() {
	w.t.Helper()
	ctx := context.Background()

	// I1 + I2 over the live set.
	live := w.liveManifests()
	for id, m := range live {
		res, err := backup.CheckChunkExistence(ctx, w.sp, m)
		if err != nil {
			w.fail("I1 %s: chunk existence check: %v", id, err)
		}
		if !res.AllPresent() {
			w.fail("I1 VIOLATED: live manifest %s references %d missing chunk(s) — an operation deleted chunks a live backup needs", id, len(res.Missing))
		}
		cur := m
		for cur.ParentBackupID != "" {
			p, ok := live[cur.ParentBackupID]
			if !ok {
				w.fail("I2 VIOLATED: live incremental %s has non-live ancestor %s — restore is impossible while the backup lists as healthy", id, cur.ParentBackupID)
			}
			cur = p
		}
	}

	// I3: audit chain integrity.
	if res, err := audit.NewStore(w.sp).VerifyChain(ctx); err != nil || !res.OK {
		w.fail("I3 VIOLATED: audit chain broken after operation: res=%+v err=%v", res, err)
	}

	// I4: replica never lies — every dst manifest fully backed by dst chunks.
	dstStore := backup.NewManifestStore(w.dstSP)
	for m, err := range dstStore.List(ctx, "db1", w.verifier) {
		if err != nil || m == nil {
			continue // unverifiable replica entries are I4-exempt; replicate owns signature fidelity
		}
		res, cerr := backup.CheckChunkExistence(ctx, w.dstSP, m)
		if cerr != nil {
			w.fail("I4 %s: replica chunk check: %v", m.BackupID, cerr)
		}
		if !res.AllPresent() {
			w.fail("I4 VIOLATED: replica manifest %s is missing %d chunk(s) at the replica — the DR copy lies about restorability", m.BackupID, len(res.Missing))
		}
	}
}

func runModelCheck(t *testing.T, seed int64, ops int, budget time.Duration) {
	t.Helper()
	w := newModelWorld(t, seed)
	t.Logf("model check: seed=%d ops=%d budget=%s (re-run with PGHS_MODELCHECK_SEED=%d)", seed, ops, budget, seed)
	start := time.Now()
	w.plantBackup(false) // always start with one full
	w.checkInvariants()
	for i := 0; i < ops; i++ {
		w.step()
		w.checkInvariants()
		if (i+1)%500 == 0 {
			t.Logf("model check: %d/%d ops, %s elapsed, all invariants held", i+1, ops, time.Since(start).Round(time.Second))
		}
		// The per-op invariant sweep scales with repo size, so deep
		// runs are wall-clock-bound, not op-bound. Stop CLEANLY at
		// the budget (every completed op was fully verified) instead
		// of letting the test binary's -timeout axe the run mid-sweep.
		if budget > 0 && time.Since(start) > budget {
			t.Logf("model check: time budget %s reached after %d/%d ops — every completed op verified; not a failure", budget, i+1, ops)
			return
		}
	}
}

// Fixed seeds: deterministic CI coverage of the sequence space.
func TestModelCheck_FixedSeeds(t *testing.T) {
	for _, seed := range []int64{1, 7, 42, 20260727, 987654321} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			runModelCheck(t, seed, 40, 0)
		})
	}
}

// Randomized deep run: opt-in (nightly / manual). The seed is logged,
// so any failure replays exactly.
func TestModelCheck_Randomized(t *testing.T) {
	seedEnv := os.Getenv("PGHS_MODELCHECK_SEED")
	opsEnv := os.Getenv("PGHS_MODELCHECK_OPS")
	// No skip when the env is unset: a skip reports PASS, so a harness
	// that greps for pass/fail records a run that never happened as a
	// success. The soak campaign of 2026-08-05 did exactly that with
	// the chaos phase — "passed" in two seconds having run nothing.
	// Every other soak in this tree defaults to a short run and lets
	// the environment deepen it; this one now matches.
	seed := time.Now().UnixNano()
	if seedEnv != "" {
		if n, err := strconv.ParseInt(seedEnv, 10, 64); err == nil {
			seed = n
		}
	}
	ops := 400
	if opsEnv != "" {
		if n, err := strconv.Atoi(opsEnv); err == nil && n > 0 {
			ops = n
		}
	}
	// Default wall-clock budget for deep runs; override with
	// PGHS_MODELCHECK_BUDGET (Go duration). Keep comfortably under
	// the test binary's -timeout so the stop is always clean.
	budget := 90 * time.Minute
	if v := os.Getenv("PGHS_MODELCHECK_BUDGET"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			budget = d
		}
	}
	runModelCheck(t, seed, ops, budget)
}
