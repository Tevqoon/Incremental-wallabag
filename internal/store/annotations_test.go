package store

import (
	"errors"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/source"
)

// book is an uploaded work with three annotations across two chapters, one of
// them note-only.
func book() source.Document {
	return source.Document{
		ExternalID: "abc123",
		Title:      "The Order of Things",
		Author:     "Michel Foucault",
		UpdatedAt:  time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
		Highlights: []source.Highlight{
			{ExternalID: "k-1", Quote: "the painter is standing back", Note: "the mirror does it",
				Chapter: "Las Meninas", Page: "42", Color: "#ffcc00", Ordinal: 1},
			{ExternalID: "k-2", Quote: "an invisible relation",
				Chapter: "Las Meninas", Page: "43", Ordinal: 2},
			{ExternalID: "k-3", Note: "come back to this",
				Chapter: "The Prose of the World", Page: "91", Ordinal: 3},
		},
	}
}

func TestImportAnnotationsCreatesASuspendedWork(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	result, err := db.ImportAnnotations(book(), ImportOptions{
		Subtitle: "An Archaeology of the Human Sciences",
		Triage:   true,
	}, now)
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}
	if !result.Created || result.Imported != 3 || result.Offered != 3 {
		t.Fatalf("result = %+v, want a new work with all three imported", result)
	}

	// The root topic is suspended because there is no body to read: putting a
	// bodyless topic in the queue would offer a page with nothing on it.
	root, err := db.RootElement(result.DocumentID)
	if err != nil {
		t.Fatalf("RootElement: %v", err)
	}
	if root.Schedule.State != ir.StateSuspended {
		t.Errorf("root state = %q, want suspended", root.Schedule.State)
	}

	document, err := db.DocumentByID(result.DocumentID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Source != SourceUpload {
		t.Errorf("source = %q, want %q", document.Source, SourceUpload)
	}
	if document.Subtitle != "An Archaeology of the Human Sciences" {
		t.Errorf("subtitle = %q", document.Subtitle)
	}

	annotations, err := db.DocumentAnnotations(result.DocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if len(annotations) != 3 {
		t.Fatalf("stored %d annotations, want 3", len(annotations))
	}

	// Triage mode parks every annotation out of the queue. The whole point:
	// a book yields hundreds of passages and they must not swamp the queue
	// before anyone has decided which are worth having.
	for _, annotation := range annotations {
		if annotation.Schedule.State != ir.StateSuspended {
			t.Errorf("annotation %d state = %q, want suspended", annotation.ID, annotation.Schedule.State)
		}
		if annotation.Triaged() {
			t.Errorf("annotation %d arrived already triaged", annotation.ID)
		}
	}

	first := annotations[0]
	if first.Chapter != "Las Meninas" || first.Page != "42" || first.Color != "#ffcc00" {
		t.Errorf("book metadata not stored: %+v", first)
	}
	if first.Note != "the mirror does it" {
		t.Errorf("note = %q; the wallabag path used to drop this on the floor", first.Note)
	}
	// The chapter is a better title than the first eighty characters of a
	// passage that is already shown directly underneath it.
	if first.Title != "Las Meninas" {
		t.Errorf("title = %q, want the chapter", first.Title)
	}

	// A note with no passage is still something the reader wrote.
	if third := annotations[2]; third.Quote != "" || third.Note != "come back to this" {
		t.Errorf("note-only annotation = %+v", third)
	}
}

func TestImportAnnotationsCanQueueImmediately(t *testing.T) {
	db := testStore(t)

	result, err := db.ImportAnnotations(book(), ImportOptions{FloorDays: 10}, time.Now())
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}

	annotations, err := db.DocumentAnnotations(result.DocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	for _, annotation := range annotations {
		if annotation.Schedule.State != ir.StateNew {
			t.Errorf("annotation %d state = %q, want new", annotation.ID, annotation.Schedule.State)
		}
		// Choosing to queue the whole file is itself the decision, so a
		// triage pass must not offer them again.
		if !annotation.Triaged() {
			t.Errorf("annotation %d was queued outright but left untriaged", annotation.ID)
		}
	}

	counts, err := db.CountTriage(result.DocumentID)
	if err != nil {
		t.Fatalf("CountTriage: %v", err)
	}
	if counts.Untriaged != 0 || !counts.Done() {
		t.Errorf("counts = %+v, want nothing left to triage", counts)
	}
}

// TestReimportUpdatesRatherThanDuplicates is what makes it safe to re-export
// a book after adding a few more highlights and upload the whole thing again.
func TestReimportUpdatesRatherThanDuplicates(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	first, err := db.ImportAnnotations(book(), ImportOptions{Triage: true}, now)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}

	// The same file again, with one annotation's chapter corrected and one
	// new annotation added in the middle of the work. A real re-export
	// renumbers from the top, so the annotation that used to be third is now
	// fourth — which is the whole reason ordinal is carried rather than
	// leaning on the order rows happened to be inserted in.
	original := book().Highlights
	original[1].Chapter = "Las Meninas (revisited)"
	original[2].Ordinal = 4

	updated := book()
	updated.Highlights = []source.Highlight{
		original[0],
		original[1],
		{ExternalID: "k-4", Quote: "a new passage", Chapter: "Las Meninas", Page: "44", Ordinal: 3},
		original[2],
	}

	second, err := db.ImportAnnotations(updated, ImportOptions{Triage: true}, now)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Created {
		t.Error("re-importing the same work created a second document")
	}
	if second.DocumentID != first.DocumentID {
		t.Fatalf("document id changed from %d to %d", first.DocumentID, second.DocumentID)
	}
	if second.Imported != 1 {
		t.Errorf("imported %d, want only the one genuinely new annotation", second.Imported)
	}

	annotations, err := db.DocumentAnnotations(second.DocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if len(annotations) != 4 {
		t.Fatalf("stored %d annotations, want 4", len(annotations))
	}

	// A correction in the re-uploaded file lands, because re-uploading is how
	// these are corrected.
	var revisited bool
	for _, annotation := range annotations {
		if annotation.ExternalRef == "k-2" && annotation.Chapter == "Las Meninas (revisited)" {
			revisited = true
		}
	}
	if !revisited {
		t.Error("a corrected chapter was discarded as already imported")
	}

	// Reading order comes from the file's own ordinals, so the new annotation
	// sits where it belongs rather than at the end.
	if annotations[2].ExternalRef != "k-4" {
		t.Errorf("annotation order = %q at position 3, want the newly inserted one",
			annotations[2].ExternalRef)
	}
}

// TestNoteOnlyAnnotationsStayDistinct guards a subtle failure. insertHighlights
// looks for an existing row with the same quote under a different ref, to
// survive an upstream id churning. With note-only annotations that lookup would
// match every empty quote against every other, and the first note imported
// would swallow the ref of each one after it.
func TestNoteOnlyAnnotationsStayDistinct(t *testing.T) {
	db := testStore(t)

	document := source.Document{
		ExternalID: "notes", Title: "Marginalia", UpdatedAt: time.Now(),
		Highlights: []source.Highlight{
			{ExternalID: "n-1", Note: "first thought", Page: "1", Ordinal: 1},
			{ExternalID: "n-2", Note: "second thought", Page: "2", Ordinal: 2},
			{ExternalID: "n-3", Note: "third thought", Page: "3", Ordinal: 3},
		},
	}

	result, err := db.ImportAnnotations(document, ImportOptions{Triage: true}, time.Now())
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}
	if result.Imported != 3 {
		t.Fatalf("imported %d note-only annotations, want 3", result.Imported)
	}

	annotations, err := db.DocumentAnnotations(result.DocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	seen := map[string]bool{}
	for _, annotation := range annotations {
		if seen[annotation.ExternalRef] {
			t.Errorf("ref %q appears twice", annotation.ExternalRef)
		}
		seen[annotation.ExternalRef] = true
	}
	if len(seen) != 3 {
		t.Errorf("stored %d distinct refs, want 3", len(seen))
	}
}

func TestImportMergesIntoAnExistingWork(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	first, err := db.ImportAnnotations(book(), ImportOptions{Triage: true}, now)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}

	// The same book annotated in a different tool, which calls it something
	// else. Merging is the reader's assertion that these belong together.
	other := source.Document{
		ExternalID: "totally-different",
		Title:      "Les mots et les choses",
		UpdatedAt:  now,
		Highlights: []source.Highlight{
			{ExternalID: "p-1", Quote: "from the PDF", Page: "12", Ordinal: 1},
		},
	}

	second, err := db.ImportAnnotations(other, ImportOptions{
		IntoDocumentID: first.DocumentID, Triage: true,
	}, now)
	if err != nil {
		t.Fatalf("merge import: %v", err)
	}
	if second.DocumentID != first.DocumentID {
		t.Fatalf("merged into %d, want %d", second.DocumentID, first.DocumentID)
	}

	annotations, err := db.DocumentAnnotations(first.DocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if len(annotations) != 4 {
		t.Errorf("stored %d annotations, want the two files' four together", len(annotations))
	}

	// The picked document keeps its own title; the merged file's does not
	// overwrite it, which is very often exactly why it was picked.
	document, err := db.DocumentByID(first.DocumentID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Title != "The Order of Things" {
		t.Errorf("title = %q, want the target's own kept", document.Title)
	}
}

func TestImportIntoAMissingDocumentFails(t *testing.T) {
	db := testStore(t)
	_, err := db.ImportAnnotations(book(), ImportOptions{IntoDocumentID: 9999}, time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestTriagePassWalksInReadingOrder(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	result, err := db.ImportAnnotations(book(), ImportOptions{Triage: true}, now)
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}

	// Reading order, not priority order: that is what makes triage a pass
	// over a work rather than a second copy of the reading queue.
	var walked []string
	for {
		element, err := db.NextUntriaged(result.DocumentID)
		if errors.Is(err, ErrNotFound) {
			break
		}
		if err != nil {
			t.Fatalf("NextUntriaged: %v", err)
		}
		walked = append(walked, element.ExternalRef)

		switch len(walked) {
		case 1:
			// An untriaged import starts suspended; "keep" is the decision
			// that ends that, same as web.triageSchedule makes explicit.
			kept := ir.Backlog(element.Schedule, 10, now)
			kept.State = ir.StateNew
			if err := db.KeepTriaged(element.ID, kept, now); err != nil {
				t.Fatalf("KeepTriaged: %v", err)
			}
		case 2:
			if err := db.SuspendTriaged(element.ID, now); err != nil {
				t.Fatalf("SuspendTriaged: %v", err)
			}
		default:
			if err := db.MarkTriaged(element.ID, now); err != nil {
				t.Fatalf("MarkTriaged: %v", err)
			}
		}
		if len(walked) > 10 {
			t.Fatal("the pass never finished; a decision failed to advance it")
		}
	}

	if want := []string{"k-1", "k-2", "k-3"}; len(walked) != 3 ||
		walked[0] != want[0] || walked[1] != want[1] || walked[2] != want[2] {
		t.Fatalf("walked %v, want %v", walked, want)
	}

	counts, err := db.CountTriage(result.DocumentID)
	if err != nil {
		t.Fatalf("CountTriage: %v", err)
	}
	if !counts.Done() {
		t.Errorf("counts = %+v, want the pass finished", counts)
	}

	annotations, err := db.DocumentAnnotations(result.DocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if state := annotations[0].Schedule.State; state != ir.StateNew {
		t.Errorf("kept annotation state = %q, want new", state)
	}
	// Kept comes back after the delay, not today: the value of a passage is
	// re-reading it once its context has faded, and triage was that sitting.
	if due := annotations[0].Schedule.DueOn; !due.After(ir.Day(now)) {
		t.Errorf("kept annotation due %v, want later than today", due)
	}
	if state := annotations[1].Schedule.State; state != ir.StateSuspended {
		t.Errorf("parked annotation state = %q, want suspended", state)
	}
}

// TestResetTriageForgetsDecisionsButNotSchedules — going through a book again
// is a chance to reconsider, not an undo.
func TestResetTriageForgetsDecisionsButNotSchedules(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	result, err := db.ImportAnnotations(book(), ImportOptions{Triage: true}, now)
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}
	element, err := db.NextUntriaged(result.DocumentID)
	if err != nil {
		t.Fatalf("NextUntriaged: %v", err)
	}
	schedule := ir.Backlog(element.Schedule, 10, now)
	schedule.State = ir.StateNew
	if err := db.KeepTriaged(element.ID, schedule, now); err != nil {
		t.Fatalf("KeepTriaged: %v", err)
	}

	if err := db.ResetTriage(result.DocumentID); err != nil {
		t.Fatalf("ResetTriage: %v", err)
	}

	counts, err := db.CountTriage(result.DocumentID)
	if err != nil {
		t.Fatalf("CountTriage: %v", err)
	}
	if counts.Untriaged != 3 {
		t.Errorf("untriaged = %d, want all three offered again", counts.Untriaged)
	}

	kept, err := db.ElementByID(element.ID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if kept.Schedule.State != ir.StateNew {
		t.Errorf("state = %q; resetting the pass must not unschedule what it kept", kept.Schedule.State)
	}
}

func TestDisplayTitleOverridesWithoutTouchingTheSyncedTitle(t *testing.T) {
	db := testStore(t)

	// A synced document, whose title a sync overwrites wholesale — which is
	// exactly why the override is a separate column.
	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "Some article", UpdatedAt: time.Now(),
	}}, 0, 0, time.Now()); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if err := db.UpdateDocumentTitles(1, "What I call it", "and a subtitle"); err != nil {
		t.Fatalf("UpdateDocumentTitles: %v", err)
	}

	// A later sync, carrying the provider's own title again.
	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "Some article, retitled upstream", UpdatedAt: time.Now(),
	}}, 0, 0, time.Now()); err != nil {
		t.Fatalf("second UpsertDocuments: %v", err)
	}

	document, err := db.DocumentByID(1)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Title != "Some article, retitled upstream" {
		t.Errorf("title = %q, want the sync's own value", document.Title)
	}
	if document.Heading() != "What I call it" {
		t.Errorf("heading = %q, want the override to survive the sync", document.Heading())
	}
	if document.Subtitle != "and a subtitle" {
		t.Errorf("subtitle = %q", document.Subtitle)
	}

	// Clearing the override has to be possible, or a bad rename is permanent.
	if err := db.UpdateDocumentTitles(1, "", ""); err != nil {
		t.Fatalf("clear titles: %v", err)
	}
	document, err = db.DocumentByID(1)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Heading() != "Some article, retitled upstream" {
		t.Errorf("heading = %q, want the fallback to the real title", document.Heading())
	}
}

// TestUpdateDocumentAuthor covers the direct-overwrite path that backs the
// rename form's Author field, offered only for an uploaded work — see
// document.html and Store.UpdateDocumentAuthor's own doc comment for why a
// wallabag document does not get the same field.
func TestUpdateDocumentAuthor(t *testing.T) {
	db := testStore(t)

	result, err := db.ImportAnnotations(book(), ImportOptions{Triage: true}, time.Now())
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}

	if err := db.UpdateDocumentAuthor(result.DocumentID, "Corrected Name"); err != nil {
		t.Fatalf("UpdateDocumentAuthor: %v", err)
	}

	document, err := db.DocumentByID(result.DocumentID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Author != "Corrected Name" {
		t.Errorf("author = %q, want the edit to have taken", document.Author)
	}

	if err := db.UpdateDocumentAuthor(9999, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound for a missing document", err)
	}
}

// TestUpdateAnnotationEditsThePassage covers the correction a malformed PDF
// extraction needs: the quote, note and chapter are all edited by hand, and
// content_html is rebuilt from the new text rather than left holding the
// original's now-stale markup.
func TestUpdateAnnotationEditsThePassage(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	result, err := db.ImportAnnotations(book(), ImportOptions{Triage: true}, now)
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}
	annotations, err := db.DocumentAnnotations(result.DocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	target := annotations[0]

	if err := db.UpdateAnnotation(target.ID,
		"the painter is standing well back", "corrected OCR noise",
		"Las Meninas (corrected)", now,
	); err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}

	edited, err := db.ElementByID(target.ID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if edited.Quote != "the painter is standing well back" {
		t.Errorf("quote = %q, want the edit to have taken", edited.Quote)
	}
	if edited.Note != "corrected OCR noise" {
		t.Errorf("note = %q, want the edit to have taken", edited.Note)
	}
	if edited.Chapter != "Las Meninas (corrected)" {
		t.Errorf("chapter = %q, want the edit to have taken", edited.Chapter)
	}
	wantHTML := `<p>the painter is standing well back</p><p class="annotation-note">corrected OCR noise</p>`
	if edited.ContentHTML != wantHTML {
		t.Errorf("content_html = %q, want %q", edited.ContentHTML, wantHTML)
	}

	if err := db.UpdateAnnotation(9999, "x", "", "", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound for a missing element", err)
	}

	root, err := db.RootElement(result.DocumentID)
	if err != nil {
		t.Fatalf("RootElement: %v", err)
	}
	if err := db.UpdateAnnotation(root.ID, "x", "", "", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound for a document's own root topic", err)
	}
}

// TestSetAnnotationChapterLeavesThePassageAlone is the mass chapter edit's
// single-row primitive: only chapter changes, not the passage or note it
// would be careless to overwrite on a batch of otherwise-unrelated rows.
func TestSetAnnotationChapterLeavesThePassageAlone(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	result, err := db.ImportAnnotations(book(), ImportOptions{Triage: true}, now)
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}
	annotations, err := db.DocumentAnnotations(result.DocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	target := annotations[0]
	originalQuote := target.Quote

	if err := db.SetAnnotationChapter(target.ID, "Introduction (by colour)", now); err != nil {
		t.Fatalf("SetAnnotationChapter: %v", err)
	}

	edited, err := db.ElementByID(target.ID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if edited.Chapter != "Introduction (by colour)" {
		t.Errorf("chapter = %q, want the edit to have taken", edited.Chapter)
	}
	if edited.Quote != originalQuote {
		t.Errorf("quote = %q, want it left alone by a chapter-only edit", edited.Quote)
	}

	if err := db.SetAnnotationChapter(9999, "x", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound for a missing element", err)
	}
}

// TestWallabagHighlightsAreUnaffected pins the sync path's behaviour, since
// insertHighlights now serves two callers with different needs.
func TestWallabagHighlightsAreUnaffected(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "An article", UpdatedAt: now,
		Highlights: []source.Highlight{{ExternalID: "h-1", Quote: "a passage"}},
	}}, 10, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	annotations, err := db.DocumentAnnotations(1)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if len(annotations) != 1 {
		t.Fatalf("stored %d highlights, want 1", len(annotations))
	}

	highlight := annotations[0]
	if highlight.Schedule.State != ir.StateNew {
		t.Errorf("state = %q, want new — a synced highlight is not triaged", highlight.Schedule.State)
	}
	if highlight.Schedule.DueOn.IsZero() {
		t.Error("a synced highlight lost its spread due date")
	}
	// Its title still summarises the passage, because a wallabag highlight
	// carries no chapter to use instead.
	if highlight.Title != "a passage" {
		t.Errorf("title = %q, want the summarised quote", highlight.Title)
	}
}

func TestCountByStateCountsEverySourceWhenUnfiltered(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "An article", UpdatedAt: now,
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	if _, err := db.ImportAnnotations(book(), ImportOptions{Triage: true}, now); err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}

	// The tabs must agree with the list beneath them, and SearchDocuments has
	// never filtered by source.
	counts, err := db.CountByState("", ir.Day(now))
	if err != nil {
		t.Fatalf("CountByState: %v", err)
	}
	if counts["all"] != 2 {
		t.Errorf("all = %d, want both sources counted", counts["all"])
	}
	if counts["books"] != 1 {
		t.Errorf("books = %d, want the uploaded work", counts["books"])
	}

	entries, err := db.SearchDocuments(LibraryFilter{State: "books"}, ir.Day(now))
	if err != nil {
		t.Fatalf("SearchDocuments: %v", err)
	}
	if len(entries) != 1 || !entries[0].IsUpload() {
		t.Fatalf("books filter returned %+v", entries)
	}
	if entries[0].UntriagedCount != 3 {
		t.Errorf("untriaged = %d, want 3", entries[0].UntriagedCount)
	}
}
