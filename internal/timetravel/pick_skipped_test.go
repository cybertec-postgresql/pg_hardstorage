package timetravel

// pick_skipped_test.go — timetravel's seed picker dropped unreadable
// manifests without counting them, and then told the operator the wrong
// thing about why.
//
// pickBackupForTarget is the third copy of "find the newest backup at or
// before X" in the tree (restore.ResolveLatest and
// restore.ResolveBackupForTime are the others). Both of those at least
// tracked a skip count; this one had:
//
//	if lerr != nil || mm == nil {
//		continue
//	}
//
// with no bookkeeping at all. Two distinct lies followed.
//
// If the manifest closest below the target was the unreadable one, an
// OLDER seed was chosen and nothing said so — the same silent
// substitution as the time-target resolver, in the feature whose entire
// purpose is "show me the database as of moment X".
//
// If EVERY manifest was unreadable, bestID stayed empty and the error
// read "no committed backup of %q is at-or-before %s" — telling the
// operator no backup exists that far back, when backups exist and merely
// could not be verified. That sends them to conclude their retention
// window is too short and stop looking, which is the opposite of the
// truth.

import (
	"context"
	"crypto/rand"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

type pickWorld struct {
	sp       storage.StoragePlugin
	repoURL  string
	verifier *backup.Verifier
	signer   *backup.Signer
	store    *backup.ManifestStore
	mgr      *Manager
	ids      []string
}

// newPickWorld commits n manifests one minute apart, oldest first.
func newPickWorld(t *testing.T, n int) *pickWorld {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	store := backup.NewManifestStore(sp)

	base := time.Date(2026, 4, 28, 14, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		id := "db1.full." + ts.Format("20060102T150405Z") + ".000" + string(rune('1'+i))
		m := &backup.Manifest{
			Schema: backup.Schema, BackupID: id, Deployment: "db1",
			Type: backup.BackupTypeFull, PGVersion: 17,
			SystemIdentifier: "7000000000000000001",
			StartLSN:         "0/1000028", StopLSN: "0/2000000", Timeline: 1,
			StartedAt: ts.Add(-time.Minute), StoppedAt: ts,
			BackupLabel: "START WAL LOCATION: 0/1000028\n",
			Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Files:       []backup.FileEntry{},
		}
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	return &pickWorld{sp: sp, repoURL: repoURL, verifier: verifier,
		signer: signer, store: store,
		mgr: NewManager(t.TempDir()+"/state.json", "pg_hardstorage"), ids: ids}
}

func (w *pickWorld) corrupt(t *testing.T, id string) {
	t.Helper()
	key := "manifests/db1/backups/" + id + "/manifest.json"
	body := `{"schema":"broken"}`
	if _, err := w.sp.Put(context.Background(), key, strings.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
}

func TestPickBackupForTarget_SkippedManifestsAreCounted(t *testing.T) {
	w := newPickWorld(t, 3)
	// The newest is the best seed for a target after all of them.
	w.corrupt(t, w.ids[2])

	target := time.Date(2026, 4, 28, 15, 0, 0, 0, time.UTC)
	id, _, skipped, err := w.mgr.pickBackupForTarget(
		context.Background(), w.repoURL, "db1", target, "", w.verifier)
	if err != nil {
		t.Fatalf("pickBackupForTarget: %v", err)
	}
	if id != w.ids[1] {
		t.Fatalf("seed = %q, want the next-best %q", id, w.ids[1])
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1.\n\nWithout the count nothing can tell the operator "+
			"that a CLOSER seed existed and could not be read — in the feature whose whole "+
			"purpose is showing the database as of a specific moment.", skipped)
	}
}

// The error when nothing is usable must distinguish "no backup that far
// back" from "none of them could be read".
func TestPickBackupForTarget_AllUnreadableDoesNotClaimNoBackupExists(t *testing.T) {
	w := newPickWorld(t, 2)
	for _, id := range w.ids {
		w.corrupt(t, id)
	}
	target := time.Date(2026, 4, 28, 15, 0, 0, 0, time.UTC)
	_, _, skipped, err := w.mgr.pickBackupForTarget(
		context.Background(), w.repoURL, "db1", target, "", w.verifier)
	if err == nil {
		t.Fatal("expected an error when every manifest is unreadable")
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
	if strings.Contains(err.Error(), "is at-or-before") {
		t.Errorf("the error claims no backup exists that far back, when two exist and merely "+
			"could not be verified — that sends the operator to conclude their retention "+
			"window is too short and stop looking:\n%v", err)
	}
	if !strings.Contains(err.Error(), "could not be verified") {
		t.Errorf("the error does not say the manifests were unreadable:\n%v", err)
	}
}

// A healthy repo must report zero, or every session carries a warning
// and the signal stops meaning anything.
func TestPickBackupForTarget_HealthyRepoReportsZeroSkipped(t *testing.T) {
	w := newPickWorld(t, 3)
	target := time.Date(2026, 4, 28, 15, 0, 0, 0, time.UTC)
	id, _, skipped, err := w.mgr.pickBackupForTarget(
		context.Background(), w.repoURL, "db1", target, "", w.verifier)
	if err != nil {
		t.Fatalf("pickBackupForTarget: %v", err)
	}
	if id != w.ids[2] || skipped != 0 {
		t.Fatalf("got (%q, skipped=%d), want (%q, 0)", id, skipped, w.ids[2])
	}
}

// "No backup that far back" must keep its own, accurate message when
// nothing was skipped.
func TestPickBackupForTarget_GenuinelyTooEarlyKeepsItsMessage(t *testing.T) {
	w := newPickWorld(t, 2)
	target := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	_, _, skipped, err := w.mgr.pickBackupForTarget(
		context.Background(), w.repoURL, "db1", target, "", w.verifier)
	if err == nil {
		t.Fatal("expected a refusal for a target before every backup")
	}
	if skipped != 0 {
		t.Errorf("skipped = %d on a healthy repo", skipped)
	}
	if !strings.Contains(err.Error(), "is at-or-before") {
		t.Errorf("the accurate too-early message was lost:\n%v", err)
	}
}

// A manifest legitimately filtered out for being NEWER than the target
// is not a skip — it was read and evaluated. Counting it would make
// every ordinary session report a warning, and a warning that always
// fires carries no information.
func TestPickBackupForTarget_TooNewManifestsAreNotCountedAsSkips(t *testing.T) {
	w := newPickWorld(t, 3) // stops at 14:00, 14:01, 14:02

	// Target just after the first: the other two are legitimately too new.
	target := time.Date(2026, 4, 28, 14, 0, 30, 0, time.UTC)
	id, _, skipped, err := w.mgr.pickBackupForTarget(
		context.Background(), w.repoURL, "db1", target, "", w.verifier)
	if err != nil {
		t.Fatalf("pickBackupForTarget: %v", err)
	}
	if id != w.ids[0] {
		t.Fatalf("seed = %q, want the only backup at-or-before the target (%q)", id, w.ids[0])
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d on a healthy repo where 2 manifests were merely too new.\n\n"+
			"Those were read and evaluated; they are the normal case. Counting them makes "+
			"every session warn, and a warning that always fires is noise.", skipped)
	}
}

// commitWithStopLSN plants a correctly-signed manifest carrying an
// arbitrary stop_lsn. Manifest.Validate requires stop_lsn to be
// non-empty and only compares it to start_lsn WHEN BOTH PARSE, so a
// malformed value commits cleanly — which is what makes timetravel's
// LSN branch reachable with a manifest that verifies.
func (w *pickWorld) commitWithStopLSN(t *testing.T, id, stopLSN string, ts time.Time) {
	t.Helper()
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: id, Deployment: "db1",
		Type: backup.BackupTypeFull, PGVersion: 17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/1000028", StopLSN: stopLSN, Timeline: 1,
		StartedAt: ts.Add(-time.Minute), StoppedAt: ts,
		BackupLabel: "START WAL LOCATION: 0/1000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:       []backup.FileEntry{},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit %s: %v", id, err)
	}
}

// In LSN mode, a manifest whose stop_lsn will not parse is one that
// could not be EVALUATED — not one that stops past the target. Both
// lived behind a single `continue`, so a malformed stop_lsn was silently
// treated as "too new" and never counted, and the operator was never
// told a candidate had been discarded for being unreadable.
func TestPickBackupForTarget_UnparseableStopLSNIsCountedNotTreatedAsTooNew(t *testing.T) {
	w := newPickWorld(t, 1) // one good manifest, stop_lsn 0/2000000
	w.commitWithStopLSN(t, "db1.full.20260428T141500Z.9999", "not-an-lsn",
		time.Date(2026, 4, 28, 14, 15, 0, 0, time.UTC))

	// Target above the good manifest's stop_lsn: it is a valid seed.
	id, _, skipped, err := w.mgr.pickBackupForTarget(
		context.Background(), w.repoURL, "db1", time.Time{}, "0/3000000", w.verifier)
	if err != nil {
		t.Fatalf("pickBackupForTarget: %v", err)
	}
	if id != w.ids[0] {
		t.Fatalf("seed = %q, want the parseable manifest %q", id, w.ids[0])
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1.\n\nThe manifest with the malformed stop_lsn could "+
			"not be evaluated, but it was folded into the ordinary stops-past-the-target "+
			"filter and vanished silently. Manifest.Validate accepts a malformed stop_lsn "+
			"(it only compares LSNs when both parse), so this manifest verifies and commits "+
			"cleanly — the branch is reachable.", skipped)
	}
}
