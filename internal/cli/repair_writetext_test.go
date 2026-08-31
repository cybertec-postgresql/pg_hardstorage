package cli

// The text renderers for `repair chunks` and `repair scrub` are what an
// operator actually reads during an incident — the JSON body is for
// tooling. Nothing executed them, and the gap had already produced a
// defect: repairChunksBody dropped the ENTIRE hash list once it exceeded
// 50 entries and said nothing about it, so a repo with 51 missing chunks
// printed strictly less than one with 50.
//
// These tests pin the properties that make the text output trustworthy:
// a corruption verdict is always stated, and any list the renderer
// shortens announces that it did.

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func hashes(n int) []string {
	out := make([]string, n)
	for i := range out {
		var h repo.Hash
		h[0], h[1] = byte(i>>8), byte(i)
		out[i] = h.String()
	}
	return out
}

func mustRender(t *testing.T, w func(*strings.Builder) error) string {
	t.Helper()
	var sb strings.Builder
	if err := w(&sb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return sb.String()
}

// The regression: over the cap, the renderer must still show hashes AND
// account for the ones it withheld.
func TestRepairChunksWriteText_LongListIsCappedButNotSilent(t *testing.T) {
	const total = 137
	b := repairChunksBody{
		Mode: "missing", DryRun: true, Total: total, RefCount: 900,
		Chunks: hashes(total),
	}
	out := mustRender(t, func(sb *strings.Builder) error { return b.WriteText(sb) })

	if !strings.Contains(out, "hashes:") {
		t.Fatalf("no hashes rendered for a %d-chunk finding — the operator sees a corruption "+
			"verdict with nothing to act on:\n%s", total, out)
	}
	shown := strings.Count(out, "\n    ") - strings.Count(out, "\n    ...")
	if shown != maxHashLines {
		t.Errorf("rendered %d hash lines, want %d", shown, maxHashLines)
	}
	if want := "+87 more"; !strings.Contains(out, want) {
		t.Errorf("output does not announce the %d withheld hashes (want %q) — a silent cap "+
			"reads as \"that is all of them\":\n%s", total-maxHashLines, want, out)
	}
	if !strings.Contains(out, "--json") {
		t.Errorf("the truncation notice must point at where the full list lives:\n%s", out)
	}
}

// Under the cap nothing is withheld, so nothing must claim otherwise.
func TestRepairChunksWriteText_ShortListHasNoTruncationNotice(t *testing.T) {
	b := repairChunksBody{Mode: "missing", Total: 3, RefCount: 10, Chunks: hashes(3)}
	out := mustRender(t, func(sb *strings.Builder) error { return b.WriteText(sb) })

	if strings.Contains(out, "more") {
		t.Errorf("a complete list must not carry a truncation notice:\n%s", out)
	}
	if got := strings.Count(out, "\n    "); got != 3 {
		t.Errorf("rendered %d hash lines, want 3:\n%s", got, out)
	}
}

// The verdict line is the part an operator scans for. Missing chunks are
// unrecoverable data loss; the text output must say so, every time.
func TestRepairChunksWriteText_MissingStatesTheCorruption(t *testing.T) {
	b := repairChunksBody{Mode: "missing", Total: 1, RefCount: 10, Chunks: hashes(1)}
	out := mustRender(t, func(sb *strings.Builder) error { return b.WriteText(sb) })
	if !strings.Contains(out, "restores referencing these chunks will fail") {
		t.Errorf("a missing-chunk finding must state the consequence:\n%s", out)
	}

	// ...and must NOT say it when there is nothing missing.
	clean := repairChunksBody{Mode: "missing", Total: 0, RefCount: 10}
	cout := mustRender(t, func(sb *strings.Builder) error { return clean.WriteText(sb) })
	if strings.Contains(cout, "real corruption") {
		t.Errorf("a clean repository must not be reported as corrupt:\n%s", cout)
	}
}

// Orphan mode must distinguish "found, not touched" from "deleted" — an
// operator reading dry-run output as applied would assume the space was
// reclaimed.
func TestRepairChunksWriteText_OrphansDistinguishesDryRunFromApplied(t *testing.T) {
	dry := repairChunksBody{Mode: "orphans", DryRun: true, Total: 5, RefCount: 10, Chunks: hashes(5)}
	dout := mustRender(t, func(sb *strings.Builder) error { return dry.WriteText(sb) })
	if !strings.Contains(dout, "dry-run") || strings.Contains(dout, "deleted") {
		t.Errorf("dry-run output must say dry-run and must not claim deletion:\n%s", dout)
	}

	applied := repairChunksBody{Mode: "orphans", Total: 5, Applied: 5, RefCount: 10, Chunks: hashes(5)}
	aout := mustRender(t, func(sb *strings.Builder) error { return applied.WriteText(sb) })
	if !strings.Contains(aout, "5 deleted") || strings.Contains(aout, "dry-run") {
		t.Errorf("applied output must report the deletion count and not say dry-run:\n%s", aout)
	}
}

func TestRepairScrubWriteText_MismatchListIsCappedButNotSilent(t *testing.T) {
	const total = 63
	b := repairScrubBody{
		Sampled: 1000, ReferencedTotal: 1000, BytesVerified: 1 << 30,
		MismatchCount: total, Mismatches: hashes(total),
	}
	out := mustRender(t, func(sb *strings.Builder) error { return b.WriteText(sb) })

	if !strings.Contains(out, "failed integrity check") {
		t.Fatalf("scrub must state the integrity failure:\n%s", out)
	}
	if want := "+13 more"; !strings.Contains(out, want) {
		t.Errorf("scrub withheld %d hashes without saying so (want %q):\n%s",
			total-maxHashLines, want, out)
	}
}

func TestRepairScrubWriteText_CleanRunSaysClean(t *testing.T) {
	b := repairScrubBody{Sampled: 100, ReferencedTotal: 100, BytesVerified: 4096}
	out := mustRender(t, func(sb *strings.Builder) error { return b.WriteText(sb) })
	if !strings.Contains(out, "no integrity failures") {
		t.Errorf("a clean scrub must say so explicitly:\n%s", out)
	}
	if strings.Contains(out, "✗") {
		t.Errorf("a clean scrub must not render a failure marker:\n%s", out)
	}
}
