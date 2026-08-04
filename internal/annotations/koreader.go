package annotations

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/source"
)

// koreaderExport is KOReader's own "Export highlights" JSON.
//
// The field names are the trap in this format: `text` is the highlighted
// passage and `note` is the reader's comment on it, which is exactly the
// reverse of the extractor envelope in envelope.go. Anything that reads one
// as though it were the other imports every comment as a passage and throws
// away every passage, and does so silently.
type koreaderExport struct {
	Title     string `json:"title"`
	Author    string `json:"author"`
	CreatedOn string `json:"created_on"`

	Entries []koreaderEntry `json:"entries"`
}

type koreaderEntry struct {
	// Page is a number for a PDF and an xpointer string for an epub, so it
	// is decoded loosely rather than as an int.
	Page     json.RawMessage `json:"page"`
	Chapter  string          `json:"chapter"`
	Text     string          `json:"text"`
	Note     string          `json:"note"`
	Datetime string          `json:"datetime"`
	Drawer   string          `json:"drawer"`
}

func parseKOReader(data []byte, now time.Time) (Parsed, error) {
	var export koreaderExport
	if err := json.Unmarshal(data, &export); err != nil {
		return Parsed{}, fmt.Errorf("annotations: parse KOReader export: %w", err)
	}

	title := collapseSpace(export.Title)
	author := collapseSpace(export.Author)
	if title == "" {
		return Parsed{}, fmt.Errorf("annotations: KOReader export has no title")
	}

	updatedAt := parseTimestamp(export.CreatedOn)
	if updatedAt.IsZero() {
		updatedAt = now
	}

	result := Parsed{Format: FormatKOReader}
	identity := documentID(title, author)
	document := source.Document{
		ExternalID: identity,
		Title:      title,
		Author:     author,
		UpdatedAt:  updatedAt,
	}

	for index, entry := range export.Entries {
		quote := cleanPassage(entry.Text)
		note := cleanPassage(entry.Note)
		if quote == "" && note == "" {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("entry %d has neither a passage nor a note; skipped", index+1))
			continue
		}

		page := scalarString(entry.Page)
		created := parseTimestamp(entry.Datetime)

		document.Highlights = append(document.Highlights, source.Highlight{
			// Two things about this ref.
			//
			// The note is part of it because KOReader leaves datetime alone
			// when a note is edited. Without it, editing a note in KOReader
			// and re-exporting produces a file the import cannot tell from
			// the one before it, and the edit never lands.
			//
			// It is scoped by the document's own derived identity rather
			// than by the title as spelled in this file. The two are the
			// same thing, except that identity has been normalised — so
			// re-exporting a book whose metadata now capitalises its title
			// differently updates the annotations already stored instead of
			// importing every one of them again.
			ExternalID: annotationID("koreader", identity, page, entry.Datetime, entry.Text, entry.Note),
			Quote:      quote,
			Note:       note,
			Chapter:    collapseSpace(entry.Chapter),
			Page:       page,
			Ordinal:    index + 1,
			CreatedAt:  created,
			UpdatedAt:  created,
		})
	}

	result.Document = document
	return result, nil
}

// scalarString renders a JSON value that may be a number or a string.
//
// KOReader's `page` is an integer for a paginated document and an xpointer
// string for a reflowable one, and both spellings appear in exports from the
// same installation depending on what was being read.
func scalarString(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return collapseSpace(text)
	}

	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return collapseSpace(trimmed)
}
