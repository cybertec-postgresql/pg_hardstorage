// Package pdfwriter is a minimal pure-Go PDF generator scoped to
// what pg_hardstorage's compliance / DSA / integrity reports need:
// pages with multi-line text blocks, simple tables (text rows
// separated by ruled lines), and internal hyperlinks-via-page-
// numbers.  It deliberately does NOT support: embedded fonts beyond
// the standard 14, raster images, vector graphics beyond rectangles
// + lines, encryption, forms, JavaScript, attachments.
//
// The on-the-wire format is PDF 1.4, the simplest revision that
// every modern reader (Acrobat, Preview, Foxit, Chromium, Firefox)
// understands and that doesn't require encryption-related dictionary
// keys.  We use one of the standard Type-1 fonts (Helvetica) which
// every PDF reader ships built-in — no font files are embedded.
//
// String encoding: the writer emits PDFDocEncoding (Latin-1 with a
// few PDF-specific glyphs).  Non-Latin-1 input bytes are replaced
// with `?` after a warning callback fires.  This is conservative
// but sufficient for the structured-report use case (status codes,
// hex digests, RFC3339 timestamps, English prose).
//
// Cross-reference table style: classic xref table (not an xref
// stream), terminated with `startxref` + `%%EOF`.  Object offsets
// are tracked precisely as bytes are written — every Object writes
// `%d 0 obj\n…\nendobj\n` and we record the leading-byte offset.
//
// The Doc API is intentionally tiny:
//
//	d := pdfwriter.New(opts)
//	d.AddTitle("pg_hardstorage compliance report")
//	d.AddParagraph("As of 2026-05-02…")
//	d.AddTable(headers, rows)
//	d.AddPageBreak()
//	d.AddHeading2("Per-deployment rollup")
//	…
//	if err := d.WriteTo(w); err != nil { … }
//
// Page layout is single-column, fixed letter-paper margins.  An
// operator who needs landscape / multi-column layout uses the HTML
// renderer + an external HTML→PDF converter (wkhtmltopdf / headless
// chrome).
package pdfwriter

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// PageSize defines the printable canvas.
type PageSize struct {
	Width, Height float64 // PDF points (1pt = 1/72 in)
}

// Standard sizes in PDF points.
var (
	PageSizeLetter = PageSize{612, 792}
	PageSizeA4     = PageSize{595, 842}
)

// Options carries Doc configuration.  The zero value picks
// US-Letter, 0.75-inch margins, 11pt body text, and Helvetica.
type Options struct {
	Page         PageSize
	MarginLeft   float64
	MarginRight  float64
	MarginTop    float64
	MarginBottom float64
	BodyFontSize float64
	HeadingFont  float64 // size for headings; 0 → derived from body
	Title        string
	Subtitle     string
	Compress     bool       // zlib-compress page content streams
	OnEncodeWarn func(rune) // called on each non-encodable rune; nil = silent
}

func (o *Options) defaulted() Options {
	out := *o
	if out.Page == (PageSize{}) {
		out.Page = PageSizeLetter
	}
	if out.MarginLeft == 0 {
		out.MarginLeft = 54 // 0.75 in
	}
	if out.MarginRight == 0 {
		out.MarginRight = 54
	}
	if out.MarginTop == 0 {
		out.MarginTop = 54
	}
	if out.MarginBottom == 0 {
		out.MarginBottom = 54
	}
	if out.BodyFontSize == 0 {
		out.BodyFontSize = 11
	}
	if out.HeadingFont == 0 {
		out.HeadingFont = 14
	}
	return out
}

// Doc accumulates page content.  Build one, append to it via the
// AddXxx methods, then call WriteTo to emit a complete PDF stream.
type Doc struct {
	opts    Options
	current *page
	pages   []*page
	footer  string
}

// page is the in-memory representation of one rendered page.  The
// content stream is built up via PDF operators ("BT", "Tf", "Td",
// "Tj", "ET", "m", "l", "S") as text + lines are added.
type page struct {
	content strings.Builder
	yCursor float64 // current text-baseline Y (top-to-bottom math)
}

// New constructs a Doc with options applied.
func New(opts Options) *Doc {
	return &Doc{opts: opts.defaulted()}
}

// SetFooter attaches a single-line footer to every page (page-N
// numbering can be added by the caller via "%d / %d" template).
// Footer is appended to existing pages when the next page is added,
// so call it before AddPageBreak / AddPage / first AddXxx.
func (d *Doc) SetFooter(s string) { d.footer = s }

// startPage opens a fresh page if there's no current one.
func (d *Doc) startPage() {
	if d.current != nil {
		return
	}
	p := &page{}
	p.yCursor = d.opts.Page.Height - d.opts.MarginTop
	d.current = p
	d.pages = append(d.pages, p)
}

// AddPageBreak forces the next AddXxx call to land on a new page.
func (d *Doc) AddPageBreak() { d.current = nil }

// AddTitle places a centred title near the top of the next page.
// Subsequent text continues below it.
func (d *Doc) AddTitle(s string) {
	d.startPage()
	size := d.opts.HeadingFont * 1.6
	x := d.opts.Page.Width / 2
	y := d.current.yCursor
	encoded := d.encode(s)
	textWidth := approxWidth(encoded, size)
	d.current.content.WriteString(fmt.Sprintf("BT /F1 %.2f Tf %.2f %.2f Td (%s) Tj ET\n",
		size, x-textWidth/2, y, escape(encoded)))
	d.current.yCursor -= size * 1.4
}

// AddHeading1 adds a heading at the next baseline.
func (d *Doc) AddHeading1(s string) { d.heading(s, d.opts.HeadingFont*1.25) }

// AddHeading2 adds a sub-heading.
func (d *Doc) AddHeading2(s string) { d.heading(s, d.opts.HeadingFont) }

func (d *Doc) heading(s string, size float64) {
	d.ensureRoom(size * 1.6)
	encoded := d.encode(s)
	d.current.content.WriteString(fmt.Sprintf("BT /F1 %.2f Tf %.2f %.2f Td (%s) Tj ET\n",
		size, d.opts.MarginLeft, d.current.yCursor, escape(encoded)))
	d.current.yCursor -= size * 1.4
}

// AddParagraph wraps and writes a paragraph at body-font size.
func (d *Doc) AddParagraph(s string) {
	if s == "" {
		d.current = d.ensurePage(d.opts.BodyFontSize * 1.2)
		d.current.yCursor -= d.opts.BodyFontSize * 0.6
		return
	}
	width := d.opts.Page.Width - d.opts.MarginLeft - d.opts.MarginRight
	for _, line := range wrapLines(d.encode(s), width, d.opts.BodyFontSize) {
		d.ensureRoom(d.opts.BodyFontSize * 1.4)
		d.current.content.WriteString(fmt.Sprintf("BT /F1 %.2f Tf %.2f %.2f Td (%s) Tj ET\n",
			d.opts.BodyFontSize, d.opts.MarginLeft, d.current.yCursor,
			escape(line)))
		d.current.yCursor -= d.opts.BodyFontSize * 1.3
	}
	// Trailing paragraph-spacing.
	d.current.yCursor -= d.opts.BodyFontSize * 0.4
}

// AddPreformatted writes monospaced text (e.g. a code block).  Each
// input line is written verbatim at the body font size; no wrapping.
func (d *Doc) AddPreformatted(s string) {
	for _, raw := range strings.Split(s, "\n") {
		d.ensureRoom(d.opts.BodyFontSize * 1.3)
		d.current.content.WriteString(fmt.Sprintf("BT /F2 %.2f Tf %.2f %.2f Td (%s) Tj ET\n",
			d.opts.BodyFontSize, d.opts.MarginLeft, d.current.yCursor,
			escape(d.encode(raw))))
		d.current.yCursor -= d.opts.BodyFontSize * 1.2
	}
	d.current.yCursor -= d.opts.BodyFontSize * 0.4
}

// AddTable writes a simple text table.  Each row is one line; the
// caller supplies headers + body rows.  Column widths are computed
// proportionally from the longest cell in each column.
func (d *Doc) AddTable(headers []string, rows [][]string) {
	if len(headers) == 0 && len(rows) == 0 {
		return
	}
	encodedHeaders := d.encodeRow(headers)
	encodedRows := make([][]string, len(rows))
	for i := range rows {
		encodedRows[i] = d.encodeRow(rows[i])
	}
	cols := len(headers)
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return
	}
	// Column widths are proportional to the widest cell in the
	// column, capped to the printable region.
	colWidths := make([]int, cols)
	for i, h := range encodedHeaders {
		if len(h) > colWidths[i] {
			colWidths[i] = len(h)
		}
	}
	for _, r := range encodedRows {
		for i, c := range r {
			if i < cols && len(c) > colWidths[i] {
				colWidths[i] = len(c)
			}
		}
	}
	totalChars := 0
	for _, w := range colWidths {
		totalChars += w
	}
	if totalChars == 0 {
		return
	}
	printable := d.opts.Page.Width - d.opts.MarginLeft - d.opts.MarginRight
	colXs := make([]float64, cols+1)
	colXs[0] = d.opts.MarginLeft
	for i, w := range colWidths {
		colXs[i+1] = colXs[i] + (float64(w)/float64(totalChars))*printable
	}
	rowHeight := d.opts.BodyFontSize * 1.5

	// Header row + separator.
	d.ensureRoom(rowHeight * 2)
	headerY := d.current.yCursor
	for i, h := range encodedHeaders {
		d.current.content.WriteString(fmt.Sprintf("BT /F1 %.2f Tf %.2f %.2f Td (%s) Tj ET\n",
			d.opts.BodyFontSize, colXs[i]+2, headerY, escape(h)))
	}
	d.current.yCursor -= rowHeight
	d.drawHorizontalLine(d.opts.MarginLeft, d.opts.Page.Width-d.opts.MarginRight, d.current.yCursor+rowHeight*0.2)
	d.current.yCursor -= rowHeight * 0.3

	// Body rows.
	for _, r := range encodedRows {
		d.ensureRoom(rowHeight)
		y := d.current.yCursor
		for i, c := range r {
			if i >= cols {
				break
			}
			d.current.content.WriteString(fmt.Sprintf("BT /F1 %.2f Tf %.2f %.2f Td (%s) Tj ET\n",
				d.opts.BodyFontSize, colXs[i]+2, y, escape(c)))
		}
		d.current.yCursor -= rowHeight
	}
	d.current.yCursor -= d.opts.BodyFontSize * 0.4
}

// drawHorizontalLine emits a thin line at y from x1 to x2.
func (d *Doc) drawHorizontalLine(x1, x2, y float64) {
	d.current.content.WriteString(fmt.Sprintf("0.5 w %.2f %.2f m %.2f %.2f l S\n",
		x1, y, x2, y))
}

// ensurePage returns the active page, starting one if needed.
func (d *Doc) ensurePage(_ float64) *page {
	d.startPage()
	return d.current
}

// ensureRoom pages-breaks when there isn't enough vertical space
// for the next content block.
func (d *Doc) ensureRoom(needed float64) {
	d.startPage()
	if d.current.yCursor-needed < d.opts.MarginBottom {
		d.AddPageBreak()
		d.startPage()
	}
}

// encodeRow encodes every cell.
func (d *Doc) encodeRow(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = d.encode(s)
	}
	return out
}

// encode replaces non-encodable runes with '?' (after firing the
// warning callback).  Latin-1 (PDFDocEncoding) is sufficient for
// the structured-report use case.
func (d *Doc) encode(s string) string {
	if !utf8.ValidString(s) || isPureASCII(s) {
		// Pure ASCII passes through; invalid UTF-8 also passes
		// through (caller's responsibility).
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x100 && r >= 0 {
			b.WriteRune(r)
			continue
		}
		if d.opts.OnEncodeWarn != nil {
			d.opts.OnEncodeWarn(r)
		}
		b.WriteByte('?')
	}
	return b.String()
}

func isPureASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7F {
			return false
		}
	}
	return true
}

// approxWidth estimates text width at a given size.  For Helvetica
// at 11pt the average glyph is ~5.5 pt wide.  Sufficient for centring
// titles; the writer doesn't need precise metrics for everything else.
func approxWidth(s string, size float64) float64 {
	return float64(len(s)) * size * 0.5
}

// wrapLines splits s into printable-width lines.  Word-wrap at
// spaces; oversized words break at the column edge.
func wrapLines(s string, maxWidth, fontSize float64) []string {
	maxChars := int(maxWidth / (fontSize * 0.5))
	if maxChars <= 0 {
		return []string{s}
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			out = append(out, "")
			continue
		}
		words := strings.Fields(line)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		current := ""
		for _, w := range words {
			candidate := w
			if current != "" {
				candidate = current + " " + w
			}
			if len(candidate) > maxChars && current != "" {
				out = append(out, current)
				current = w
				continue
			}
			current = candidate
		}
		if current != "" {
			out = append(out, current)
		}
	}
	return out
}

// escape escapes the four PDF-string special characters.
func escape(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`(`, `\(`,
		`)`, `\)`,
	)
	return r.Replace(s)
}

// ----- output -----

// WriteTo serialises the Doc to w.  Every Doc must have at least
// one page; an empty Doc is rejected so callers don't accidentally
// produce invalid PDFs.
func (d *Doc) WriteTo(w io.Writer) (int64, error) {
	if len(d.pages) == 0 {
		return 0, errors.New("pdfwriter: empty document (no pages)")
	}
	cw := &countingWriter{w: w}

	// Object 0 is implicit (free).  Real objects start at 1.
	// Layout we'll emit:
	//   1 = Catalog
	//   2 = Pages (root)
	//   3 = Font (Helvetica)
	//   4 = Font (Courier — for AddPreformatted)
	//   5..(5+N-1) = Page objects
	//   5+N..(5+2N-1) = Content streams (one per page)
	const (
		catalogObjID   = 1
		pagesObjID     = 2
		helveticaObjID = 3
		courierObjID   = 4
		firstPageObjID = 5
	)
	N := len(d.pages)
	contentBaseID := firstPageObjID + N

	objects := make(map[int]string)

	// Catalog
	objects[catalogObjID] = fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pagesObjID)

	// Pages root with /Kids array of every page reference
	var kids strings.Builder
	for i := 0; i < N; i++ {
		fmt.Fprintf(&kids, "%d 0 R ", firstPageObjID+i)
	}
	objects[pagesObjID] = fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>",
		N, strings.TrimSpace(kids.String()))

	// Standard fonts.  /BaseFont names are reserved by the
	// PDF spec — every conforming reader ships them.
	objects[helveticaObjID] = `<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>`
	objects[courierObjID] = `<< /Type /Font /Subtype /Type1 /BaseFont /Courier /Encoding /WinAnsiEncoding >>`

	// Per-page objects + content streams.
	mediaBox := fmt.Sprintf("[0 0 %.2f %.2f]", d.opts.Page.Width, d.opts.Page.Height)
	for i, p := range d.pages {
		// Append the footer if configured.
		body := p.content.String()
		if d.footer != "" {
			footer := fmt.Sprintf("BT /F1 9 Tf %.2f %.2f Td (%s) Tj ET\n",
				d.opts.MarginLeft, d.opts.MarginBottom*0.5,
				escape(d.encode(d.footer)))
			body = body + footer
		}
		pageObjID := firstPageObjID + i
		contentObjID := contentBaseID + i
		objects[pageObjID] = fmt.Sprintf(
			"<< /Type /Page /Parent %d 0 R /MediaBox %s /Resources << /Font << /F1 %d 0 R /F2 %d 0 R >> >> /Contents %d 0 R >>",
			pagesObjID, mediaBox, helveticaObjID, courierObjID, contentObjID)

		streamBody := body
		streamHeader := ""
		if d.opts.Compress {
			compressed, err := zlibCompress([]byte(streamBody))
			if err == nil {
				streamBody = string(compressed)
				streamHeader = " /Filter /FlateDecode"
			}
		}
		objects[contentObjID] = fmt.Sprintf("<< /Length %d%s >>\nstream\n%sendstream",
			len(streamBody), streamHeader, streamBody)
	}

	// Header.
	if _, err := io.WriteString(cw, "%PDF-1.4\n%\xff\xff\xff\xff\n"); err != nil {
		return cw.n, err
	}
	// Emit objects in numeric order; record their starting offsets.
	maxID := contentBaseID + N - 1
	offsets := make(map[int]int64, maxID+1)
	for id := 1; id <= maxID; id++ {
		body, ok := objects[id]
		if !ok {
			continue
		}
		offsets[id] = cw.n
		if _, err := fmt.Fprintf(cw, "%d 0 obj\n%s\nendobj\n", id, body); err != nil {
			return cw.n, err
		}
	}
	// xref table.
	xrefOffset := cw.n
	if _, err := fmt.Fprintf(cw, "xref\n0 %d\n", maxID+1); err != nil {
		return cw.n, err
	}
	if _, err := io.WriteString(cw, "0000000000 65535 f \n"); err != nil {
		return cw.n, err
	}
	for id := 1; id <= maxID; id++ {
		if _, err := fmt.Fprintf(cw, "%010d 00000 n \n", offsets[id]); err != nil {
			return cw.n, err
		}
	}
	// trailer.
	if _, err := fmt.Fprintf(cw,
		"trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		maxID+1, catalogObjID, xrefOffset); err != nil {
		return cw.n, err
	}
	return cw.n, nil
}

// zlibCompress runs s through zlib (deflate with zlib header) so
// the content stream qualifies for the standard /FlateDecode filter.
func zlibCompress(in []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(in); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// countingWriter tracks bytes written so we can record xref offsets
// without re-buffering the whole output.
type countingWriter struct {
	w io.Writer
	n int64
}

// Write implements io.Writer; the byte count is added to the running
// total used for xref offset bookkeeping.
func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
