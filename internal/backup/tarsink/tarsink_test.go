package tarsink_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	mathrand "math/rand"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/tarsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/basebackup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// fileSpec describes one file to embed in a synthetic tar.
type fileSpec struct {
	name string
	body []byte
	mode int64
}

// buildTar produces a tar archive containing files. Used by tests to
// drive Sink as if PG had streamed the bytes.
func buildTar(t *testing.T, files []fileSpec) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		mode := f.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:    f.name,
			Mode:    mode,
			Size:    int64(len(f.body)),
			ModTime: time.Date(2026, 4, 28, 14, 0, 0, 0, time.UTC),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newSinkAndCAS returns a freshly constructed Sink rooted on an fs-backed CAS.
func newSinkAndCAS(t *testing.T) (*tarsink.Sink, *repo.CAS) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cas := repo.NewCAS(sp)
	return tarsink.New(context.Background(), cas), cas
}

// drive feeds the given tar bytes through the Sink as if BASE_BACKUP
// emitted them in N CopyData chunks. chunkSize controls the granularity
// (network packets aren't aligned to file boundaries).
func drive(t *testing.T, sink *tarsink.Sink, idx int, info basebackup.TablespaceInfo, tarBytes []byte, chunkSize int) error {
	t.Helper()
	if err := sink.OnTablespaceStart(idx, info); err != nil {
		return err
	}
	if chunkSize <= 0 {
		chunkSize = len(tarBytes)
	}
	for off := 0; off < len(tarBytes); off += chunkSize {
		end := off + chunkSize
		if end > len(tarBytes) {
			end = len(tarBytes)
		}
		if err := sink.OnTablespaceData(idx, tarBytes[off:end]); err != nil {
			return err
		}
	}
	return sink.OnTablespaceEnd(idx)
}

// TestTarsink_RejectsTraversalEntryNames is the security regression for
// path-traversal in the BASE_BACKUP stream: a tar entry whose name is
// absolute or carries a ".." escape (or a backslash separator) must be
// refused at backup time so a hostile/corrupt source can't commit a
// backup whose paths would later be written outside the restore target.
func TestTarsink_RejectsTraversalEntryNames(t *testing.T) {
	bad := []string{
		"../escape",
		"../../etc/cron.d/evil",
		"base/../../../../etc/passwd", // cleans to a leading ".."
		"/etc/shadow",                 // absolute
		"..",                          // exactly the parent ref
		`base\16384\2619`,             // backslash separator
	}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			sink, _ := newSinkAndCAS(t)
			tarBytes := buildTar(t, []fileSpec{{name: name, body: []byte("x")}})
			err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 64)
			if err == nil {
				t.Errorf("entry name %q must be rejected", name)
			}
		})
	}

	// Ordinary PGDATA-relative names — including an interior ".." that
	// stays in-tree (base/../global => global) — must still be accepted.
	for _, name := range []string{"base/16384/2619", "global/pg_control", "base/../global/1"} {
		t.Run("ok:"+name, func(t *testing.T) {
			sink, _ := newSinkAndCAS(t)
			tarBytes := buildTar(t, []fileSpec{{name: name, body: []byte("data")}})
			if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 64); err != nil {
				t.Errorf("entry name %q should be accepted: %v", name, err)
			}
		})
	}
}

func TestTarsink_SingleFile_RoundTrip(t *testing.T) {
	sink, cas := newSinkAndCAS(t)
	body := bytes.Repeat([]byte("a"), 100*1024) // 100 KiB
	tarBytes := buildTar(t, []fileSpec{{name: "base/16384/2619", body: body}})

	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 4096); err != nil {
		t.Fatalf("drive: %v", err)
	}

	files := sink.Files(0)
	if len(files) != 1 {
		t.Fatalf("Files(0) len = %d, want 1", len(files))
	}
	entry := files[0]
	if entry.Path != "base/16384/2619" {
		t.Errorf("Path = %q", entry.Path)
	}
	if entry.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", entry.Size, len(body))
	}
	if len(entry.Chunks) == 0 {
		t.Fatalf("expected at least one chunk")
	}

	// Reconstitute via CAS using the chunk refs.
	var rebuilt bytes.Buffer
	for _, ref := range entry.Chunks {
		bs, err := cas.GetChunkBytes(context.Background(), ref.Hash)
		if err != nil {
			t.Fatalf("GetChunkBytes: %v", err)
		}
		if int64(len(bs)) != ref.Len {
			t.Errorf("chunk %s len %d != ref %d", ref.Hash, len(bs), ref.Len)
		}
		rebuilt.Write(bs)
	}
	if !bytes.Equal(rebuilt.Bytes(), body) {
		t.Error("reconstituted bytes do not match original")
	}
}

func TestTarsink_MultipleFiles(t *testing.T) {
	sink, _ := newSinkAndCAS(t)
	tarBytes := buildTar(t, []fileSpec{
		{name: "PG_VERSION", body: []byte("17\n")},
		{name: "global/pg_control", body: bytes.Repeat([]byte{0x42}, 8192)},
		{name: "base/1/2619", body: []byte("page bytes here")},
	})
	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 0); err != nil {
		t.Fatalf("drive: %v", err)
	}
	files := sink.Files(0)
	if len(files) != 3 {
		t.Fatalf("Files = %d, want 3", len(files))
	}
	wantPaths := map[string]bool{"PG_VERSION": true, "global/pg_control": true, "base/1/2619": true}
	for _, f := range files {
		delete(wantPaths, f.Path)
	}
	if len(wantPaths) != 0 {
		t.Errorf("missing paths: %v", wantPaths)
	}
}

// newSinkAndCASWithObserver mirrors newSinkAndCAS but registers a
// FileObserver so the test can assert on per-file callbacks.
func newSinkAndCASWithObserver(t *testing.T, obs tarsink.FileObserver) (*tarsink.Sink, *repo.CAS) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cas := repo.NewCAS(sp)
	return tarsink.New(context.Background(), cas, tarsink.WithFileObserver(obs)), cas
}

// TestTarsink_FileObserver_FiresPerFile is the issue #9 happy path:
// the observer sees one callback per regular file with realistic
// stats (path, size, chunk count).  Empty files still report
// (size=0, chunks=0) so a `--verbose` consumer never sees a silent
// skip.
func TestTarsink_FileObserver_FiresPerFile(t *testing.T) {
	var stats []tarsink.FileStats
	sink, _ := newSinkAndCASWithObserver(t, func(s tarsink.FileStats) {
		stats = append(stats, s)
	})
	tarBytes := buildTar(t, []fileSpec{
		{name: "PG_VERSION", body: []byte("17\n")},
		{name: "global/pg_control", body: bytes.Repeat([]byte{0x42}, 64*1024)},
		{name: "base/1/empty", body: nil},
	})
	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 0); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(stats) != 3 {
		t.Fatalf("observer fired %d times; want 3", len(stats))
	}
	wantSizes := map[string]int64{
		"PG_VERSION":        3,
		"global/pg_control": 64 * 1024,
		"base/1/empty":      0,
	}
	for _, s := range stats {
		want, ok := wantSizes[s.Path]
		if !ok {
			t.Errorf("unexpected path %q", s.Path)
			continue
		}
		if s.Size != want {
			t.Errorf("Size for %q = %d; want %d", s.Path, s.Size, want)
		}
		// Empty files have no chunks; non-empty have at least one.
		if want == 0 && s.ChunkCount != 0 {
			t.Errorf("%q: empty file reported %d chunks", s.Path, s.ChunkCount)
		}
		if want > 0 && s.ChunkCount == 0 {
			t.Errorf("%q: non-empty file reported zero chunks", s.Path)
		}
	}
}

// TestTarsink_FileObserver_TracksDedup verifies the dedup counters
// the verbose renderer leans on.  Two identical files should
// chunk-equal — the second's stats report DedupedChunks == ChunkCount
// and UniqueBytes == 0.
func TestTarsink_FileObserver_TracksDedup(t *testing.T) {
	var stats []tarsink.FileStats
	sink, _ := newSinkAndCASWithObserver(t, func(s tarsink.FileStats) {
		stats = append(stats, s)
	})
	body := bytes.Repeat([]byte("dedup-me"), 16*1024) // 128 KiB, deterministic content
	tarBytes := buildTar(t, []fileSpec{
		{name: "twin/a", body: body},
		{name: "twin/b", body: body}, // identical content
	})
	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 0); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("observer fired %d times; want 2", len(stats))
	}
	first := stats[0]
	second := stats[1]
	if first.DedupedChunks != 0 {
		t.Errorf("first file reported %d deduped chunks; want 0 (nothing in CAS yet)",
			first.DedupedChunks)
	}
	if first.UniqueBytes == 0 {
		t.Errorf("first file reported UniqueBytes=0; want >0")
	}
	if second.ChunkCount == 0 {
		t.Fatalf("second file reported zero chunks")
	}
	if second.DedupedChunks != second.ChunkCount {
		t.Errorf("second file deduped %d/%d chunks; want all (identical content)",
			second.DedupedChunks, second.ChunkCount)
	}
	if second.UniqueBytes != 0 {
		t.Errorf("second file UniqueBytes=%d; want 0 (full dedup)", second.UniqueBytes)
	}
}

func TestTarsink_BackupLabel_And_TablespaceMap_Captured(t *testing.T) {
	sink, _ := newSinkAndCAS(t)
	wantLabel := []byte("START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n")
	wantMap := []byte("16384 /srv/ts2\n")

	tarBytes := buildTar(t, []fileSpec{
		{name: "backup_label", body: wantLabel},
		{name: "tablespace_map", body: wantMap},
		{name: "PG_VERSION", body: []byte("17\n")},
	})
	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 0); err != nil {
		t.Fatalf("drive: %v", err)
	}

	if !bytes.Equal(sink.BackupLabel(), wantLabel) {
		t.Errorf("BackupLabel mismatch: got %q", sink.BackupLabel())
	}
	if !bytes.Equal(sink.TablespaceMap(), wantMap) {
		t.Errorf("TablespaceMap mismatch: got %q", sink.TablespaceMap())
	}
	// They must NOT appear in Files — they're manifest fields, not chunks.
	for _, f := range sink.Files(0) {
		if f.Path == "backup_label" || f.Path == "tablespace_map" {
			t.Errorf("special file %q leaked into Files", f.Path)
		}
	}
}

// TestTarsink_BackupLabel_CapturedFromBaseTablespace_NotIndexZero is the
// regression for issue #17: when a non-default tablespace exists, PG
// streams the user tablespace archive(s) FIRST and the base/default
// tablespace (the one carrying backup_label + tablespace_map) LAST. The
// sink must capture those special files from whichever archive holds
// them, not assume tablespace index 0. The previous idx==0 gate dropped
// backup_label here, leaving an empty manifest field that failed its own
// invariant check ("backup_label is empty (required for restore)") and
// refused to commit the backup.
func TestTarsink_BackupLabel_CapturedFromBaseTablespace_NotIndexZero(t *testing.T) {
	sink, _ := newSinkAndCAS(t)

	wantLabel := []byte("START WAL LOCATION: 0/23000168 (file 000000010000000000000023)\n")
	wantMap := []byte("16384 /data/postgresql/18/tablespaces/tbs1\n")

	// idx 0 — the USER tablespace (tbs1). Its tar entries are nested
	// under PG_<ver>_<cat>/<dboid>/...; there is no root backup_label.
	userTS := buildTar(t, []fileSpec{
		{name: "PG_18_202209061/16384/12345", body: []byte("user tablespace relfile")},
	})
	// idx 1 — the BASE/default tablespace, streamed last, carrying the
	// special files at its root.
	baseTS := buildTar(t, []fileSpec{
		{name: "backup_label", body: wantLabel},
		{name: "tablespace_map", body: wantMap},
		{name: "PG_VERSION", body: []byte("18\n")},
		{name: "global/pg_control", body: []byte("control")},
	})

	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 16384}, userTS, 0); err != nil {
		t.Fatalf("drive user tablespace: %v", err)
	}
	if err := drive(t, sink, 1, basebackup.TablespaceInfo{OID: 0}, baseTS, 0); err != nil {
		t.Fatalf("drive base tablespace: %v", err)
	}

	if !bytes.Equal(sink.BackupLabel(), wantLabel) {
		t.Errorf("backup_label not captured from the base tablespace (issue #17): got %q", sink.BackupLabel())
	}
	if !bytes.Equal(sink.TablespaceMap(), wantMap) {
		t.Errorf("tablespace_map not captured from the base tablespace: got %q", sink.TablespaceMap())
	}
	// The special files must not leak into the base tablespace's file list.
	for _, f := range sink.Files(1) {
		if f.Path == "backup_label" || f.Path == "tablespace_map" {
			t.Errorf("special file %q leaked into Files(1)", f.Path)
		}
	}
	// The user tablespace's real relfile is preserved as a normal file.
	if files0 := sink.Files(0); len(files0) != 1 || files0[0].Path != "PG_18_202209061/16384/12345" {
		t.Errorf("user tablespace file list = %+v, want the single relfile", files0)
	}
}

func TestTarsink_EmptyFile_ProducesZeroChunks(t *testing.T) {
	sink, _ := newSinkAndCAS(t)
	tarBytes := buildTar(t, []fileSpec{{name: "marker", body: nil}})
	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 0); err != nil {
		t.Fatal(err)
	}
	files := sink.Files(0)
	if len(files) != 1 {
		t.Fatalf("Files = %d, want 1", len(files))
	}
	if files[0].Size != 0 {
		t.Errorf("Size = %d, want 0", files[0].Size)
	}
	if len(files[0].Chunks) != 0 {
		t.Errorf("Chunks = %d, want 0 for empty file", len(files[0].Chunks))
	}
}

func TestTarsink_DirectoriesCaptured_SymlinksSkipped(t *testing.T) {
	// Regression test for issue #7: empty directories like
	// pg_wal/ must be captured into manifest.Dirs so the
	// restore step can re-create them.  Without this, PG
	// refuses to start on the restored datadir.
	sink, _ := newSinkAndCAS(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Three TypeDir entries — the empty PG-required dirs we
	// expect every restore to recreate.
	tw.WriteHeader(&tar.Header{Name: "base/", Typeflag: tar.TypeDir, Mode: 0o700})
	tw.WriteHeader(&tar.Header{Name: "pg_wal/", Typeflag: tar.TypeDir, Mode: 0o700})
	tw.WriteHeader(&tar.Header{Name: "pg_replslot/", Typeflag: tar.TypeDir, Mode: 0o700})
	// Symlink (still skipped — tablespace symlinks are
	// handled out-of-band via tablespace_map).
	tw.WriteHeader(&tar.Header{Name: "pg_tblspc/16384", Typeflag: tar.TypeSymlink, Linkname: "/srv/ts2"})
	// Regular file.
	tw.WriteHeader(&tar.Header{Name: "base/PG_VERSION", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
	tw.Write([]byte("17\n"))
	tw.Close()

	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, buf.Bytes(), 0); err != nil {
		t.Fatal(err)
	}
	files := sink.Files(0)
	if len(files) != 1 {
		t.Errorf("only the regular file should land in Files; got %v", files)
	}
	dirs := sink.AllDirs()
	wantPaths := map[string]uint32{
		"base/":        0o700,
		"pg_wal/":      0o700,
		"pg_replslot/": 0o700,
	}
	if len(dirs) != len(wantPaths) {
		t.Fatalf("AllDirs = %d entries, want %d (%+v)", len(dirs), len(wantPaths), dirs)
	}
	got := map[string]uint32{}
	for _, d := range dirs {
		got[d.Path] = d.Mode
	}
	for path, mode := range wantPaths {
		if got[path] != mode {
			t.Errorf("dir %s: mode %#o, want %#o (all dirs: %+v)", path, got[path], mode, dirs)
		}
	}
}

func TestTarsink_MalformedTar_PropagatesError(t *testing.T) {
	sink, _ := newSinkAndCAS(t)
	// Random bytes that aren't a valid tar.
	garbage := bytes.Repeat([]byte{0xFE}, 4096)
	err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, garbage, 0)
	if err == nil {
		t.Fatal("expected error on malformed tar")
	}
	if !strings.Contains(err.Error(), "parse") && !strings.Contains(err.Error(), "tar") {
		t.Errorf("error should mention tar/parse: %v", err)
	}
}

func TestTarsink_AllFiles_AcrossTablespaces(t *testing.T) {
	sink, _ := newSinkAndCAS(t)
	t0 := buildTar(t, []fileSpec{{name: "PG_VERSION", body: []byte("17")}})
	t1 := buildTar(t, []fileSpec{{name: "16384/2619", body: []byte("ts2 data")}})

	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, t0, 0); err != nil {
		t.Fatal(err)
	}
	if err := drive(t, sink, 1, basebackup.TablespaceInfo{OID: 16384}, t1, 0); err != nil {
		t.Fatal(err)
	}

	all := sink.AllFiles()
	if len(all) != 2 {
		t.Errorf("AllFiles = %d, want 2", len(all))
	}
	if all[0].Path != "PG_VERSION" || all[1].Path != "16384/2619" {
		t.Errorf("AllFiles order: %v", all)
	}
}

func TestTarsink_ManifestSinkIndex_AccumulatesRawBytes(t *testing.T) {
	sink, _ := newSinkAndCAS(t)
	mani := []byte(`{"PostgreSQL-Backup-Manifest-Version": 1, "Files": []}`)

	// First a tablespace, then the manifest CopyOut.
	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, buildTar(t, []fileSpec{{name: "x", body: []byte("y")}}), 0); err != nil {
		t.Fatal(err)
	}
	if err := drive(t, sink, basebackup.ManifestSinkIndex, basebackup.TablespaceInfo{}, mani, 16); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sink.ManifestBytes(), mani) {
		t.Errorf("ManifestBytes = %q, want %q", sink.ManifestBytes(), mani)
	}
	// AllFiles should not include the manifest pseudo-tablespace.
	if len(sink.AllFiles()) != 1 {
		t.Errorf("AllFiles should exclude manifest sink; got %d entries", len(sink.AllFiles()))
	}
}

func TestTarsink_DataChunks_RemainConsistent_WithRandomSplits(t *testing.T) {
	// Drive the same tar through Sink twice with different chunk sizes.
	// CAS dedup means the second run is all-deduped, and the file
	// chunk-list must be identical.
	r := mathrand.New(mathrand.NewSource(0xDEAD))
	body := make([]byte, 256*1024) // 256 KiB
	r.Read(body)

	sink1, _ := newSinkAndCAS(t)
	tarBytes := buildTar(t, []fileSpec{{name: "f", body: body}})
	if err := drive(t, sink1, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 17); err != nil {
		t.Fatal(err)
	}
	sink2, _ := newSinkAndCAS(t)
	if err := drive(t, sink2, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 9999); err != nil {
		t.Fatal(err)
	}
	if len(sink1.Files(0)) != len(sink2.Files(0)) {
		t.Fatalf("Files len differs: %d vs %d", len(sink1.Files(0)), len(sink2.Files(0)))
	}
	for i := range sink1.Files(0) {
		c1 := sink1.Files(0)[i].Chunks
		c2 := sink2.Files(0)[i].Chunks
		if len(c1) != len(c2) {
			t.Fatalf("file %d chunk count differs: %d vs %d", i, len(c1), len(c2))
		}
		for j := range c1 {
			if c1[j].Hash != c2[j].Hash {
				t.Errorf("file %d chunk %d hash differs across drive splits", i, j)
			}
		}
	}
}

func TestTarsink_MisorderedCallbacks_Erroring(t *testing.T) {
	sink, _ := newSinkAndCAS(t)
	// Data without Start.
	if err := sink.OnTablespaceData(0, []byte("nope")); err == nil {
		t.Error("data without start must error")
	}
	// Start while another active.
	if err := sink.OnTablespaceStart(0, basebackup.TablespaceInfo{}); err != nil {
		t.Fatal(err)
	}
	if err := sink.OnTablespaceStart(1, basebackup.TablespaceInfo{}); err == nil {
		t.Error("nested Start must error")
	}
	// End mismatch.
	if err := sink.OnTablespaceEnd(99); err == nil {
		t.Error("End for wrong idx must error")
	}
	// Clean End to release state.
	if err := sink.OnTablespaceEnd(0); err != nil {
		t.Fatalf("End: %v", err)
	}
}

func TestTarsink_CtxCancel_AbortsParser(t *testing.T) {
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	cas := repo.NewCAS(sp)

	ctx, cancel := context.WithCancel(context.Background())
	sink := tarsink.New(ctx, cas)

	// Build a sizeable tar so the parser is mid-stream when we cancel.
	body := bytes.Repeat([]byte("x"), 4*1024*1024) // 4 MiB
	tarBytes := buildTar(t, []fileSpec{{name: "big", body: body}})

	if err := sink.OnTablespaceStart(0, basebackup.TablespaceInfo{}); err != nil {
		t.Fatal(err)
	}

	// Push the first chunk so the parser starts.
	if err := sink.OnTablespaceData(0, tarBytes[:64]); err != nil {
		t.Fatal(err)
	}

	// Cancel the ctx; subsequent writes / End should surface ctx error.
	cancel()
	// Try a few more writes; one of them should fail OR End should fail.
	var sawErr error
	for off := 64; off < len(tarBytes); off += 4096 {
		end := off + 4096
		if end > len(tarBytes) {
			end = len(tarBytes)
		}
		if err := sink.OnTablespaceData(0, tarBytes[off:end]); err != nil {
			sawErr = err
			break
		}
	}
	if sawErr == nil {
		// All writes succeeded (parser was fast); End should detect ctx cancel.
		sawErr = sink.OnTablespaceEnd(0)
	}
	if sawErr == nil {
		t.Fatal("expected an error after ctx cancel")
	}
	if !errors.Is(sawErr, context.Canceled) && !strings.Contains(sawErr.Error(), "canceled") && !strings.Contains(sawErr.Error(), "closed") {
		t.Logf("got: %v (acceptable: any error indicating cancellation propagated)", sawErr)
	}
}

// TestTarsink_CtxCancel_SurfacesCanceledRootCause locks in the
// contract that, when the parser-side pipe closes due to ctx
// cancellation (not a parser-internal error), the caller observes
// context.Canceled — NOT the wrapped io.ErrClosedPipe symptom. This
// matters because operators reading logs need to see the actual root
// cause, and downstream code may use errors.Is(err, context.Canceled)
// to distinguish "user aborted" from "transport broke".
func TestTarsink_CtxCancel_SurfacesCanceledRootCause(t *testing.T) {
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	cas := repo.NewCAS(sp)

	ctx, cancel := context.WithCancel(context.Background())
	sink := tarsink.New(ctx, cas)

	// Big enough that the parser doesn't drain instantly.
	body := bytes.Repeat([]byte("y"), 8*1024*1024)
	tarBytes := buildTar(t, []fileSpec{{name: "big", body: body}})

	if err := sink.OnTablespaceStart(0, basebackup.TablespaceInfo{}); err != nil {
		t.Fatal(err)
	}

	// Cancel BEFORE feeding any data — this guarantees the next pipe
	// write will see a closed pipe (parser exits on ctx.Err) and our
	// fallback path is the one that fires.
	cancel()

	// Drive the data; one of the writes will return.
	var sawErr error
	for off := 0; off < len(tarBytes); off += 4096 {
		end := off + 4096
		if end > len(tarBytes) {
			end = len(tarBytes)
		}
		if err := sink.OnTablespaceData(0, tarBytes[off:end]); err != nil {
			sawErr = err
			break
		}
	}
	if sawErr == nil {
		// Parser drained everything before we noticed; End surfaces it.
		sawErr = sink.OnTablespaceEnd(0)
	}
	if sawErr == nil {
		t.Fatal("expected an error after ctx cancel; got nil")
	}
	// Either the parser's own ctx.Err propagated through peekParserErr,
	// or our explicit ctx.Err() fallback fired. Either way the chain
	// must include context.Canceled — never a bare "closed pipe".
	if !errors.Is(sawErr, context.Canceled) {
		t.Errorf("expected context.Canceled in error chain; got %v", sawErr)
	}
	if strings.Contains(sawErr.Error(), "closed pipe") {
		t.Errorf("error message should not leak the io.ErrClosedPipe symptom; got %v", sawErr)
	}
}

func TestNew_NilArguments(t *testing.T) {
	cas := repo.NewCAS(&fs.Plugin{})
	defer func() { recover() }()
	tarsink.New(nil, cas) // ctx nil should panic
	t.Error("expected panic on nil ctx")
}

// Exercise that the on-disk CAS chunks really do match SHA-256 of the
// original file bytes — independent of any tar/chunker logic.
func TestTarsink_ChunkContentsHashCorrectly(t *testing.T) {
	sink, cas := newSinkAndCAS(t)
	body := []byte("predictable body for sha verification")
	tarBytes := buildTar(t, []fileSpec{{name: "f", body: body}})
	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 0); err != nil {
		t.Fatal(err)
	}
	files := sink.Files(0)
	if len(files) != 1 {
		t.Fatal("expected exactly one file")
	}

	var rebuilt bytes.Buffer
	for _, ref := range files[0].Chunks {
		got, err := cas.GetChunkBytes(context.Background(), ref.Hash)
		if err != nil {
			t.Fatal(err)
		}
		// Each chunk's hash must match its content exactly (CAS contract).
		if h := repo.Hash(sha256.Sum256(got)); h != ref.Hash {
			t.Errorf("chunk hash drift: stored %s vs declared %s", h, ref.Hash)
		}
		rebuilt.Write(got)
	}
	if !bytes.Equal(rebuilt.Bytes(), body) {
		t.Error("rebuilt bytes do not match original")
	}
}
