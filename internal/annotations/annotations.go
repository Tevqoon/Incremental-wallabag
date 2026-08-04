// Package annotations turns an uploaded annotation file into a document.
//
// Three formats arrive by this route, and they are all the same thing seen
// from different tools: KOReader's own JSON export, the JSON envelope that
// org-roam-annotation-import's PyMuPDF extractor writes, and a PDF still
// carrying its own annotations. Each becomes a source.Document with its
// Highlights filled in, so the store's existing import path handles it
// without knowing which tool produced it.
//
// The package is a leaf in the same sense internal/source is: it imports
// source for the types it returns and rsc.io/pdf for the one format that
// needs it, and nothing else from this application. In particular it knows
// nothing about SQLite, HTTP, or incremental reading — deciding what a
// freshly imported annotation's schedule should be is the store's business,
// not a parser's.
package annotations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Tevqoon/increader/internal/source"
)

// Format names the shape a file turned out to be.
type Format string

const (
	FormatKOReader Format = "koreader"
	FormatEnvelope Format = "extractor"
	FormatPDF      Format = "pdf"
)

// ErrUnrecognised reports that a file is neither a PDF nor either JSON shape.
var ErrUnrecognised = errors.New("annotations: unrecognised file format")

// Parsed is the result of reading one uploaded file.
type Parsed struct {
	// Document carries the metadata and every annotation found, ready to
	// hand to the store. Its ExternalID is derived from the title and
	// author, so re-uploading an updated export of the same work updates
	// what is already stored rather than making a second copy of it.
	Document source.Document

	Format Format

	// Warnings are per-annotation problems that did not stop the import:
	// a PDF highlight whose text could not be recovered, an entry with
	// neither a passage nor a note. They are shown once after an upload
	// rather than logged, because the person who just chose the file is
	// the only one who can do anything about them.
	Warnings []string
}

// Parse reads an uploaded annotation file.
//
// filename is used only to tell a PDF from JSON; the JSON shapes are then
// told apart by their own contents rather than by what they were called,
// since both formats are conventionally just "<book title>.json".
func Parse(filename string, data []byte, now time.Time) (Parsed, error) {
	if strings.EqualFold(filepath.Ext(filename), ".pdf") || hasPDFHeader(data) {
		return parsePDF(filename, data, now)
	}

	trimmed := strings.TrimLeftFunc(string(data), unicode.IsSpace)
	if !strings.HasPrefix(trimmed, "{") {
		return Parsed{}, ErrUnrecognised
	}

	// Both JSON shapes carry a top-level "entries" array of objects, and are
	// distinguished by what those objects call the highlighted passage: the
	// extractor writes "quote" and puts the reader's comment in "text",
	// KOReader puts the passage itself in "text" and the comment in "note".
	// Getting this backwards silently imports every comment as a passage and
	// discards every passage, which is why it is decided by inspection
	// rather than by the file's name or a source_tag it may not carry.
	var probe struct {
		SourceTag string `json:"source_tag"`
		Entries   []struct {
			Quote *string `json:"quote"`
			Note  *string `json:"note"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return Parsed{}, fmt.Errorf("annotations: parse JSON: %w", err)
	}

	envelope := probe.SourceTag != "" && probe.SourceTag != "koreader"
	for _, entry := range probe.Entries {
		if entry.Quote != nil {
			envelope = true
			break
		}
		if entry.Note != nil {
			envelope = false
			break
		}
	}

	if envelope {
		return parseEnvelope(data, now)
	}
	return parseKOReader(data, now)
}

// hasPDFHeader reports whether the bytes begin with a PDF signature.
//
// Checked in a prefix rather than at offset zero exactly: the specification
// wants %PDF- first, but files that have been through a mail gateway or a
// careless concatenation routinely carry junk ahead of it, and every reader
// in existence accepts them.
func hasPDFHeader(data []byte) bool {
	limit := min(len(data), 1024)
	return strings.Contains(string(data[:limit]), "%PDF-")
}

// documentID derives a stable external id for a work from its own metadata.
//
// There is no server behind these files and so no identifier to borrow. The
// title and author are what a person would use to say "this is the same
// book", so they are what identity is built from — normalised, because the
// same book exported twice by two tools disagrees about capitalisation and
// spacing far more often than it disagrees about the words.
//
// A collision means two genuinely differently-titled works hashing alike,
// which is not a thing sha256 does. Two *editions* of the same title by the
// same author do collide, deliberately: they are the same work and their
// annotations belong together.
func documentID(title, author string) string {
	sum := sha256.Sum256([]byte(normaliseKey(title) + "\x00" + normaliseKey(author)))
	return hex.EncodeToString(sum[:])[:16]
}

// annotationID derives a stable external ref for one annotation.
//
// The parts must include everything a reader can edit that is not itself the
// identity: KOReader in particular does not update an annotation's datetime
// when its note is changed, so leaving the note out would make an edited note
// invisible to a re-import. Including it means the edit arrives as a new
// annotation rather than silently not arriving, which is the better failure.
func annotationID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + "-" + hex.EncodeToString(sum[:])[:16]
}

// normaliseKey folds a title or author down to what identity should ignore.
func normaliseKey(text string) string {
	return strings.ToLower(collapseSpace(text))
}

// collapseSpace trims and collapses every run of whitespace to one space.
//
// Deliberately not shared with ir.NormalizeSpace: this package is a leaf and
// the one caller here is four lines, whereas ir's version is load-bearing for
// article offsets and must not acquire a second set of requirements.
func collapseSpace(text string) string {
	return strings.Join(strings.FieldsFunc(text, unicode.IsSpace), " ")
}

// joinHyphenation repairs words broken across a line by a PDF or an ebook.
//
// A hyphen immediately before a newline is a typesetting artefact rather than
// part of the word, and leaving it in makes the passage unsearchable and ugly
// in equal measure. A hyphen with anything else after it is a real one and is
// left alone.
func joinHyphenation(text string) string {
	var out strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '-' && i+1 < len(runes) && (runes[i+1] == '\n' || runes[i+1] == '\r') {
			// Skip the hyphen and every line break that follows it.
			for i+1 < len(runes) && (runes[i+1] == '\n' || runes[i+1] == '\r') {
				i++
			}
			continue
		}
		out.WriteRune(runes[i])
	}
	return out.String()
}

// cleanPassage is the single normalisation every format's text goes through.
func cleanPassage(text string) string {
	return collapseSpace(joinHyphenation(text))
}

// parseTimestamp reads the several spellings these files use for a time.
//
// KOReader's own "Export highlights" plugin writes "2024-03-01 09:12:41",
// but KOReader's built-in highlight exporter writes a bare Unix timestamp
// instead — the same field, two installations, two shapes. The extractor
// writes RFC3339, and a PDF writes "D:20240301091241+01'00'". None of them is
// worth failing an import over, so an unparseable value yields the zero time
// and the caller falls back to the upload's own clock.
func parseTimestamp(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}

	if strings.HasPrefix(value, "D:") {
		return parsePDFDate(value)
	}

	// A bare run of digits is never a valid match for any layout below, so
	// trying it first cannot shadow a real date — it can only catch what
	// would otherwise fall through to the zero-time case anyway.
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(seconds, 0)
	}

	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// parsePDFDate reads the PDF date syntax, D:YYYYMMDDHHmmSSOHH'mm'.
//
// Everything after the year is optional in the specification and genuinely
// omitted in the wild, so this reads as much as is there and defaults the
// rest rather than requiring the full form.
func parsePDFDate(value string) time.Time {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, strings.TrimPrefix(value, "D:"))

	// A trailing timezone offset would otherwise be read as part of the
	// timestamp; cut it off and accept local time, since these values are
	// only ever displayed, never compared across zones.
	if len(digits) > 14 {
		digits = digits[:14]
	}
	for _, layout := range []string{"20060102150405", "200601021504", "2006010215", "20060102", "200601", "2006"} {
		if len(digits) == len(layout) {
			if parsed, err := time.ParseInLocation(layout, digits, time.Local); err == nil {
				return parsed
			}
		}
	}
	return time.Time{}
}
