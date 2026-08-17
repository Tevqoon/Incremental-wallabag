package annotations

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requirePdftotext skips a test rather than failing it when poppler is not
// on this machine's PATH. Every test that goes through Parse/parsePDF now
// shells out to pdftotext for word recovery (see pageWords in pdftext.go),
// which the increader production image installs deliberately but a plain
// `go test` environment has no guarantee of — see the Dockerfile's own
// comment on why that dependency exists at all. Skipping rather than
// failing is what keeps a checkout without poppler installed able to run
// the rest of this package's tests instead of reporting the whole suite
// red for a missing binary that has nothing to do with what most of them
// check.
func requirePdftotext(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext (poppler-utils) not found on PATH; skipping")
	}
}

// buildPDF assembles a minimal but genuinely valid PDF from object bodies.
//
// Written by hand rather than fetched or checked in as a fixture: the whole
// point of these tests is what happens between a highlight's geometry and the
// glyphs underneath it, and that needs the geometry to be something the test
// itself chose. A binary fixture would hide exactly the numbers under test.
//
// bodies[0] is object 1, and so on. The xref offsets are computed as the file
// is written, because rsc.io/pdf reads the trailer through startxref and will
// not accept a table that lies.
func buildPDF(bodies []string) []byte {
	var out strings.Builder
	out.WriteString("%PDF-1.4\n")

	offsets := make([]int, len(bodies))
	for i, body := range bodies {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(bodies)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(bodies)+1, len(bodies), xref)

	return []byte(out.String())
}

// stream wraps content in a stream object with a correct /Length.
func stream(content string) string {
	return "<< /Length " + strconv.Itoa(len(content)) + " >>\nstream\n" + content + "\nendstream"
}

// uniformWidths is a /Widths array giving every printable ASCII character
// 500/1000 of the type size.
//
// Real, because rsc.io/pdf takes glyph advances from /Widths and nowhere
// else: a base-14 font with the array omitted reports every glyph as zero
// wide, which stacks the whole line at one x coordinate and would make this
// test pass for the wrong reason.
func uniformWidths() string {
	widths := make([]string, 0, 95)
	for i := 32; i <= 126; i++ {
		widths = append(widths, "500")
	}
	return "[" + strings.Join(widths, " ") + "]"
}

// annotatedPDF is one page reading "Hello marked world at last" with a
// highlight over "marked world", a sticky note, and an outline entry.
//
// At 12pt with 500-width glyphs each character advances 6 points from x=72,
// so "marked world" runs from x=108 to x=180 on the baseline at y=700. The
// quadrilateral below covers exactly that and nothing either side of it.
func annotatedPDF(t *testing.T) []byte {
	t.Helper()
	return buildPDF([]string{
		// 1 catalog
		"<< /Type /Catalog /Pages 2 0 R /Outlines 6 0 R >>",
		// 2 page tree
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		// 3 page
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R " +
			"/Annots [7 0 R 9 0 R] >>",
		// 4 content
		stream("BT /F1 12 Tf 72 700 Td (Hello marked world at last) Tj ET"),
		// 5 font
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica " +
			"/FirstChar 32 /LastChar 126 /Widths " + uniformWidths() + " >>",
		// 6 outline root
		"<< /Type /Outlines /First 8 0 R /Last 8 0 R /Count 1 >>",
		// 7 the highlight, covering "marked world"
		"<< /Type /Annot /Subtype /Highlight /Rect [106 694 182 714] " +
			"/QuadPoints [106 714 182 714 106 694 182 694] " +
			"/Contents (why this matters) /C [1 0.8 0] /NM (annot-1) " +
			"/CreationDate (D:20260701090000+02'00') >>",
		// 8 outline entry
		"<< /Title (Chapter One) /Parent 6 0 R /Dest [3 0 R /XYZ 0 792 0] >>",
		// 9 a sticky note, which has no text under it at all
		"<< /Type /Annot /Subtype /Text /Rect [500 700 512 712] " +
			"/Contents (a thought with no passage) /NM (annot-2) >>",
		// 10 document info
		"<< /Title (A Handmade Book) /Author (Nobody) /Subject (a test) >>",
	})
}

// TestParsePDFRecoversMarkedText is the central claim of the PDF route: a
// highlight stores where it is, not what it covers, so the passage has to be
// reconstructed from which glyphs sit underneath it.
func TestParsePDFRecoversMarkedText(t *testing.T) {
	requirePdftotext(t)
	parsed, err := Parse("book.pdf", annotatedPDF(t), now)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Format != FormatPDF {
		t.Fatalf("format = %q, want %q", parsed.Format, FormatPDF)
	}

	if parsed.Document.Title != "A Handmade Book" {
		t.Errorf("title = %q, want the document information dictionary's", parsed.Document.Title)
	}
	if parsed.Document.Subtitle != "a test" {
		t.Errorf("subtitle = %q, want /Subject", parsed.Document.Subtitle)
	}

	if got := len(parsed.Document.Highlights); got != 2 {
		t.Fatalf("imported %d annotations, want 2; got %+v", got, parsed.Document.Highlights)
	}

	highlight := parsed.Document.Highlights[0]
	if highlight.Quote != "marked world" {
		t.Errorf("quote = %q, want exactly the glyphs under the quadrilateral", highlight.Quote)
	}
	if highlight.Note != "why this matters" {
		t.Errorf("note = %q, want /Contents", highlight.Note)
	}
	if highlight.Page != "1" {
		t.Errorf("page = %q", highlight.Page)
	}
	if highlight.Chapter != "Chapter One" {
		t.Errorf("chapter = %q, want the outline entry pointing at this page", highlight.Chapter)
	}
	if highlight.Color != "#ffcc00" {
		t.Errorf("color = %q, want /C converted from its 0..1 components", highlight.Color)
	}
	if highlight.ExternalID != "pdf-annot-1" {
		t.Errorf("ref = %q, want the PDF's own /NM name preferred", highlight.ExternalID)
	}
	if highlight.CreatedAt.IsZero() {
		t.Error("CreationDate was not read")
	}

	// A sticky note has no glyphs under it by definition, and is kept for
	// what the reader typed into it.
	note := parsed.Document.Highlights[1]
	if note.Quote != "" || note.Note != "a thought with no passage" {
		t.Errorf("sticky note = %+v", note)
	}
	if note.Ordinal != 2 {
		t.Errorf("ordinal = %d, want reading order preserved", note.Ordinal)
	}
}

// TestParsePDFIsIdempotent guards the property that makes re-uploading a book
// safe: nothing about a second read of the same file may differ.
func TestParsePDFIsIdempotent(t *testing.T) {
	requirePdftotext(t)
	data := annotatedPDF(t)

	first, err := Parse("book.pdf", data, now)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	second, err := Parse("book-renamed.pdf", data, now.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("Parse again: %v", err)
	}

	if first.Document.ExternalID != second.Document.ExternalID {
		t.Error("the same PDF derived two document identities")
	}
	for i := range first.Document.Highlights {
		if first.Document.Highlights[i].ExternalID != second.Document.Highlights[i].ExternalID {
			t.Errorf("annotation %d derived two refs from the same file", i)
		}
	}
}

// TestParsePDFSurvivesRubbish is the reason every call into rsc.io/pdf sits
// behind a recover: it reports structural problems by panicking, in three
// dozen places, and never recovers. An uploaded file is untrusted input, so a
// malformed one has to come back as an error rather than as a dead server.
func TestParsePDFSurvivesRubbish(t *testing.T) {
	valid := annotatedPDF(t)

	cases := map[string][]byte{
		"header only": []byte("%PDF-1.4\n"),
		"truncated":   valid[:len(valid)/2],
		"broken startxref": []byte(strings.Replace(string(valid),
			"startxref", "startxrefX", 1)),
		"garbage after header": append([]byte("%PDF-1.4\n"), []byte(strings.Repeat("\x00\xff", 400))...),
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			// The requirement is only that it returns rather than panicking.
			// Some of these are recoverable enough that a reader can still
			// find the trailer, and reading what is there is a fine outcome.
			if _, err := Parse("broken.pdf", data, now); err != nil {
				t.Logf("returned an error, as expected: %v", err)
			}
		})
	}
}

// TestPDFFallsBackToFilename covers the common case rather than the tidy one:
// most PDFs carry no title of their own, and the name the reader gave the
// file is a better answer than refusing the upload.
func TestPDFFallsBackToFilename(t *testing.T) {
	data := buildPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		"<< >>",
	})

	parsed, err := Parse("Some Paper (2024).pdf", data, now)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Document.Title != "Some Paper (2024)" {
		t.Errorf("title = %q, want the filename without its extension", parsed.Document.Title)
	}
	if len(parsed.Document.Highlights) != 0 {
		t.Errorf("a PDF with no annotations produced %d", len(parsed.Document.Highlights))
	}
}

// TestAssembleWordsJoinsSameLine checks the ordinary case: poppler's own
// word list already has real spaces between words on the wire (unlike
// rsc.io/pdf's glyph stream, which had none), so the only thing left to
// reconstruct from geometry is whether consecutive selected words are on
// the same line at all.
func TestAssembleWordsJoinsSameLine(t *testing.T) {
	words := []pdfWord{
		wordAt(10, 90, 30, 100, "Hello"),
		wordAt(34, 90, 60, 100, "marked"),
		wordAt(64, 90, 80, 100, "world"),
	}
	if got := assembleWords(words); got != "Hello marked world" {
		t.Errorf("assembleWords = %q, want %q", got, "Hello marked world")
	}
}

// TestAssembleWordsRepairsHyphenationAcrossLines is the reason the line
// break has to survive as a real newline rather than collapsing straight to
// a space the way collapseSpace would treat it otherwise: cleanPassage's
// own joinHyphenation pass only fires on a hyphen immediately before a
// newline, so this is what actually proves the newline insertion is load-
// bearing and not merely cosmetic — the same output would come out of a
// same-line join too, if the words themselves did not need repairing.
func TestAssembleWordsRepairsHyphenationAcrossLines(t *testing.T) {
	words := []pdfWord{
		wordAt(10, 90, 60, 100, "anno-"),
		wordAt(10, 70, 60, 80, "tation"),
	}
	if got := assembleWords(words); got != "annotation" {
		t.Errorf("assembleWords = %q, want %q (the hyphen repaired across the line break)", got, "annotation")
	}
}

// wordAt builds a pdfWord at the given box, in PDF page-coordinate space —
// bottom-left origin, y increasing upward, the space assembleWords and
// textUnderMarkup both work in (see pdfWord's own doc comment on why that
// is not the same space pdftotext -bbox reports coordinates in on the
// wire).
func wordAt(x0, y0, x1, y1 float64, text string) pdfWord {
	return pdfWord{box: rect{x0: x0, y0: y0, x1: x1, y1: y1}, text: text}
}
