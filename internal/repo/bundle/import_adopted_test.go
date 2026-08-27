package bundle_test

// import_adopted_test.go — the import half of the dedup-vs-GC race.
//
// A chunk the import ADOPTS (already present in the destination repo,
// so putIfNotExists writes nothing) is an orphan there until a
// manifest from this bundle claims it — and gc --apply computes its
// delete list from a reference snapshot that can predate that
// manifest. The backup runner, the walsink and replicate all re-check
// their adopted chunks at commit time; import is a committer too, and
// had no gate: a sweep firing mid-import produced an import that
// reported success over a manifest whose chunk was gone.
//
// The tar is single-pass, so the gate cannot rewrite a swept chunk —
// it FAILS LOUDLY instead, and the remedy is re-running the import,
// which is idempotent and writes the now-absent chunk for real. Both
// halves are asserted here.

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/bundle"
)

// sweepOnAdoptSP lets the adoption Stat of one chunk key succeed and
// then deletes the object — the tightest gc interleaving: adopted,
// then gone. Same shape as replicate's test double.
type sweepOnAdoptSP struct {
	storage.StoragePlugin
	key   string
	fired atomic.Bool
}

func (p *sweepOnAdoptSP) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	info, err := p.StoragePlugin.Stat(ctx, key)
	if err == nil && key == p.key && p.fired.CompareAndSwap(false, true) {
		if derr := p.StoragePlugin.Delete(ctx, key); derr != nil {
			return info, derr
		}
	}
	return info, err
}

func TestImport_AdoptedChunkSweptMidImport_FailsLoudAndRerunHeals(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	m := sampleManifest(t, src, "db1.full.20260428T120000Z")
	commitManifest(t, src, m)

	var buf bytes.Buffer
	if _, err := bundle.Export(ctx, src, &buf, bundle.ExportOptions{Deployment: "db1"}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	tarBytes := buf.Bytes()

	// Destination repo that ALREADY holds the first chunk — the
	// import will adopt rather than write it.
	dst := newRepo(t)
	sweptHash, _ := putChunk(t, dst, []byte("chunk-alpha-bytes"))
	sweptKey := repo.ChunkKey(sweptHash)

	hooked := &sweepOnAdoptSP{StoragePlugin: dst, key: sweptKey}
	_, err := bundle.Import(ctx, bytes.NewReader(tarBytes), hooked, bundle.ImportOptions{})
	if err == nil {
		t.Fatalf("import reported success while an adopted chunk was swept mid-import — " +
			"the resulting repo has a manifest over a missing chunk and restore will fail")
	}
	for _, want := range []string{sweptKey, "re-run the import", "repo gc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("gate error does not mention %q — the operator cannot act on it:\n%v", want, err)
		}
	}

	// The remedy the error promises must actually work: the re-run
	// finds the chunk absent, writes it for real, and succeeds.
	if _, err := bundle.Import(ctx, bytes.NewReader(tarBytes), dst, bundle.ImportOptions{}); err != nil {
		t.Fatalf("re-import did not heal: %v", err)
	}
	if _, err := dst.Stat(ctx, sweptKey); err != nil {
		t.Fatalf("chunk still missing after the healing re-import: %v", err)
	}
}

func TestImport_AdoptionWithoutSweepIsSilent(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	m := sampleManifest(t, src, "db1.full.20260428T120000Z")
	commitManifest(t, src, m)
	var buf bytes.Buffer
	if _, err := bundle.Export(ctx, src, &buf, bundle.ExportOptions{Deployment: "db1"}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	dst := newRepo(t)
	putChunk(t, dst, []byte("chunk-alpha-bytes")) // adopted, never swept
	if _, err := bundle.Import(ctx, bytes.NewReader(buf.Bytes()), dst, bundle.ImportOptions{}); err != nil {
		t.Fatalf("ordinary adoption must not trip the gate: %v", err)
	}
}
