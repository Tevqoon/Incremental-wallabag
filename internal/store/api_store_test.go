package store

import (
	"errors"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/source"
)

// seedAnnotatedDocument creates one document with one extract hanging off it,
// returning the document and extract ids.
func seedAnnotatedDocument(t *testing.T, db *Store, sourceName, externalID, title, quote string) (int64, int64) {
	t.Helper()

	if _, err := db.UpsertDocuments(sourceName, []source.Document{{
		ExternalID: externalID,
		Title:      title,
		URL:        "https://example.com/" + externalID,
		UpdatedAt:  time.Now(),
	}}, 0, 0, time.Now()); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	document, err := db.DocumentByExternalID(sourceName, externalID)
	if err != nil {
		t.Fatalf("DocumentByExternalID: %v", err)
	}
	root, err := db.RootElement(document.ID)
	if err != nil {
		t.Fatalf("RootElement: %v", err)
	}

	extractID, err := db.CreateExtract(NewExtract{
		ParentID:    root.ID,
		DocumentID:  document.ID,
		Kind:        KindTopic,
		Title:       SummariseQuote(quote),
		ContentHTML: "<p>" + quote + "</p>",
		Quote:       quote,
	}, time.Now())
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}
	return document.ID, extractID
}

func TestAnnotatedDocumentsOmitsDocumentsWithNoAnnotations(t *testing.T) {
	db := testStore(t)

	// A document with nothing harvested from it. It has a root topic, which
	// is exactly what the parent_id filter has to exclude — counting roots
	// would make every synced article look annotated.
	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "bare", Title: "Nothing harvested", UpdatedAt: time.Now(),
	}}, 0, 0, time.Now()); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	seedAnnotatedDocument(t, db, "wallabag", "1", "Has a passage", "A passage")

	documents, err := db.AnnotatedDocuments(AnnotatedFilter{})
	if err != nil {
		t.Fatalf("AnnotatedDocuments: %v", err)
	}
	if len(documents) != 1 {
		t.Fatalf("documents = %d, want 1", len(documents))
	}
	if documents[0].Title != "Has a passage" {
		t.Errorf("title = %q, want the annotated document", documents[0].Title)
	}
	if documents[0].Annotations != 1 {
		t.Errorf("annotations = %d, want 1", documents[0].Annotations)
	}
	if documents[0].AnnotationsUpdatedAt.IsZero() {
		t.Error("AnnotationsUpdatedAt is zero, so nothing can poll on it")
	}
}

// TestAnnotatedDocumentsSinceIsStrict pins the boundary: handing back the
// timestamp from a previous response must not return the row that produced
// it, or every poll re-reports the same document forever.
func TestAnnotatedDocumentsSinceIsStrict(t *testing.T) {
	db := testStore(t)
	seedAnnotatedDocument(t, db, "wallabag", "1", "An article", "A passage")

	documents, err := db.AnnotatedDocuments(AnnotatedFilter{})
	if err != nil {
		t.Fatalf("AnnotatedDocuments: %v", err)
	}
	watermark := documents[0].AnnotationsUpdatedAt

	again, err := db.AnnotatedDocuments(AnnotatedFilter{Since: watermark})
	if err != nil {
		t.Fatalf("AnnotatedDocuments(since): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("documents = %d, want 0 at exactly the watermark", len(again))
	}

	earlier, err := db.AnnotatedDocuments(AnnotatedFilter{Since: watermark.Add(-time.Second)})
	if err != nil {
		t.Fatalf("AnnotatedDocuments(earlier): %v", err)
	}
	if len(earlier) != 1 {
		t.Fatalf("documents = %d, want 1 just before the watermark", len(earlier))
	}
}

func TestAnnotatedDocumentsFiltersBySource(t *testing.T) {
	db := testStore(t)
	seedAnnotatedDocument(t, db, "wallabag", "1", "An article", "A passage")
	seedAnnotatedDocument(t, db, SourceUpload, "book", "A book", "A page")

	all, err := db.AnnotatedDocuments(AnnotatedFilter{})
	if err != nil {
		t.Fatalf("AnnotatedDocuments: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("documents = %d, want 2 with no source filter", len(all))
	}

	uploads, err := db.AnnotatedDocuments(AnnotatedFilter{Source: SourceUpload})
	if err != nil {
		t.Fatalf("AnnotatedDocuments(upload): %v", err)
	}
	if len(uploads) != 1 {
		t.Fatalf("documents = %d, want 1 upload", len(uploads))
	}
	if uploads[0].Title != "A book" {
		t.Errorf("title = %q, want the uploaded document", uploads[0].Title)
	}
}

func TestAnnotatedDocumentsHonoursLimit(t *testing.T) {
	db := testStore(t)
	seedAnnotatedDocument(t, db, "wallabag", "1", "First", "A passage")
	seedAnnotatedDocument(t, db, "wallabag", "2", "Second", "Another passage")

	limited, err := db.AnnotatedDocuments(AnnotatedFilter{Limit: 1})
	if err != nil {
		t.Fatalf("AnnotatedDocuments: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("documents = %d, want 1", len(limited))
	}
}

// TestEditAnnotationOnlyTouchesWhatItWasGiven is the property the pointer
// fields exist for.
func TestEditAnnotationOnlyTouchesWhatItWasGiven(t *testing.T) {
	db := testStore(t)
	_, id := seedAnnotatedDocument(t, db, "wallabag", "1", "An article", "A passage")

	if err := db.UpdateAnnotation(id, "A passage", "A note", "Chapter One", time.Now()); err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}

	title := "A new title"
	if err := db.EditAnnotation(id, AnnotationEdit{Title: &title}, time.Now()); err != nil {
		t.Fatalf("EditAnnotation: %v", err)
	}

	element, err := db.ElementByID(id)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if element.Title != "A new title" {
		t.Errorf("title = %q", element.Title)
	}
	if element.Note != "A note" {
		t.Errorf("note = %q, want it untouched", element.Note)
	}
	if element.Chapter != "Chapter One" {
		t.Errorf("chapter = %q, want it untouched", element.Chapter)
	}
	if element.Quote != "A passage" {
		t.Errorf("quote = %q, want it untouched", element.Quote)
	}
}

// TestEditAnnotationDistinguishesAbsentFromEmpty: a pointer to "" clears a
// field, while a nil pointer leaves it alone. Conflating the two is what makes
// a partial update destructive.
func TestEditAnnotationDistinguishesAbsentFromEmpty(t *testing.T) {
	db := testStore(t)
	_, id := seedAnnotatedDocument(t, db, "wallabag", "1", "An article", "A passage")

	if err := db.UpdateAnnotation(id, "A passage", "A note", "Chapter One", time.Now()); err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}

	empty := ""
	if err := db.EditAnnotation(id, AnnotationEdit{Note: &empty}, time.Now()); err != nil {
		t.Fatalf("EditAnnotation: %v", err)
	}

	element, err := db.ElementByID(id)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if element.Note != "" {
		t.Errorf("note = %q, want it cleared", element.Note)
	}
	if element.Chapter != "Chapter One" {
		t.Errorf("chapter = %q, want it untouched", element.Chapter)
	}
}

// TestEditedQuoteDoesNotDisturbTheAnchorText is the invariant the whole
// two-column design rests on: DisplayQuote changes, Quote does not, and
// content_html — the faithful record of what arrived — does not either.
func TestEditedQuoteDoesNotDisturbTheAnchorText(t *testing.T) {
	db := testStore(t)
	_, id := seedAnnotatedDocument(t, db, "wallabag", "1", "An article", "Mangled  text")

	before, err := db.ElementByID(id)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}

	corrected := "Corrected text"
	if err := db.EditAnnotation(id, AnnotationEdit{EditedQuote: &corrected}, time.Now()); err != nil {
		t.Fatalf("EditAnnotation: %v", err)
	}

	after, err := db.ElementByID(id)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if after.Quote != before.Quote {
		t.Errorf("quote changed from %q to %q; anchoring and write-back both read it",
			before.Quote, after.Quote)
	}
	if after.ContentHTML != before.ContentHTML {
		t.Errorf("content_html changed from %q to %q", before.ContentHTML, after.ContentHTML)
	}
	if after.DisplayQuote() != corrected {
		t.Errorf("DisplayQuote() = %q, want %q", after.DisplayQuote(), corrected)
	}
	if !after.Edited() {
		t.Error("Edited() = false after an override")
	}

	// Clearing it falls back rather than leaving the passage blank.
	empty := ""
	if err := db.EditAnnotation(id, AnnotationEdit{EditedQuote: &empty}, time.Now()); err != nil {
		t.Fatalf("EditAnnotation(clear): %v", err)
	}
	reverted, err := db.ElementByID(id)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if reverted.DisplayQuote() != before.Quote {
		t.Errorf("DisplayQuote() = %q, want the original %q",
			reverted.DisplayQuote(), before.Quote)
	}
	if reverted.Edited() {
		t.Error("Edited() = true after clearing the override")
	}
}

// TestEditAnnotationRejectsRoot: a document's root topic stands for the whole
// work and has no passage of its own, the same scope UpdateAnnotation keeps.
func TestEditAnnotationRejectsRoot(t *testing.T) {
	db := testStore(t)
	documentID, _ := seedAnnotatedDocument(t, db, "wallabag", "1", "An article", "A passage")

	root, err := db.RootElement(documentID)
	if err != nil {
		t.Fatalf("RootElement: %v", err)
	}

	title := "Not allowed"
	err = db.EditAnnotation(root.ID, AnnotationEdit{Title: &title}, time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("EditAnnotation on a root = %v, want ErrNotFound", err)
	}
}

// TestEditAnnotationWithNothingSetIsANoOp: a consumer sending an empty patch
// should not bump updated_at, or every poll would see a change it did not
// make.
func TestEditAnnotationWithNothingSetIsANoOp(t *testing.T) {
	db := testStore(t)
	_, id := seedAnnotatedDocument(t, db, "wallabag", "1", "An article", "A passage")

	before, err := db.ElementByID(id)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}

	if err := db.EditAnnotation(id, AnnotationEdit{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("EditAnnotation: %v", err)
	}

	after, err := db.ElementByID(id)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("updated_at moved from %v to %v on an empty edit",
			before.UpdatedAt, after.UpdatedAt)
	}
}
