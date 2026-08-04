package annotations

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

const koreaderJSON = `{
  "title": "  The Order of   Things ",
  "author": "Michel Foucault",
  "created_on": "2026-07-30 08:14:02",
  "entries": [
    {"page": 42, "chapter": "Las Meninas", "datetime": "2026-07-01 09:00:00",
     "text": "the painter is standing a little back from his canvas",
     "note": "the mirror does the work"},
    {"page": "/body/DocFragment[3]/body/p[7]", "chapter": "Las Meninas",
     "datetime": "2026-07-01 09:05:00",
     "text": "an in-\nvisible relation", "note": ""},
    {"page": 91, "chapter": "The Prose of the World",
     "datetime": "2026-07-02 10:00:00", "text": "", "note": "come back to this"},
    {"page": 92, "datetime": "2026-07-02 10:01:00", "text": "", "note": ""}
  ]
}`

// TestParseKOReader covers the format's own conventions, all of which are
// traps: text is the passage and note is the comment, page is sometimes a
// number and sometimes an xpointer, and a hyphen at a line break is
// typesetting rather than spelling.
func TestParseKOReader(t *testing.T) {
	parsed, err := Parse("The Order of Things.json", []byte(koreaderJSON), now)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Format != FormatKOReader {
		t.Fatalf("format = %q, want %q", parsed.Format, FormatKOReader)
	}

	if parsed.Document.Title != "The Order of Things" {
		t.Errorf("title = %q, want the collapsed form", parsed.Document.Title)
	}
	if got := len(parsed.Document.Highlights); got != 3 {
		t.Fatalf("imported %d highlights, want 3 (the empty one is dropped)", got)
	}
	if len(parsed.Warnings) != 1 {
		t.Errorf("warnings = %v, want one for the empty entry", parsed.Warnings)
	}

	first := parsed.Document.Highlights[0]
	if !strings.HasPrefix(first.Quote, "the painter is standing") {
		t.Errorf("quote = %q; KOReader's `text` is the passage", first.Quote)
	}
	if first.Note != "the mirror does the work" {
		t.Errorf("note = %q; KOReader's `note` is the comment", first.Note)
	}
	if first.Chapter != "Las Meninas" || first.Page != "42" || first.Ordinal != 1 {
		t.Errorf("chapter/page/ordinal = %q/%q/%d", first.Chapter, first.Page, first.Ordinal)
	}

	if got := parsed.Document.Highlights[1].Quote; got != "an invisible relation" {
		t.Errorf("quote = %q, want the hyphenated line break repaired", got)
	}
	if got := parsed.Document.Highlights[1].Page; !strings.HasPrefix(got, "/body/") {
		t.Errorf("page = %q, want the xpointer preserved as a string", got)
	}

	// The note-only entry survives: an annotation with a comment and no
	// passage is still something the reader wrote.
	if third := parsed.Document.Highlights[2]; third.Quote != "" || third.Note != "come back to this" {
		t.Errorf("note-only entry = %+v", third)
	}
}

const envelopeJSON = `{
  "title": "Discipline and Punish",
  "author": "Michel Foucault",
  "url": null,
  "source_tag": "highlights",
  "updated_at": "2026-07-30T08:14:02+02:00",
  "entries": [
    {"id": "highlights-abc123", "source": "Highlights", "quote": "the soul is the prison of the body",
     "text": "against the body/mind order", "page": 30, "chapter": "Panopticism",
     "color": "#ffcc00", "updated_at": "D:20260701090000+02'00'"},
    {"id": "", "quote": "docile bodies", "text": "", "page": 135,
     "chapter": "Docile Bodies", "color": "#112767b"},
    {"id": "highlights-fig", "quote": "", "text": "the plan of the prison",
     "page": 200, "image": "/home/someone/images/dp-p200-1a2b3c4d.png"}
  ]
}`

// TestParseEnvelope covers the extractor's own shape, whose field names are
// the reverse of KOReader's — quote is the passage, text is the comment —
// and which is told apart from KOReader by inspection rather than by name.
func TestParseEnvelope(t *testing.T) {
	parsed, err := Parse("Discipline and Punish.json", []byte(envelopeJSON), now)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Format != FormatEnvelope {
		t.Fatalf("format = %q, want %q", parsed.Format, FormatEnvelope)
	}
	if got := len(parsed.Document.Highlights); got != 3 {
		t.Fatalf("imported %d highlights, want 3", got)
	}

	first := parsed.Document.Highlights[0]
	if first.Quote != "the soul is the prison of the body" {
		t.Errorf("quote = %q; the envelope's `quote` is the passage", first.Quote)
	}
	if first.Note != "against the body/mind order" {
		t.Errorf("note = %q; the envelope's `text` is the comment", first.Note)
	}
	if first.ExternalID != "highlights-abc123" {
		t.Errorf("ref = %q, want the extractor's own id used as-is", first.ExternalID)
	}
	if first.Color != "#ffcc00" {
		t.Errorf("color = %q", first.Color)
	}

	// The extractor's own comments record having emitted a seven-digit hex
	// from an out-of-range channel. It is dropped rather than stored.
	if got := parsed.Document.Highlights[1].Color; got != "" {
		t.Errorf("color = %q, want a malformed value dropped", got)
	}
	if parsed.Document.Highlights[1].ExternalID == "" {
		t.Error("an entry with no id must still get a derived one")
	}

	// The snapshot itself stayed on the machine that ran the extractor, so
	// only the fact of it can be carried across.
	if got := parsed.Document.Highlights[2].Note; !strings.Contains(got, "dp-p200-1a2b3c4d.png") {
		t.Errorf("note = %q, want the figure's filename recorded", got)
	}
}

// TestFormatsAreNotConfused is the whole reason Parse inspects the entries
// rather than trusting a filename or a source_tag: reading one shape as the
// other imports every comment as a passage and discards every passage, and
// does so without any error at all.
func TestFormatsAreNotConfused(t *testing.T) {
	koreader, err := Parse("book.json", []byte(koreaderJSON), now)
	if err != nil {
		t.Fatalf("Parse KOReader: %v", err)
	}
	envelope, err := Parse("book.json", []byte(envelopeJSON), now)
	if err != nil {
		t.Fatalf("Parse envelope: %v", err)
	}

	// Same filename, opposite field conventions, both read correctly.
	if koreader.Format == envelope.Format {
		t.Fatal("both files were read as the same format")
	}
	if koreader.Document.Highlights[0].Note != "the mirror does the work" {
		t.Error("KOReader's note ended up somewhere other than Note")
	}
	if envelope.Document.Highlights[0].Note != "against the body/mind order" {
		t.Error("the envelope's note ended up somewhere other than Note")
	}
}

// TestIdentityIsStable is what makes re-importing an updated export an update
// rather than a second copy of the book.
func TestIdentityIsStable(t *testing.T) {
	first, err := Parse("a.json", []byte(koreaderJSON), now)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Same work, different capitalisation and spacing, different filename,
	// one extra annotation.
	variant := strings.Replace(koreaderJSON, `"  The Order of   Things "`, `"the order of things"`, 1)
	second, err := Parse("b.json", []byte(variant), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Parse variant: %v", err)
	}

	if first.Document.ExternalID != second.Document.ExternalID {
		t.Errorf("external id %q != %q; the same work must import into the same document",
			first.Document.ExternalID, second.Document.ExternalID)
	}
	for i := range first.Document.Highlights {
		if first.Document.Highlights[i].ExternalID != second.Document.Highlights[i].ExternalID {
			t.Errorf("highlight %d changed ref between identical exports", i)
		}
	}
}

// TestNoteEditChangesIdentity documents a deliberate compromise. KOReader
// does not update an annotation's datetime when its note is edited, so the
// note is folded into the ref — an edited note therefore arrives as a new
// annotation rather than silently not arriving at all.
func TestNoteEditChangesIdentity(t *testing.T) {
	before, err := Parse("a.json", []byte(koreaderJSON), now)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	edited := strings.Replace(koreaderJSON, "the mirror does the work", "the mirror does all of it", 1)
	after, err := Parse("a.json", []byte(edited), now)
	if err != nil {
		t.Fatalf("Parse edited: %v", err)
	}

	if before.Document.Highlights[0].ExternalID == after.Document.Highlights[0].ExternalID {
		t.Error("editing a note left the ref unchanged, so the edit would never be imported")
	}
}

func TestParseRejectsRubbish(t *testing.T) {
	for name, data := range map[string]string{
		"empty":     "",
		"plaintext": "just some notes I typed",
		"an array":  `[{"quote": "no envelope"}]`,
	} {
		if _, err := Parse("notes.json", []byte(data), now); err == nil {
			t.Errorf("%s: Parse succeeded, want an error", name)
		}
	}
}

func TestParseTimestampSpellings(t *testing.T) {
	for _, spelling := range []string{
		"2026-07-01 09:00:00",
		"2026-07-01T09:00:00+02:00",
		"2026-07-01",
		"D:20260701090000+02'00'",
		"D:2026",
	} {
		if parseTimestamp(spelling).IsZero() {
			t.Errorf("parseTimestamp(%q) returned the zero time", spelling)
		}
	}
	// Unparseable is not an error — the caller falls back to the upload's own
	// clock rather than failing an import over a timestamp.
	if !parseTimestamp("last tuesday").IsZero() {
		t.Error("parseTimestamp accepted nonsense")
	}
}
