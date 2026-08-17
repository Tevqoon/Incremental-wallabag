package annotations

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// pdftextTimeout bounds one pdftotext invocation. Generous for a large
// scanned book — a few hundred pages of already-embedded OCR text, not a
// fresh OCR pass — while still failing an upload that has genuinely hung
// rather than blocking the request forever.
const pdftextTimeout = 2 * time.Minute

// pdfWord is one word poppler's own layout analysis found on a page, in PDF
// page-coordinate space — bottom-left origin, y increasing upward, the same
// space /QuadPoints and /Rect already use. pdftotext's own -bbox output is
// top-left origin; parseBBox flips it at parse time so nothing downstream
// has to think about the difference.
type pdfWord struct {
	box  rect
	text string
}

// pageWords extracts every page's words with position, by shelling out to
// poppler's pdftotext -bbox rather than reading font glyphs directly out of
// the PDF's own content streams the way the rest of this package's reads do.
//
// This exists because increader's own in-process PDF reader (rsc.io/pdf)
// cannot recover text a scanned book's OCR pass adds. Confirmed directly,
// not assumed: ocrmypdf (and, from what its own generated object names
// look like, most other OCR tools) writes the recognised text as its own
// Form XObject, invoked from the page's content stream via a Do operator —
// and rsc.io/pdf's content-stream interpreter, like every other small
// pure-Go PDF library checked, never follows a Do into the XObject it
// names. The text-showing operators inside are consequently never reached
// at all: a real scanned page that pdftotext recovers thousands of
// characters from comes back with zero glyphs from rsc.io/pdf, no error,
// nothing a recover() could have caught. poppler is a full, mature
// rendering engine that resolves Form XObjects as a matter of course — the
// same reason the Emacs/PyMuPDF extractor increader's own PDF route was
// ported from already handled this correctly.
//
// A process dependency, not a Go one, and a deliberate trade rather than an
// accident: see the Dockerfile's own comment on the base image this
// requires poppler-utils to be installed into.
func pageWords(data []byte) (map[int][]pdfWord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pdftextTimeout)
	defer cancel()

	// "-" twice: read the PDF from stdin, write the bbox XML to stdout.
	// Neither direction touches disk, which matters here specifically — an
	// uploaded file is untrusted, and a temp file is one more thing to name
	// safely and guarantee gets cleaned up on every exit path.
	cmd := exec.CommandContext(ctx, "pdftotext", "-bbox", "-", "-")
	cmd.Stdin = bytes.NewReader(data)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("annotations: pdftotext: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return parseBBox(stdout.Bytes())
}

// bboxDocument, bboxPage and bboxWord decode pdftotext -bbox's own output
// shape: an HTML document with one <page> per PDF page, in page order, each
// holding the words poppler's layout analysis found on it, in reading
// order.
type bboxDocument struct {
	Pages []bboxPage `xml:"body>doc>page"`
}

type bboxPage struct {
	Width  float64    `xml:"width,attr"`
	Height float64    `xml:"height,attr"`
	Words  []bboxWord `xml:"word"`
}

type bboxWord struct {
	XMin float64 `xml:"xMin,attr"`
	YMin float64 `xml:"yMin,attr"`
	XMax float64 `xml:"xMax,attr"`
	YMax float64 `xml:"yMax,attr"`
	Text string  `xml:",chardata"`
}

// parseBBox decodes pdftotext -bbox's output into per-page word lists,
// keyed by page number starting at 1 — the same numbering parsePDF's own
// page loop uses, since <page> elements appear in document order with no
// number of their own to read back out.
//
// The y-flip on every word (page.Height - value) is the one piece of real
// work here: poppler reports -bbox coordinates top-left origin, y growing
// downward, the ordinary convention for a bounding box on a page image —
// but a PDF's own /QuadPoints and /Rect are bottom-left origin, y growing
// upward, and textUnderMarkup has to compare a word's box against those
// directly. Getting this backwards would not error; it would silently
// match every highlight against words on the mirror-opposite part of the
// page, which is exactly the kind of wrong that looks like "no highlights
// worked" without ever crashing.
func parseBBox(data []byte) (map[int][]pdfWord, error) {
	var doc bboxDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("annotations: parse pdftotext output: %w", err)
	}

	pages := make(map[int][]pdfWord, len(doc.Pages))
	for i, page := range doc.Pages {
		words := make([]pdfWord, 0, len(page.Words))
		for _, w := range page.Words {
			text := strings.TrimSpace(w.Text)
			if text == "" {
				continue
			}
			words = append(words, pdfWord{
				box: rect{
					x0: w.XMin,
					y0: page.Height - w.YMax,
					x1: w.XMax,
					y1: page.Height - w.YMin,
				},
				text: text,
			})
		}
		pages[i+1] = words
	}
	return pages, nil
}
