// Build-tagged integration test: full PITR pipeline on a real PG.
//
// What this test proves:
//
//  1. base backup → committed manifest in CAS
//  2. WAL streaming → segment manifests in repo
//  3. restore --to-lsn → recovery.signal + postgresql.auto.conf
//  4. wal fetch → segment reassembly from CAS
//  5. (we stop short of actually starting the restored cluster — that
//     needs a second testcontainer pinned to the restored data dir,
//     which is its own infrastructure exercise. The pieces a started
//     cluster would consume are all asserted here directly.)
//
//go:build integration

package restore_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestIntegration_PITR_RestoreToLSN(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// 1. Take a base backup.
	bres, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString:    srv.DSN,
		RepoURL:         repoURL,
		Deployment:      "db1",
		Signer:          signer,
		Verifier:        verifier,
		Fast:            true,
		IncludeManifest: true,
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	// 2. Stream WAL into the repo while we generate enough write
	//    activity to commit at least one segment. We drive the CLI
	//    in --once mode so it self-cancels once a segment lands.
	streamCtx, streamCancel := context.WithTimeout(ctx, 60*time.Second)
	defer streamCancel()

	// Generate WAL on a side connection.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Brief delay so the stream command is past the slot setup.
		time.Sleep(500 * time.Millisecond)
		c, err := pg.Connect(streamCtx, srv.DSN, pg.ModeRegular)
		if err != nil {
			return
		}
		defer c.Close(streamCtx)
		_ = c.PgConn().ExecParams(streamCtx, "SELECT pg_switch_wal()", nil, nil, nil, nil).Read()
		_ = c.PgConn().ExecParams(streamCtx, "CREATE TABLE pitr_t (i int, payload text)", nil, nil, nil, nil).Read()
		_ = c.PgConn().ExecParams(streamCtx,
			"INSERT INTO pitr_t SELECT g, repeat('p', 16384) FROM generate_series(1, 2048) g",
			nil, nil, nil, nil).Read()
		_ = c.PgConn().ExecParams(streamCtx, "CHECKPOINT", nil, nil, nil, nil).Read()
		_ = c.PgConn().ExecParams(streamCtx, "SELECT pg_switch_wal()", nil, nil, nil, nil).Read()
	}()

	root := cli.NewRoot()
	root.SetArgs([]string{
		"wal", "stream", "db1",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--status-interval", "250ms",
		"--once",
		"--output", "json",
	})
	if exit := cli.Run(root); exit != 0 {
		streamCancel()
		wg.Wait()
		t.Fatalf("wal stream --once exit = %d", exit)
	}
	streamCancel()
	wg.Wait()

	// 3. Restore with --to-lsn pointing at the backup's stop_lsn.
	//    This is the simplest verifiable PITR target — we know that
	//    LSN exists and that the backup is consistent at it.
	target := filepath.Join(t.TempDir(), "restored")
	recovery := &restore.Recovery{
		Enable:         true,
		TargetLSN:      bres.StopLSN,
		Inclusive:      true,
		Action:         "pause",
		Timeline:       "latest",
		RestoreCommand: "/usr/bin/pg_hardstorage wal fetch db1 %f %p --repo " + repoURL,
	}
	rres, err := restore.Restore(ctx, restore.Options{
		RepoURL:    repoURL,
		Deployment: "db1",
		BackupID:   bres.BackupID,
		TargetDir:  target,
		Verifier:   verifier,
		Recovery:   recovery,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if rres.BytesWritten == 0 {
		t.Error("restore wrote 0 bytes")
	}

	// 4. Recovery files present.
	for _, f := range []string{"recovery.signal", "postgresql.auto.conf"} {
		if _, err := os.Stat(filepath.Join(target, f)); err != nil {
			t.Errorf("expected %s in target: %v", f, err)
		}
	}
	autoConf, err := os.ReadFile(filepath.Join(target, "postgresql.auto.conf"))
	if err != nil {
		t.Fatal(err)
	}
	autoStr := string(autoConf)
	for _, want := range []string{
		"# --- pg_hardstorage managed block (PITR) ---",
		"recovery_target_lsn = '" + bres.StopLSN + "'",
		"recovery_target_inclusive = true",
		"recovery_target_action = 'pause'",
		"recovery_target_timeline = 'latest'",
		"restore_command = '/usr/bin/pg_hardstorage wal fetch db1 %f %p --repo " + repoURL + "'",
	} {
		if !strings.Contains(autoStr, want) {
			t.Errorf("postgresql.auto.conf missing %q\n%s", want, autoStr)
		}
	}

	// 5. wal fetch round-trips the first WAL segment from the repo.
	//    This proves the restore_command pipeline would work if PG
	//    were to invoke it during recovery.
	walTarget := filepath.Join(t.TempDir(), "fetched.wal")
	fetchRoot := cli.NewRoot()
	fetchRoot.SetArgs([]string{
		"wal", "fetch", "db1",
		// Segment 0 may not exist (depending on starting LSN), but
		// the test's stream produced at least one segment that does.
		// We rely on the stream having committed segment(s) and
		// ask for the first one we find.
		firstCommittedSegmentName(t, repoURL, "db1", bres.Timeline),
		walTarget,
		"--repo", repoURL,
		"--output", "json",
	})
	if exit := cli.Run(fetchRoot); exit != 0 {
		t.Errorf("wal fetch exit = %d", exit)
	}
	stat, err := os.Stat(walTarget)
	if err != nil {
		t.Fatalf("fetched WAL missing: %v", err)
	}
	if stat.Size() != 16*1024*1024 {
		t.Errorf("fetched WAL size = %d, want 16 MiB", stat.Size())
	}

	// 6. The new sharded audit log must be intact across the whole PITR
	//    cycle: the backup's audit event landed in the deployment's own
	//    shard and `verify-chain` is clean across every shard.
	_, asp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatalf("open repo for audit check: %v", err)
	}
	defer asp.Close()
	avres, err := audit.NewStore(asp).VerifyChain(context.Background())
	if err != nil {
		t.Fatalf("audit verify-chain: %v", err)
	}
	if !avres.OK {
		t.Errorf("audit chain not clean after the PITR cycle: %+v", avres)
	}
	sawShard := false
	for info, lerr := range asp.List(context.Background(), "audit/shards/d.db1/") {
		if lerr != nil {
			t.Fatalf("list db1 audit shard: %v", lerr)
		}
		if strings.HasSuffix(info.Key, ".json") && !strings.HasSuffix(info.Key, "_head.json") {
			sawShard = true
			break
		}
	}
	if !sawShard {
		t.Errorf("backup audit event was not sharded under audit/shards/d.db1/")
	}
}

// firstCommittedSegmentName lists the WAL prefix in the repo and
// returns the lex-smallest committed segment's bare 24-char name.
// Used by the integration test to ask for a segment we know exists.
func firstCommittedSegmentName(t *testing.T, repoURL, deployment string, timeline uint32) string {
	t.Helper()
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer sp.Close()
	prefix := "wal/" + deployment + "/"
	var smallest string
	for info, err := range sp.List(context.Background(), prefix) {
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		key := info.Key
		const wantSuffix = ".json"
		if !strings.HasSuffix(key, wantSuffix) {
			continue
		}
		if strings.Contains(key, ".json.tmp.") {
			continue
		}
		// Pull the bare name from the end of the key.
		base := key[len(key)-len(wantSuffix)-24 : len(key)-len(wantSuffix)]
		if len(base) != 24 {
			continue
		}
		if smallest == "" || base < smallest {
			smallest = base
		}
	}
	if smallest == "" {
		t.Fatal("no committed WAL segments in repo")
	}
	return smallest
}
