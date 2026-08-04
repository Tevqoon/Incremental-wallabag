package annotations

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/source"
)

// extractorEnvelope is the JSON that org-roam-annotation-import's PyMuPDF
// extractor writes — scripts/pdf-annotation-extractor.py in that repository,
// which reads a PDF's own annotations as written by Highlights, Skim, Preview
// or Acrobat.
//
// Note the field names against koreaderExport: here `quote` is the
// highlighted passage and `text` is the reader's comment, the exact reverse
// of KOReader's. The two shapes are told apart in Parse by looking for
// `quote`, precisely because reading one as the other fails silently.
type extractorEnvelope struct {
	Title     string `json:"title"`
	Author    string `json:"author"`
	URL       string `json:"url"`
	SourceTag string `json:"source_tag"`
	UpdatedAt string `json:"updated_at"`

	Entries []extractorEntry `json:"entries"`
}

type extractorEntry struct {
	ID        string          `json:"id"`
	Quote     string          `json:"quote"`
	Text      string          `json:"text"`
	Page      json.RawMessage `json:"page"`
	Chapter   string          `json:"chapter"`
	Color     string          `json:"color"`
	UpdatedAt string          `json:"updated_at"`

	// Image is a path on the machine that ran the extractor, written for a
	// rectangle annotation snapshotting a figure. It is recorded in the note
	// rather than followed: the file is not part of the upload and increader
	// has no access to the filesystem it names.
	Image string `json:"image"`
}

func parseEnvelope(data []byte, now time.Time) (Parsed, error) {
	var envelope extractorEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Parsed{}, fmt.Errorf("annotations: parse annotation envelope: %w", err)
	}

	title := collapseSpace(envelope.Title)
	author := collapseSpace(envelope.Author)
	if title == "" {
		return Parsed{}, fmt.Errorf("annotations: annotation envelope has no title")
	}

	updatedAt := parseTimestamp(envelope.UpdatedAt)
	if updatedAt.IsZero() {
		updatedAt = now
	}

	result := Parsed{Format: FormatEnvelope}
	identity := documentID(title, author)
	document := source.Document{
		ExternalID: identity,
		Title:      title,
		Author:     author,
		URL:        strings.TrimSpace(envelope.URL),
		UpdatedAt:  updatedAt,
	}

	for index, entry := range envelope.Entries {
		quote := cleanPassage(entry.Quote)
		note := cleanPassage(entry.Text)
		page := scalarString(entry.Page)

		if entry.Image != "" {
			// The snapshot itself stayed on the machine that ran the
			// extractor, so all that can honestly be carried across is that
			// there was one and what it was called.
			marker := "[figure: " + filepath.Base(entry.Image) + "]"
			if note == "" {
				note = marker
			} else {
				note += " " + marker
			}
		}

		if quote == "" && note == "" {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("entry %d has neither a passage nor a note; skipped", index+1))
			continue
		}

		// The extractor already produces a stable id — the PDF's own /NM name
		// when the writing tool set one, a content hash when it did not — so
		// it is used as-is, and re-importing the same PDF through either the
		// extractor or increader's own reader lands on the same annotation.
		ref := strings.TrimSpace(entry.ID)
		if ref == "" {
			// Scoped by the derived identity, not the file's spelling of
			// the title — see the same choice in parseKOReader.
			ref = annotationID("extract", identity, page, entry.Quote, entry.Text)
		}

		created := parseTimestamp(entry.UpdatedAt)

		document.Highlights = append(document.Highlights, source.Highlight{
			ExternalID: ref,
			Quote:      quote,
			Note:       note,
			Chapter:    collapseSpace(entry.Chapter),
			Page:       page,
			Color:      normaliseColor(entry.Color),
			Ordinal:    index + 1,
			CreatedAt:  created,
			UpdatedAt:  created,
		})
	}

	result.Document = document
	return result, nil
}

// normaliseColor reduces a colour to "#rrggbb" or nothing.
//
// The extractor's own comments record having emitted a malformed seven-digit
// value from an out-of-range channel, so anything that is not exactly six hex
// digits after the hash is dropped rather than stored to be puzzled over
// later.
func normaliseColor(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 7 || value[0] != '#' {
		return ""
	}
	for _, r := range value[1:] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return ""
		}
	}
	return value
}
