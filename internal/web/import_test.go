package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/store"
)

// postFile posts one file plus form fields as multipart/form-data.
//
// The counterpart to post() for the one route that takes an upload. It is here
// rather than in web_test.go for the same reason import.go is not in
// server.go: everything about the upload path lives together.
func postFile(t *testing.T, server *Server, path, filename string, content []byte, fields url.Values) *httptest.ResponseRecorder {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, values := range fields {
		for _, value := range values {
			if err := writer.WriteField(name, value); err != nil {
				t.Fatalf("write field %s: %v", name, err)
			}
		}
	}
	if filename != "" {
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, path, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

const uploadJSON = `{
  "title": "The Order of Things",
  "author": "Michel Foucault",
  "created_on": "2026-07-30 08:14:02",
  "entries": [
    {"page": 42, "chapter": "Las Meninas", "datetime": "2026-07-01 09:00:00",
     "text": "the painter is standing a little back", "note": "the mirror does the work"},
    {"page": 43, "chapter": "Las Meninas", "datetime": "2026-07-01 09:05:00",
     "text": "an invisible relation", "note": ""},
    {"page": 91, "chapter": "The Prose of the World", "datetime": "2026-07-02 10:00:00",
     "text": "resemblance organised the play of symbols", "note": ""}
  ]
}`

// importedDocumentID uploads uploadJSON and returns the document it made.
func importedDocumentID(t *testing.T, server *Server, mode string) int64 {
	t.Helper()

	response := postFile(t, server, "/import", "book.json", []byte(uploadJSON),
		url.Values{"mode": {mode}, "subtitle": {"An Archaeology of the Human Sciences"}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("upload: status %d, body %s", response.Code, response.Body.String())
	}

	location := response.Header().Get("Location")
	id, err := strconv.ParseInt(strings.TrimPrefix(location, "/documents/"), 10, 64)
	if err != nil {
		t.Fatalf("upload redirected to %q, want a document page", location)
	}
	return id
}

func TestImportFormRenders(t *testing.T) {
	server, _, _ := newTestServer(t, false)

	response := get(t, server, "/import")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `enctype="multipart/form-data"`) {
		t.Error("the upload form is not multipart, so no file would ever arrive")
	}
	if !strings.Contains(body, `name="mode" value="triage" checked`) {
		t.Error("triage is not the default; a book could swamp the queue by leaving a radio alone")
	}
	if !strings.Contains(body, `name="author"`) {
		t.Error("the upload form does not offer an author field")
	}
}

// TestImportAuthorFieldOverridesTheFilesOwn covers the gap a scanned PDF
// leaves: its own metadata routinely has no author at all (confirmed against
// a real ocrmypdf file — no /Author, no XMP dc:creator), and until this field
// existed the only way to set one was a second visit to the document's own
// rename form after the upload had already landed with the field blank. The
// form's own value wins even when the file does carry an author, the same way
// the title field already does.
func TestImportAuthorFieldOverridesTheFilesOwn(t *testing.T) {
	server, db, _ := newTestServer(t, false)

	response := postFile(t, server, "/import", "book.json", []byte(uploadJSON),
		url.Values{"mode": {"triage"}, "author": {"A Corrected Author"}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("upload: status %d, body %s", response.Code, response.Body.String())
	}
	location := response.Header().Get("Location")
	id, err := strconv.ParseInt(strings.TrimPrefix(location, "/documents/"), 10, 64)
	if err != nil {
		t.Fatalf("upload redirected to %q, want a document page", location)
	}

	document, err := db.DocumentByID(id)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Author != "A Corrected Author" {
		t.Errorf("author = %q, want the form's own value even though the file claims %q",
			document.Author, "Michel Foucault")
	}
}

func TestImportCreatesAWorkAndLandsOnIt(t *testing.T) {
	server, db, _ := newTestServer(t, false)

	id := importedDocumentID(t, server, "triage")

	document, err := db.DocumentByID(id)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Source != store.SourceUpload {
		t.Errorf("source = %q, want %q", document.Source, store.SourceUpload)
	}
	if document.Subtitle != "An Archaeology of the Human Sciences" {
		t.Errorf("subtitle = %q, want the one typed on the form", document.Subtitle)
	}

	response := get(t, server, "/documents/"+strconv.FormatInt(id, 10))
	if response.Code != http.StatusOK {
		t.Fatalf("contents page: status %d", response.Code)
	}
	body := response.Body.String()

	// Grouped by chapter, in the work's own order, with the chapter list on
	// top — the thing a book's annotations need and a flat list cannot give.
	if !strings.Contains(body, "Las Meninas") || !strings.Contains(body, "The Prose of the World") {
		t.Error("the contents page does not show the chapters")
	}
	if strings.Index(body, "Las Meninas") > strings.Index(body, "The Prose of the World") {
		t.Error("chapters are out of the work's own order")
	}
	if !strings.Contains(body, "the mirror does the work") {
		t.Error("a reader's note on a passage is not shown")
	}
	if !strings.Contains(body, "/documents/"+strconv.FormatInt(id, 10)+"/triage") {
		t.Error("nothing offers to go through the untriaged annotations")
	}
}

// TestImportedWorkIsNotOfferedForReading covers the case that would otherwise
// be a 500: there is no body and no configured source to fetch one from, so
// /read has nothing to render.
func TestImportedWorkIsNotOfferedForReading(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	id := importedDocumentID(t, server, "triage")

	root, err := db.RootElement(id)
	if err != nil {
		t.Fatalf("RootElement: %v", err)
	}

	response := get(t, server, "/read/"+strconv.FormatInt(root.ID, 10))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want a redirect rather than an attempt to fetch a body", response.Code)
	}
	if got := response.Header().Get("Location"); got != "/documents/"+strconv.FormatInt(id, 10) {
		t.Errorf("redirected to %q, want the contents page", got)
	}
}

// TestTriagePassEmptiesItself walks the whole pass the way a reader would,
// and is the test that would catch a decision failing to advance it — which
// would leave the same passage on screen forever.
func TestTriagePassEmptiesItself(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	id := importedDocumentID(t, server, "triage")
	path := "/documents/" + strconv.FormatInt(id, 10) + "/triage"

	decisions := []url.Values{
		// "keep" needs a schedule choice now, the same as a grade or backlog
		// button would send — see triageSchedule.
		{"decision": {"keep"}, "grade": {"next"}},
		{"decision": {"suspend"}},
		{"decision": {"drop"}},
	}
	for step, form := range decisions {
		response := get(t, server, path)
		if response.Code != http.StatusOK {
			t.Fatalf("step %d: status %d, want another annotation to decide about", step, response.Code)
		}

		element, err := db.NextUntriaged(id)
		if err != nil {
			t.Fatalf("step %d: NextUntriaged: %v", step, err)
		}

		decided := post(t, server, "/elements/"+strconv.FormatInt(element.ID, 10)+"/triage", form)
		if decided.Code != http.StatusSeeOther {
			t.Fatalf("step %d: status %d, body %s", step, decided.Code, decided.Body.String())
		}
		// Straight back into the pass: triage is a rhythm, and a stop
		// between each decision breaks it.
		if got := decided.Header().Get("Location"); got != path {
			t.Errorf("step %d redirected to %q, want %q", step, got, path)
		}
	}

	// Nothing left, so the pass hands back to the contents page.
	response := get(t, server, path)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want the finished pass to redirect", response.Code)
	}

	counts, err := db.CountTriage(id)
	if err != nil {
		t.Fatalf("CountTriage: %v", err)
	}
	if !counts.Done() {
		t.Errorf("counts = %+v, want nothing left", counts)
	}
	// "drop" is a real delete, not a state change.
	if counts.Total != 2 {
		t.Errorf("total = %d, want the dropped annotation gone", counts.Total)
	}

	annotations, err := db.DocumentAnnotations(id)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if annotations[0].Schedule.State != ir.StateNew {
		t.Errorf("kept annotation state = %q", annotations[0].Schedule.State)
	}
	if annotations[1].Schedule.State != ir.StateSuspended {
		t.Errorf("parked annotation state = %q", annotations[1].Schedule.State)
	}
}

// TestTriageKeepAcceptsAScheduleChoice covers the point of reusing the
// reader's own schedule panel here: "keep" is not a single fixed delay any
// more, it applies whichever choice was actually pressed — a backlog preset
// in this case, the same param a preset button in the reader sends.
func TestTriageKeepAcceptsAScheduleChoice(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	id := importedDocumentID(t, server, "triage")

	element, err := db.NextUntriaged(id)
	if err != nil {
		t.Fatalf("NextUntriaged: %v", err)
	}

	response := post(t, server, "/elements/"+strconv.FormatInt(element.ID, 10)+"/triage",
		url.Values{"decision": {"keep"}, "days": {"14"}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %s", response.Code, response.Body.String())
	}

	kept, err := db.ElementByID(element.ID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if kept.Schedule.State != ir.StateNew {
		t.Errorf("state = %q, want new — an untriaged import starts suspended, and keeping it ends that", kept.Schedule.State)
	}
	wantDue := ir.Day(time.Now()).AddDate(0, 0, 14)
	if !kept.Schedule.DueOn.Equal(wantDue) {
		t.Errorf("due = %v, want %v (the chosen preset, not the old fixed extract delay)",
			kept.Schedule.DueOn, wantDue)
	}
}

// TestTriageKeepRejectsAnUnknownGrade guards the 400 path: a "keep" with
// neither days nor a recognised grade must not silently schedule something
// nobody actually chose.
func TestTriageKeepRejectsAnUnknownGrade(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	id := importedDocumentID(t, server, "triage")

	element, err := db.NextUntriaged(id)
	if err != nil {
		t.Fatalf("NextUntriaged: %v", err)
	}

	response := post(t, server, "/elements/"+strconv.FormatInt(element.ID, 10)+"/triage",
		url.Values{"decision": {"keep"}})
	if response.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a keep with no schedule choice", response.Code)
	}
}

// TestTriagePageOffersTheScheduleRow checks the page actually renders the
// reader's own schedule panel rather than the old flat Keep button.
func TestTriagePageOffersTheScheduleRow(t *testing.T) {
	server, _, _ := newTestServer(t, false)
	id := importedDocumentID(t, server, "triage")

	body := get(t, server, "/documents/"+strconv.FormatInt(id, 10)+"/triage").Body.String()
	for _, want := range []string{
		`"decision":"keep","grade":"sooner"`,
		`"decision":"keep","grade":"next"`,
		`decision: 'keep', days:`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("triage page is missing %q:\n%s", want, body)
		}
	}
}

// TestReaderShowsChapterAndPage covers the gap where a book-imported
// extract's chapter and page — already shown on the contents and triage
// pages — never made it onto the reader page itself.
func TestReaderShowsChapterAndPage(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	id := importedDocumentID(t, server, "triage")

	element, err := db.NextUntriaged(id)
	if err != nil {
		t.Fatalf("NextUntriaged: %v", err)
	}

	body := get(t, server, "/read/"+strconv.FormatInt(element.ID, 10)).Body.String()
	if !strings.Contains(body, "Las Meninas") {
		t.Errorf("reader page does not show the chapter:\n%s", body)
	}
	if !strings.Contains(body, "p. 42") {
		t.Errorf("reader page does not show the page:\n%s", body)
	}
}

func TestTriageResetOffersThePassAgain(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	id := importedDocumentID(t, server, "queue")

	// Queued outright, so the pass is already finished.
	counts, err := db.CountTriage(id)
	if err != nil {
		t.Fatalf("CountTriage: %v", err)
	}
	if !counts.Done() {
		t.Fatalf("counts = %+v, want a queued import to need no triage", counts)
	}

	response := post(t, server, "/documents/"+strconv.FormatInt(id, 10)+"/triage/reset", nil)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d", response.Code)
	}

	counts, err = db.CountTriage(id)
	if err != nil {
		t.Fatalf("CountTriage: %v", err)
	}
	if counts.Untriaged != 3 {
		t.Errorf("untriaged = %d, want all three offered again", counts.Untriaged)
	}
}

func TestDocumentTitlesAreEditable(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	id := importedDocumentID(t, server, "triage")
	path := "/documents/" + strconv.FormatInt(id, 10)

	response := post(t, server, path+"/titles", url.Values{
		"display_title": {"Les mots et les choses"},
		"subtitle":      {"une archéologie des sciences humaines"},
	})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d", response.Code)
	}

	document, err := db.DocumentByID(id)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Heading() != "Les mots et les choses" {
		t.Errorf("heading = %q", document.Heading())
	}
	// The file's own title is kept underneath, so clearing the override gets
	// it back rather than leaving the work nameless.
	if document.Title != "The Order of Things" {
		t.Errorf("title = %q, want the file's own preserved", document.Title)
	}

	if body := get(t, server, path).Body.String(); !strings.Contains(body, "Les mots et les choses") {
		t.Error("the contents page does not show the override")
	}
	if body := get(t, server, "/library").Body.String(); !strings.Contains(body, "Les mots et les choses") {
		t.Error("the library does not show the override")
	}
}

// TestDocumentAuthorIsEditableForUploads covers the field the rename form
// only offers for a source-upload document — see document.html and
// Store.UpdateDocumentAuthor.
func TestDocumentAuthorIsEditableForUploads(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	id := importedDocumentID(t, server, "triage")
	path := "/documents/" + strconv.FormatInt(id, 10)

	if body := get(t, server, path).Body.String(); !strings.Contains(body, `name="author"`) {
		t.Error("the rename form does not offer an author field for an uploaded work")
	}

	response := post(t, server, path+"/titles", url.Values{
		"display_title": {""},
		"subtitle":      {""},
		"author":        {"Corrected Name"},
	})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d", response.Code)
	}

	document, err := db.DocumentByID(id)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Author != "Corrected Name" {
		t.Errorf("author = %q, want the edit to have taken", document.Author)
	}
}

// TestDocumentAuthorFieldHiddenForWallabag guards the reason the field is
// conditional in the first place: a wallabag document's author is
// overwritten by the next sync, with no override column protecting an edit
// to it the way display_title protects a title edit, so offering it here
// would be a trap.
func TestDocumentAuthorFieldHiddenForWallabag(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	body := get(t, server, "/documents/1").Body.String()
	if strings.Contains(body, `name="author"`) {
		t.Error("the rename form offers an author field for a wallabag document, which the next sync would silently discard")
	}

	// Posting the rename form without an author key — exactly what the
	// template above sends for a wallabag document — must not touch it.
	response := post(t, server, "/documents/1/titles", url.Values{
		"display_title": {"A renamed article"},
		"subtitle":      {""},
	})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d", response.Code)
	}
	document, err := db.DocumentByID(1)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Author != "Someone" {
		t.Errorf("author = %q, want it left exactly as synced", document.Author)
	}
}

// TestEditAnnotationSavesCorrections covers the editor a malformed PDF
// extraction (OCR noise, a mis-split sentence) or an uncorrected KOReader
// export often needs — the passage, note and chapter are all edited by hand
// through the same document contents page the annotations already live on.
func TestEditAnnotationSavesCorrections(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	documentID := importedDocumentID(t, server, "triage")

	annotations, err := db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	target := annotations[0]

	response := post(t, server, "/elements/"+strconv.FormatInt(target.ID, 10)+"/annotation", url.Values{
		"quote":   {"the painter is standing well back"},
		"note":    {"corrected OCR noise"},
		"chapter": {"Las Meninas (corrected)"},
	})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Location"); got != "/documents/"+strconv.FormatInt(documentID, 10) {
		t.Errorf("Location = %q, want the document's own contents page", got)
	}

	edited, err := db.ElementByID(target.ID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if edited.Quote != "the painter is standing well back" {
		t.Errorf("quote = %q, want the edit to have taken", edited.Quote)
	}
	if edited.Chapter != "Las Meninas (corrected)" {
		t.Errorf("chapter = %q, want the edit to have taken", edited.Chapter)
	}

	body := get(t, server, "/documents/"+strconv.FormatInt(documentID, 10)).Body.String()
	if !strings.Contains(body, "the painter is standing well back") {
		t.Errorf("the document page does not show the corrected passage:\n%s", body)
	}
	if !strings.Contains(body, "Las Meninas (corrected)") {
		t.Errorf("the document page does not show the corrected chapter:\n%s", body)
	}
}

// TestEditAnnotationRejectsAnEmptyPassageAndNote mirrors the guard
// insertHighlights applies on import: an annotation with neither a passage
// nor a note is not an annotation any more, so the editor must not be able
// to save one into that state.
func TestEditAnnotationRejectsAnEmptyPassageAndNote(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	documentID := importedDocumentID(t, server, "triage")
	annotations, _ := db.DocumentAnnotations(documentID)

	response := post(t, server, "/elements/"+strconv.FormatInt(annotations[0].ID, 10)+"/annotation", url.Values{
		"quote": {""}, "note": {""}, "chapter": {"x"},
	})
	if response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.Code)
	}
}

// TestEditAnnotationRejectsARootElement guards against editing a document's
// own root topic through this route — it has no passage of its own, and the
// contents page never offers this form for it.
func TestEditAnnotationRejectsARootElement(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	documentID := importedDocumentID(t, server, "triage")
	root, err := db.RootElement(documentID)
	if err != nil {
		t.Fatalf("RootElement: %v", err)
	}

	response := post(t, server, "/elements/"+strconv.FormatInt(root.ID, 10)+"/annotation", url.Values{
		"quote": {"x"},
	})
	if response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.Code)
	}
}

// TestDocumentOffersAnnotationEditControls guards that the edit forms are
// actually reachable by browsing the contents page, not just callable by a
// client that already knows the routes exist.
func TestDocumentOffersAnnotationEditControls(t *testing.T) {
	server, _, _ := newTestServer(t, false)
	documentID := importedDocumentID(t, server, "triage")

	body := get(t, server, "/documents/"+strconv.FormatInt(documentID, 10)).Body.String()
	if !strings.Contains(body, `action="/elements/`) || !strings.Contains(body, `/annotation"`) {
		t.Errorf("the document page has no per-annotation edit form:\n%s", body)
	}
	if !strings.Contains(body, `id="chapter-form"`) {
		t.Error("the document page has no mass chapter-edit form")
	}
}

// TestSetChaptersBulkAssignsAndScopesToTheDocument is the mass chapter edit:
// several checked annotations get one chapter in a single request, and an id
// belonging to a different document — a tampered request, since no checkbox
// on the page could ever produce one — is ignored rather than honoured.
func TestSetChaptersBulkAssignsAndScopesToTheDocument(t *testing.T) {
	server, db, _ := newTestServer(t, false)
	documentID := importedDocumentID(t, server, "triage")
	annotations, err := db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}

	// A second, unrelated work — a different title so it lands as its own
	// document rather than merging with the first (identity is derived from
	// title and author; see annotations.documentID).
	otherResponse := postFile(t, server, "/import", "other.json", []byte(`{
	  "title": "A Different Book",
	  "entries": [{"page": 1, "chapter": "One", "text": "an unrelated passage"}]
	}`), url.Values{"mode": {"triage"}})
	if otherResponse.Code != http.StatusSeeOther {
		t.Fatalf("second upload: status %d, body %s", otherResponse.Code, otherResponse.Body.String())
	}
	otherDocumentID, err := strconv.ParseInt(
		strings.TrimPrefix(otherResponse.Header().Get("Location"), "/documents/"), 10, 64)
	if err != nil {
		t.Fatalf("second upload redirected to %q, want a document page", otherResponse.Header().Get("Location"))
	}
	otherAnnotations, err := db.DocumentAnnotations(otherDocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations (other): %v", err)
	}

	response := post(t, server, "/documents/"+strconv.FormatInt(documentID, 10)+"/chapters", url.Values{
		"chapter": {"Introduction (by colour)"},
		"ids": {
			strconv.FormatInt(annotations[0].ID, 10),
			strconv.FormatInt(annotations[1].ID, 10),
			strconv.FormatInt(otherAnnotations[0].ID, 10),
		},
	})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", response.Code, response.Body.String())
	}

	for _, id := range []int64{annotations[0].ID, annotations[1].ID} {
		element, err := db.ElementByID(id)
		if err != nil {
			t.Fatalf("ElementByID(%d): %v", id, err)
		}
		if element.Chapter != "Introduction (by colour)" {
			t.Errorf("element %d chapter = %q, want the bulk edit to have taken", id, element.Chapter)
		}
	}

	untouched, err := db.ElementByID(otherAnnotations[0].ID)
	if err != nil {
		t.Fatalf("ElementByID (other): %v", err)
	}
	if untouched.Chapter == "Introduction (by colour)" {
		t.Error("the bulk chapter edit reached into a different document")
	}
}

func TestImportRejectsAFileItCannotRead(t *testing.T) {
	server, _, _ := newTestServer(t, false)

	response := postFile(t, server, "/import", "notes.txt",
		[]byte("just some notes I typed"), url.Values{"mode": {"triage"}})
	// Back to the form with an explanation, not a 500: an unreadable file is
	// the reader's to fix, and they need to be told which.
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, want the form again", response.Code)
	}
	if !strings.Contains(response.Body.String(), "could not be imported") {
		t.Error("no explanation was shown")
	}
}

func TestImportWithNoFileAsks(t *testing.T) {
	server, _, _ := newTestServer(t, false)

	response := postFile(t, server, "/import", "", nil, url.Values{"mode": {"triage"}})
	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), "Choose a file") {
		t.Error("submitting with no file gave no useful message")
	}
}

// TestReuploadDoesNotDuplicate is the property that makes it safe to export a
// book again after adding a few highlights and upload the whole file.
func TestReuploadDoesNotDuplicate(t *testing.T) {
	server, db, _ := newTestServer(t, false)

	id := importedDocumentID(t, server, "triage")
	again := importedDocumentID(t, server, "triage")
	if again != id {
		t.Fatalf("the same file made two documents, %d and %d", id, again)
	}

	annotations, err := db.DocumentAnnotations(id)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if len(annotations) != 3 {
		t.Errorf("stored %d annotations after two identical uploads, want 3", len(annotations))
	}
}

func TestLibraryOffersTheBooksTab(t *testing.T) {
	server, _, _ := newTestServer(t, false)
	importedDocumentID(t, server, "triage")

	body := get(t, server, "/library").Body.String()
	if !strings.Contains(body, "/library?state=books") {
		t.Error("the library has no Books tab once a work has been uploaded")
	}
	if !strings.Contains(body, "to sort") {
		t.Error("the library does not show how many annotations are still untriaged")
	}

	filtered := get(t, server, "/library?state=books")
	if filtered.Code != http.StatusOK {
		t.Fatalf("books filter: status %d", filtered.Code)
	}
	if !strings.Contains(filtered.Body.String(), "The Order of Things") {
		t.Error("the books filter does not list the uploaded work")
	}
	// The article seeded by newTestServer is not a book.
	if strings.Contains(filtered.Body.String(), "A test article") {
		t.Error("the books filter listed a synced article")
	}
}
