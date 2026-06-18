package pdfwriter_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/pdf/pdfwriter"
)

// renderToBytes is the standard "make a PDF" helper for tests.
func renderToBytes(t *testing.T, build func(d *pdfwriter.Doc), opts pdfwriter.Options) []byte {
	t.Helper()
	d := pdfwriter.New(opts)
	build(d)
	var buf bytes.Buffer
	if _, err := d.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return buf.Bytes()
}

// ----- shape-of-output -----

func TestWriter_EmptyDocRejected(t *testing.T) {
	d := pdfwriter.New(pdfwriter.Options{})
	var buf bytes.Buffer
	if _, err := d.WriteTo(&buf); err == nil {
		t.Errorf("expected error on empty doc")
	}
}

func TestWriter_HeaderAndTrailer(t *testing.T) {
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddTitle("Hello")
		d.AddParagraph("Line one.")
	}, pdfwriter.Options{})
	if !bytes.HasPrefix(body, []byte("%PDF-1.4\n")) {
		t.Errorf("missing PDF header: %q", body[:min(len(body), 32)])
	}
	if !bytes.Contains(body, []byte("%%EOF")) {
		t.Errorf("missing %%EOF trailer")
	}
	if !bytes.Contains(body, []byte("startxref")) {
		t.Errorf("missing startxref")
	}
	if !bytes.Contains(body, []byte("xref\n0 ")) {
		t.Errorf("missing xref table")
	}
}

func TestWriter_CatalogPagesFontObjects(t *testing.T) {
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddParagraph("body")
	}, pdfwriter.Options{})
	for _, want := range []string{
		"/Type /Catalog",
		"/Type /Pages",
		"/Type /Page ",
		"/BaseFont /Helvetica",
		"/BaseFont /Courier",
		"/Encoding /WinAnsiEncoding",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("missing %q in body", want)
		}
	}
}

// TestWriter_StringEscapes exercises the three special characters
// (parens + backslash) that must escape inside a PDF literal string.
func TestWriter_StringEscapes(t *testing.T) {
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddParagraph(`A (paren) and a \ backslash.`)
	}, pdfwriter.Options{})
	// The literal must contain the escaped forms (`\(`, `\)`, `\\`)
	// and NOT contain unescaped (paren) bytes inside the BT/ET text
	// operator block.
	if !bytes.Contains(body, []byte(`\(paren\)`)) {
		t.Errorf("paren escapes missing")
	}
	if !bytes.Contains(body, []byte(`\\ backslash`)) {
		t.Errorf("backslash escape missing")
	}
}

// TestWriter_Encoding_NonLatinReplacedWithQuestion: a non-Latin-1
// rune must NOT corrupt the output; the writer replaces it with `?`
// and fires the warning callback.
func TestWriter_Encoding_NonLatinReplacedWithQuestion(t *testing.T) {
	var warned []rune
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddParagraph("emoji rocket: 🚀")
	}, pdfwriter.Options{
		OnEncodeWarn: func(r rune) { warned = append(warned, r) },
	})
	if !bytes.Contains(body, []byte("rocket: ?")) {
		t.Errorf("expected `?` substitution; body excerpt:\n%s", excerpt(body, "rocket"))
	}
	if len(warned) != 1 || warned[0] != '🚀' {
		t.Errorf("warned = %v, want [🚀]", warned)
	}
}

// ----- pagination -----

func TestWriter_PageBreakProducesTwoPages(t *testing.T) {
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddTitle("first")
		d.AddPageBreak()
		d.AddTitle("second")
	}, pdfwriter.Options{})
	pageCount := bytes.Count(body, []byte("/Type /Page "))
	if pageCount != 2 {
		t.Errorf("page count = %d, want 2", pageCount)
	}
	// Pages root /Count must agree.
	if !bytes.Contains(body, []byte("/Type /Pages /Count 2 ")) {
		t.Errorf("/Count mismatch in /Pages dict")
	}
}

func TestWriter_AutoPaginatesWhenContentOverflows(t *testing.T) {
	// 200 paragraphs at 11pt body in a US-Letter page should
	// trigger at least one auto page-break.
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddTitle("overflow")
		for i := 0; i < 200; i++ {
			d.AddParagraph("A line that occupies space on the page.")
		}
	}, pdfwriter.Options{})
	pageCount := bytes.Count(body, []byte("/Type /Page "))
	if pageCount < 2 {
		t.Errorf("expected auto-pagination; got %d pages", pageCount)
	}
}

// ----- tables -----

func TestWriter_AddTable_ProducesHeaderAndRows(t *testing.T) {
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddHeading2("Affected backups")
		d.AddTable(
			[]string{"Deployment", "BackupID", "Tenant"},
			[][]string{
				{"db1", "db1.full.x", "tenant-a"},
				{"db2", "db2.full.y", "tenant-a"},
			},
		)
	}, pdfwriter.Options{})
	// Each cell value must end up rendered.
	for _, want := range []string{"Deployment", "BackupID", "Tenant",
		"db1", "db1.full.x", "tenant-a", "db2.full.y"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("missing cell %q", want)
		}
	}
	// A horizontal line below the header (the writer emits 0.5 w
	// line-width + an "S" stroke op).
	if !bytes.Contains(body, []byte(" S\n")) {
		t.Errorf("expected stroke op for header underline")
	}
}

// ----- footer -----

func TestWriter_FooterAppearsOnEveryPage(t *testing.T) {
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.SetFooter("pg_hardstorage compliance report")
		d.AddTitle("p1")
		d.AddPageBreak()
		d.AddTitle("p2")
	}, pdfwriter.Options{})
	occurrences := bytes.Count(body, []byte("compliance report"))
	if occurrences < 2 {
		t.Errorf("footer occurrences = %d, want ≥ 2 (one per page)", occurrences)
	}
}

// ----- compression -----

func TestWriter_CompressedHasFlateFilter(t *testing.T) {
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddTitle("compressed")
		for i := 0; i < 20; i++ {
			d.AddParagraph(strings.Repeat("Lorem ipsum dolor sit amet. ", 8))
		}
	}, pdfwriter.Options{Compress: true})
	if !bytes.Contains(body, []byte("/Filter /FlateDecode")) {
		t.Errorf("expected /Filter /FlateDecode in content-stream dict")
	}
}

func TestWriter_UncompressedHasNoFilter(t *testing.T) {
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddTitle("uncompressed")
		d.AddParagraph("hello")
	}, pdfwriter.Options{})
	if bytes.Contains(body, []byte("/FlateDecode")) {
		t.Errorf("uncompressed doc should not declare /FlateDecode")
	}
}

// ----- structural integrity -----

// TestWriter_XrefOffsets_AreMonotonic asserts the byte offsets in
// the xref table strictly increase (modulo the free-list slot 0).
// A regression that miscomputes any offset would break readers.
func TestWriter_XrefOffsets_AreMonotonic(t *testing.T) {
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddTitle("a")
		d.AddPageBreak()
		d.AddTitle("b")
	}, pdfwriter.Options{})
	xrefIdx := bytes.Index(body, []byte("xref\n0 "))
	if xrefIdx < 0 {
		t.Fatal("no xref table")
	}
	tail := body[xrefIdx:]
	// First data row is the slot-0 free entry; subsequent rows are
	// 10-digit offsets followed by " 00000 n ".
	rows := bytes.Split(tail, []byte("\n"))
	prev := int64(-1)
	for _, r := range rows {
		if !bytes.Contains(r, []byte(" 00000 n ")) {
			continue
		}
		if len(r) < 10 {
			continue
		}
		var n int64
		for i := 0; i < 10; i++ {
			n = n*10 + int64(r[i]-'0')
		}
		if n <= prev {
			t.Errorf("non-monotonic offset: %d after %d", n, prev)
		}
		prev = n
	}
	if prev <= 0 {
		t.Errorf("no n-rows found in xref")
	}
}

// TestWriter_TrailerSizeMatchesObjectCount asserts the /Size value
// in the trailer dict equals the number of objects we declared.
func TestWriter_TrailerSizeMatchesObjectCount(t *testing.T) {
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddTitle("a")
	}, pdfwriter.Options{})
	// We declare: 1 Catalog + 1 Pages + 2 Fonts + 1 Page + 1 Content
	// = 6, plus the implicit 0-th = /Size 7.
	if !bytes.Contains(body, []byte("/Size 7 ")) {
		t.Errorf("expected /Size 7 in trailer; body excerpt:\n%s",
			excerpt(body, "trailer"))
	}
}

// TestWriter_PreformattedUsesCourier checks the AddPreformatted code
// path picks the Courier font (F2) rather than Helvetica (F1).
func TestWriter_PreformattedUsesCourier(t *testing.T) {
	body := renderToBytes(t, func(d *pdfwriter.Doc) {
		d.AddPreformatted("monospace")
	}, pdfwriter.Options{})
	if !bytes.Contains(body, []byte("/F2 11.00 Tf")) {
		t.Errorf("preformatted should use F2 (Courier); body excerpt:\n%s",
			excerpt(body, "monospace"))
	}
}

// ----- helpers -----

func excerpt(body []byte, near string) string {
	idx := bytes.Index(body, []byte(near))
	if idx < 0 {
		return string(body[:min(len(body), 200)])
	}
	start := max(0, idx-80)
	end := min(len(body), idx+200)
	return string(body[start:end])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
