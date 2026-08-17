package annotations

import (
	"bytes"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rsc.io/pdf"

	"github.com/Tevqoon/increader/internal/source"
)

// Annotation subtypes worth importing. Ink, line, polygon, link and widget
// annotations carry nothing a reader wrote in words, so they are skipped
// rather than imported as empty passages.
var (
	markupSubtypes = map[string]bool{
		"Highlight": true, "Underline": true, "StrikeOut": true, "Squiggly": true,
	}
	noteSubtypes = map[string]bool{
		"Text": true, "FreeText": true,
	}
	// A Square is a rectangle drawn around a figure. There is no text under
	// it to recover, but the reader put it there on purpose and usually
	// typed something into it, so it is imported for its note alone.
	regionSubtypes = map[string]bool{
		"Square": true, "Circle": true,
	}
)

// quadPad widens a quadrilateral before glyphs are tested against it.
//
// Writers routinely emit markup geometry a shade tighter than the glyphs it
// covers — the first and last characters of a highlight are the ones that go
// missing — and a point or two of slack costs nothing, since the gap to the
// neighbouring line is far larger than that.
const quadPad = 2.0

// parsePDF reads a PDF's own annotations.
//
// Text under a highlight is not stored in the annotation: the PDF format
// records only where the highlight is, and the passage has to be recovered by
// working out which words sit underneath it. That recovery is imperfect — a
// scanned page with genuinely no text layer at all yields nothing — so a
// highlight whose text cannot be recovered is still imported, carrying its
// page, colour and note, and warned about. An annotation you can see and fix
// is better than one silently dropped.
//
// The word recovery itself shells out to poppler's pdftotext rather than
// reading glyphs out of the PDF's own content streams the way this package
// once did — see pageWords in pdftext.go for why: a scanned book's OCR text
// routinely lives in a Form XObject rsc.io/pdf's own interpreter cannot
// follow, and no small pure-Go PDF library checked can either.
func parsePDF(filename string, data []byte, now time.Time) (result Parsed, err error) {
	// rsc.io/pdf reports every structural problem by panicking: there are
	// three dozen panic sites in it and not one recover. A malformed upload
	// would take the whole server down, so every call into it happens under
	// this.
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("annotations: cannot read PDF: %v", recovered)
		}
	}()

	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Parsed{}, fmt.Errorf("annotations: cannot read PDF: %w", err)
	}

	title, author, subtitle := pdfMetadata(reader)
	if title == "" {
		// A PDF with no title of its own is the common case, not the odd
		// one. The filename is what the reader themselves called it, which
		// is a better answer than refusing the upload.
		title = collapseSpace(strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename)))
	}
	if title == "" {
		return Parsed{}, fmt.Errorf("annotations: PDF has neither a title nor a usable filename")
	}

	result = Parsed{Format: FormatPDF}
	document := source.Document{
		ExternalID: documentID(title, author),
		Title:      title,
		Author:     author,
		Subtitle:   subtitle,
		UpdatedAt:  now,
	}

	outline := readOutline(reader)
	ordinal := 0

	// Read once for the whole document, up front, rather than lazily per
	// page the way this used to read glyphs: pdftotext has no cheaper mode
	// that reads only the pages asked for while still resolving every Form
	// XObject a page's OCR text might live in, so there is nothing to gain
	// by deferring it, and doing it once here means every page below just
	// looks its own words up in a map instead of managing when to fetch
	// them. A failure is not fatal to the whole import — every markup
	// annotation still gets imported for its note alone, exactly as if its
	// own individual text recovery had failed — but it is worth exactly one
	// warning up top rather than one per annotation, since the page-by-page
	// message ("the page may have no text layer") would otherwise repeat
	// once per highlight in the file and say nothing more useful the second
	// time.
	words, wordsErr := pageWords(data)
	if wordsErr != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"could not recover any passage text (%v); every highlight below was kept for its note only, if it has one", wordsErr))
	}

	for number := 1; number <= reader.NumPage(); number++ {
		page := reader.Page(number)
		if page.V.IsNull() {
			continue
		}

		annots := page.V.Key("Annots")
		if annots.Kind() != pdf.Array || annots.Len() == 0 {
			continue
		}

		for i := 0; i < annots.Len(); i++ {
			annot := annots.Index(i)
			subtype := annot.Key("Subtype").Name()
			if !markupSubtypes[subtype] && !noteSubtypes[subtype] && !regionSubtypes[subtype] {
				continue
			}

			note := cleanPassage(annot.Key("Contents").Text())

			var quote string
			if markupSubtypes[subtype] {
				quote = textUnderMarkup(words[number], annot)
				if quote == "" && note == "" && wordsErr == nil {
					result.Warnings = append(result.Warnings, fmt.Sprintf(
						"page %d: could not recover the text under a %s and it has no note; "+
							"the page may have no text layer", number, strings.ToLower(subtype)))
				} else if quote == "" && wordsErr == nil {
					result.Warnings = append(result.Warnings, fmt.Sprintf(
						"page %d: kept a %s for its note only — its text could not be recovered",
						number, strings.ToLower(subtype)))
				}
			}

			if quote == "" && note == "" {
				continue
			}

			created := parseTimestamp(annot.Key("CreationDate").Text())
			if created.IsZero() {
				created = parseTimestamp(annot.Key("M").Text())
			}

			ordinal++
			document.Highlights = append(document.Highlights, source.Highlight{
				ExternalID: pdfAnnotationID(annot, number, quote, note),
				Quote:      quote,
				Note:       note,
				Chapter:    outline.chapterFor(number),
				Page:       strconv.Itoa(number),
				Color:      annotationColor(annot),
				Ordinal:    ordinal,
				CreatedAt:  created,
				UpdatedAt:  created,
			})
		}
	}

	result.Document = document
	return result, nil
}

// pdfMetadata reads the document information dictionary.
//
// /Subject is taken as the subtitle: it is where a typesetting workflow puts
// the thing that is not quite the title, and it is empty far more often than
// it is wrong.
func pdfMetadata(reader *pdf.Reader) (title, author, subtitle string) {
	info := reader.Trailer().Key("Info")
	return collapseSpace(info.Key("Title").Text()),
		collapseSpace(info.Key("Author").Text()),
		collapseSpace(info.Key("Subject").Text())
}

// rect is an axis-aligned box in PDF user space, y increasing upwards.
type rect struct{ x0, y0, x1, y1 float64 }

func (r rect) contains(x, y float64) bool {
	return x >= r.x0 && x <= r.x1 && y >= r.y0 && y <= r.y1
}

// markupRects returns the boxes an annotation covers.
//
// Text markup geometry lives in /QuadPoints as a flat run of eight numbers
// per quadrilateral, one quadrilateral per line of text covered. A multi-line
// highlight must be read line by line: taking the bounding box of the whole
// thing instead would sweep up every word on the intermediate lines,
// including the ones to the left of where the highlight starts and to the
// right of where it ends.
//
// The order of the four corners within a quadrilateral is famously
// inconsistent between producers, so this takes the extremes of all four
// points rather than trusting any particular corner to be a particular one.
func markupRects(annot pdf.Value) []rect {
	quads := annot.Key("QuadPoints")
	var rects []rect

	if quads.Kind() == pdf.Array && quads.Len() >= 8 {
		for base := 0; base+7 < quads.Len(); base += 8 {
			minX, minY := math.Inf(1), math.Inf(1)
			maxX, maxY := math.Inf(-1), math.Inf(-1)
			for offset := 0; offset < 8; offset += 2 {
				x := quads.Index(base + offset).Float64()
				y := quads.Index(base + offset + 1).Float64()
				minX, maxX = math.Min(minX, x), math.Max(maxX, x)
				minY, maxY = math.Min(minY, y), math.Max(maxY, y)
			}
			rects = append(rects, rect{minX - quadPad, minY - quadPad, maxX + quadPad, maxY + quadPad})
		}
		return rects
	}

	// No quadrilaterals: fall back to the annotation's own bounding box,
	// which is all a badly written markup annotation leaves behind.
	box := annot.Key("Rect")
	if box.Kind() == pdf.Array && box.Len() == 4 {
		x0, y0 := box.Index(0).Float64(), box.Index(1).Float64()
		x1, y1 := box.Index(2).Float64(), box.Index(3).Float64()
		rects = append(rects, rect{
			math.Min(x0, x1) - quadPad, math.Min(y0, y1) - quadPad,
			math.Max(x0, x1) + quadPad, math.Max(y0, y1) + quadPad,
		})
	}
	return rects
}

// textUnderMarkup recovers the passage a markup annotation covers, from the
// page's own word list — see pageWords in pdftext.go for where that comes
// from and why.
//
// A word belongs to the annotation when its bbox centre falls inside one of
// the covered boxes. Centre rather than any corner or the whole box, for the
// same reason pdf-annotation-extractor.py's own word-selection settled on
// it (the Python/PyMuPDF extractor this route was ported from — see that
// script's _text_under_markup): it is what keeps a word straddling the edge
// of a highlight from being counted twice when two highlights abut, and
// what avoids the doubled-up runs an OCR text layer's overlapping
// marked-content groups can otherwise produce.
func textUnderMarkup(words []pdfWord, annot pdf.Value) string {
	boxes := markupRects(annot)
	if len(boxes) == 0 || len(words) == 0 {
		return ""
	}

	var selected []pdfWord
	for _, word := range words {
		cx := (word.box.x0 + word.box.x1) / 2
		cy := (word.box.y0 + word.box.y1) / 2
		for _, box := range boxes {
			if box.contains(cx, cy) {
				selected = append(selected, word)
				break
			}
		}
	}
	return assembleWords(selected)
}

// assembleWords turns selected words back into a readable passage.
//
// Unlike the glyph-level assembly this replaced, there is no sorting to do
// first: pageWords already returns each page's words in poppler's own
// reading order — real layout analysis, not a guess reconstructed from raw
// coordinates — and filtering down to the ones a highlight covers preserves
// that relative order. Trusting it also does the right thing in a layout
// glyph-sorting could not: a multi-column page's reading order is "down
// this column, then down the next", not "left to right across the whole
// page", and poppler already knows the difference.
func assembleWords(words []pdfWord) string {
	if len(words) == 0 {
		return ""
	}

	var out strings.Builder
	for index, word := range words {
		if index > 0 {
			previous := words[index-1]
			height := math.Max(word.box.y1-word.box.y0, 1)
			cy := (word.box.y0 + word.box.y1) / 2
			previousCY := (previous.box.y0 + previous.box.y1) / 2
			if math.Abs(cy-previousCY) > 0.5*height {
				// A new line. Written as a real newline so that
				// joinHyphenation downstream can see the break and repair a
				// word split across it.
				out.WriteString("\n")
			} else {
				out.WriteString(" ")
			}
		}
		out.WriteString(word.text)
	}
	return cleanPassage(out.String())
}

// annotationColor reads /C as "#rrggbb".
//
// /C is a colour in whatever space its length implies: three components is
// RGB, one is grey, four is CMYK. Anything else is left alone rather than
// guessed at.
func annotationColor(annot pdf.Value) string {
	components := annot.Key("C")
	if components.Kind() != pdf.Array {
		return ""
	}

	channel := func(v float64) int {
		return int(math.Round(math.Max(0, math.Min(1, v)) * 255))
	}

	switch components.Len() {
	case 1:
		grey := channel(components.Index(0).Float64())
		return fmt.Sprintf("#%02x%02x%02x", grey, grey, grey)
	case 3:
		return fmt.Sprintf("#%02x%02x%02x",
			channel(components.Index(0).Float64()),
			channel(components.Index(1).Float64()),
			channel(components.Index(2).Float64()))
	case 4:
		c, m, y := components.Index(0).Float64(), components.Index(1).Float64(), components.Index(2).Float64()
		k := components.Index(3).Float64()
		return fmt.Sprintf("#%02x%02x%02x",
			channel((1-c)*(1-k)), channel((1-m)*(1-k)), channel((1-y)*(1-k)))
	}
	return ""
}

// pdfAnnotationID gives an annotation a ref that survives re-importing.
//
// /NM is the PDF's own annotation name and is what a well-behaved writer
// leaves behind, so it is preferred. Many readers omit it, and the xref
// number is no substitute — it is not stable across a save that rewrites or
// optimises the file — so the fallback is a hash of what the annotation
// actually is. This matches what the PyMuPDF extractor does, deliberately:
// the same annotation imported through either route should be the same
// annotation.
func pdfAnnotationID(annot pdf.Value, page int, quote, note string) string {
	if name := strings.TrimSpace(annot.Key("NM").Text()); name != "" {
		return "pdf-" + name
	}
	return annotationID("pdf", strconv.Itoa(page), markupGeometryKey(annot), quote, note)
}

// markupGeometryKey renders an annotation's geometry for hashing.
//
// Included in the identity so that two identical short highlights on the same
// page — "the state", say, marked twice — remain two annotations rather than
// collapsing into one.
func markupGeometryKey(annot pdf.Value) string {
	for _, key := range []string{"QuadPoints", "Rect"} {
		value := annot.Key(key)
		if value.Kind() == pdf.Array {
			return value.String()
		}
	}
	return ""
}
