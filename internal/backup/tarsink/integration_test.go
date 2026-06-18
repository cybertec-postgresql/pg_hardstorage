// Build-tagged integration test: real BASE_BACKUP through the Sink.
// Run with `make test-integration` (requires Docker).
//
//go:build integration

package tarsink_test

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/tarsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/basebackup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestIntegration_BasebackupTarsink_RealPG runs BASE_BACKUP against a
// real PG 17 container, drives the bytes through the tarsink Sink, and
// asserts the output makes sense:
//
//   - non-zero file count from the default tablespace
//   - backup_label was extracted (PG always emits it)
//   - PG_VERSION is one of the files
//   - reconstituting PG_VERSION from its chunk-refs yields "17"
//   - manifest bytes are populated when MANIFEST is enabled
func TestIntegration_BasebackupTarsink_RealPG(t *testing.T) {
	srv := testkit.StartPostgres(t)

	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	cas := repo.NewCAS(sp)

	// 240s, not 90s: this budget covers a full basebackup streamed through
	// the tarsink to a file CAS that fsyncs every chunk. On a slow / loaded
	// disk that streaming alone exceeds 90s (the basebackup of even a fresh
	// cluster is fsync-bound), so the old budget made the test flake by
	// disk speed rather than catch a real regression. 240s still bounds a
	// genuine hang and matches the slowest sibling integration budgets.
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	conn, err := pg.Connect(ctx, srv.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	sink := tarsink.New(ctx, cas)
	res, err := basebackup.Run(ctx, conn, basebackup.Options{
		Label:    "tarsink-integration",
		Fast:     true,
		Manifest: true,
	}, sink)
	if err != nil {
		t.Fatalf("basebackup.Run: %v", err)
	}

	files := sink.AllFiles()
	if len(files) == 0 {
		t.Fatal("AllFiles is empty; the default tablespace should contain many files")
	}
	if len(sink.BackupLabel()) == 0 {
		t.Errorf("BackupLabel must be populated; got empty")
	}
	if !bytes.Contains(sink.BackupLabel(), []byte("START WAL LOCATION:")) {
		t.Errorf("BackupLabel doesn't look like a real backup_label; got %q", sink.BackupLabel())
	}
	if !bytes.Contains(sink.ManifestBytes(), []byte("PostgreSQL-Backup-Manifest-Version")) {
		t.Errorf("ManifestBytes doesn't look like a real PG manifest; first 200 bytes:\n%s",
			truncateBytes(sink.ManifestBytes(), 200))
	}

	// Find PG_VERSION and verify its bytes round-trip through CAS.
	var pgVersion *backup.FileEntry
	for i := range files {
		if strings.HasSuffix(files[i].Path, "PG_VERSION") {
			pgVersion = &files[i]
			break
		}
	}
	if pgVersion == nil {
		t.Fatal("PG_VERSION not found in AllFiles")
	}

	var rebuilt bytes.Buffer
	for _, ref := range pgVersion.Chunks {
		body, err := cas.GetChunkBytes(ctx, ref.Hash)
		if err != nil {
			t.Fatalf("get chunk: %v", err)
		}
		rebuilt.Write(body)
	}
	got := strings.TrimSpace(rebuilt.String())
	if want := testkit.ExpectedPGMajor(); got != want {
		t.Errorf("PG_VERSION reconstituted = %q, want %q", got, want)
	}

	if res.StopLSN == "" || res.StopTimeline == 0 {
		t.Errorf("Result missing stop LSN/timeline: %+v", res)
	}
}

// truncateBytes returns at most n bytes of body, with a "..." suffix
// when truncation happened. Used for compact error messages.
func truncateBytes(body []byte, n int) string {
	if len(body) <= n {
		return string(body)
	}
	return string(body[:n]) + "..."
}
