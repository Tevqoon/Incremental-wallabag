package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/store"
)

// patch issues a PATCH with a JSON body, the shape every write to this API
// takes.
func patch(t *testing.T, server *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// decodeJSON parses a response body, failing the test rather than returning an
// error: every caller here would only turn one into a Fatalf anyway.
func decodeJSON[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, response.Body.String())
	}
	return value
}

type documentsResponse struct {
	Documents []apiDocument `json:"documents"`
}

// seedAnnotation creates one extract on the seeded article and returns its id.
func seedAnnotation(t *testing.T, db *store.Store, quote string) int64 {
	t.Helper()
	id, err := db.CreateExtract(store.NewExtract{
		ParentID:    1,
		DocumentID:  1,
		Kind:        store.KindTopic,
		Title:       store.SummariseQuote(quote),
		ContentHTML: "<p>" + quote + "</p>",
		Quote:       quote,
	}, time.Now())
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}
	return id
}

func TestAPIDocumentsListsOnlyAnnotatedDocuments(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	// Before anything is harvested the article has no passages, so it has
	// nothing for a consumer to mirror and must not appear.
	empty := decodeJSON[documentsResponse](t, get(t, server, "/api/documents"))
	if len(empty.Documents) != 0 {
		t.Fatalf("documents = %d, want 0 before any annotation exists", len(empty.Documents))
	}

	seedAnnotation(t, db, "A harvested passage")

	response := get(t, server, "/api/documents")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	listed := decodeJSON[documentsResponse](t, response)
	if len(listed.Documents) != 1 {
		t.Fatalf("documents = %d, want 1", len(listed.Documents))
	}

	document := listed.Documents[0]
	if document.Title != "A test article" {
		t.Errorf("title = %q, want %q", document.Title, "A test article")
	}
	if document.URL != "https://example.com/article" {
		t.Errorf("url = %q", document.URL)
	}
	if document.AnnotationCount != 1 {
		t.Errorf("annotation_count = %d, want 1", document.AnnotationCount)
	}
	if document.AnnotationsUpdatedAt == "" {
		t.Error("annotations_updated_at is empty, so a consumer has nothing to poll on")
	}
	// The feed carries no bodies: that is what keeps listing a whole library
	// one small response.
	if len(document.Annotations) != 0 {
		t.Errorf("the feed embedded %d annotations, want 0", len(document.Annotations))
	}
}

// TestAPIDocumentsSince is the incremental-poll contract: feeding the newest
// timestamp from one response back in returns nothing until something
// actually changes, and returns the document again once it does. Getting this
// wrong in either direction is what makes an exporter either rewrite every
// file on every run or silently miss an edit.
func TestAPIDocumentsSince(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "A harvested passage")

	first := decodeJSON[documentsResponse](t, get(t, server, "/api/documents"))
	watermark := first.Documents[0].AnnotationsUpdatedAt

	quiet := decodeJSON[documentsResponse](t, get(t, server, "/api/documents?since="+watermark))
	if len(quiet.Documents) != 0 {
		t.Fatalf("documents = %d, want 0 when nothing changed since the watermark",
			len(quiet.Documents))
	}

	// Stored timestamps have second resolution, so an edit inside the same
	// second as the seed would be indistinguishable from it — which is a
	// property of the storage format, not of the filter under test.
	time.Sleep(1100 * time.Millisecond)
	if err := db.EditAnnotation(id, store.AnnotationEdit{Title: strptr("Renamed")}, time.Now()); err != nil {
		t.Fatalf("EditAnnotation: %v", err)
	}

	moved := decodeJSON[documentsResponse](t, get(t, server, "/api/documents?since="+watermark))
	if len(moved.Documents) != 1 {
		t.Fatalf("documents = %d, want 1 after an annotation changed", len(moved.Documents))
	}
}

func TestAPIDocumentsRejectsBadSince(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	seedAnnotation(t, db, "A harvested passage")

	response := get(t, server, "/api/documents?since=yesterday")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	failure := decodeJSON[map[string]string](t, response)
	if failure["error"] == "" {
		t.Error("a 400 carried no error message")
	}
}

func TestAPIDocumentDetailCarriesAnnotations(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "A harvested passage")

	response := get(t, server, "/api/documents/1")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	document := decodeJSON[apiDocument](t, response)

	if len(document.Annotations) != 1 {
		t.Fatalf("annotations = %d, want 1", len(document.Annotations))
	}
	annotation := document.Annotations[0]
	if annotation.ID != id {
		t.Errorf("annotation id = %d, want %d", annotation.ID, id)
	}
	if annotation.Text != "A harvested passage" {
		t.Errorf("text = %q", annotation.Text)
	}
	if annotation.OriginalText != "A harvested passage" {
		t.Errorf("original_text = %q", annotation.OriginalText)
	}
	if annotation.Edited {
		t.Error("an untouched annotation reports itself edited")
	}
	if annotation.Origin != store.OriginManual {
		t.Errorf("origin = %q, want %q", annotation.Origin, store.OriginManual)
	}
	if annotation.Source != "wallabag" {
		t.Errorf("source = %q, want wallabag", annotation.Source)
	}
}

func TestAPIDocumentNotFound(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	if code := get(t, server, "/api/documents/999").Code; code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// TestAPIEditAnnotationTitle is the everyday write: an org heading renamed on
// the Emacs side and pushed back.
func TestAPIEditAnnotationTitle(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "A harvested passage")

	response := patch(t, server, apiAnnotationPath(id), `{"title":"A better name"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}

	// The response is the row as it now stands, so one PATCH is a complete
	// round trip for a consumer keeping its own copy.
	returned := decodeJSON[apiAnnotation](t, response)
	if returned.Title != "A better name" {
		t.Errorf("returned title = %q", returned.Title)
	}

	stored, err := db.ElementByID(id)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if stored.Title != "A better name" {
		t.Errorf("stored title = %q", stored.Title)
	}
}

// TestAPIEditAnnotationTextLeavesTheAnchorAlone is the whole reason
// edited_quote exists. A correction from outside must change what is
// displayed and nothing else: the verbatim quote still drives anchoring and
// the wallabag write-back, and rewriting it here would silently detach the
// highlight on the next open.
func TestAPIEditAnnotationTextLeavesTheAnchorAlone(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "The formula is E = mc2 approximately")

	response := patch(t, server, apiAnnotationPath(id),
		`{"text":"The formula is E = mc² exactly"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}

	returned := decodeJSON[apiAnnotation](t, response)
	if returned.Text != "The formula is E = mc² exactly" {
		t.Errorf("text = %q, want the correction", returned.Text)
	}
	if returned.OriginalText != "The formula is E = mc2 approximately" {
		t.Errorf("original_text = %q, want the text as it arrived", returned.OriginalText)
	}
	if !returned.Edited {
		t.Error("edited = false after a correction")
	}

	stored, err := db.ElementByID(id)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if stored.Quote != "The formula is E = mc2 approximately" {
		t.Errorf("the API rewrote the verbatim quote to %q; anchoring and the "+
			"wallabag write-back both read that field", stored.Quote)
	}
	if stored.DisplayQuote() != "The formula is E = mc² exactly" {
		t.Errorf("DisplayQuote() = %q, want the correction", stored.DisplayQuote())
	}
}

// TestAPIEditAnnotationClearsOverride: sending an empty string is how a
// consumer reverts to the original, rather than there being a separate
// operation for it.
func TestAPIEditAnnotationClearsOverride(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "The original wording")

	patch(t, server, apiAnnotationPath(id), `{"text":"A correction"}`)
	response := patch(t, server, apiAnnotationPath(id), `{"text":""}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}

	returned := decodeJSON[apiAnnotation](t, response)
	if returned.Edited {
		t.Error("edited = true after clearing the override")
	}
	if returned.Text != "The original wording" {
		t.Errorf("text = %q, want the original back", returned.Text)
	}
}

// TestAPIEditAnnotationLeavesUnmentionedFieldsAlone is why AnnotationEdit is
// built from pointers. A consumer that only knows about titles must not blank
// the notes and chapters of everything it touches.
func TestAPIEditAnnotationLeavesUnmentionedFieldsAlone(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "A harvested passage")

	if err := db.UpdateAnnotation(id, "A harvested passage", "A standing note",
		"Chapter Two", time.Now()); err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}

	response := patch(t, server, apiAnnotationPath(id), `{"title":"Only the title"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}

	returned := decodeJSON[apiAnnotation](t, response)
	if returned.Note != "A standing note" {
		t.Errorf("note = %q, want it untouched", returned.Note)
	}
	if returned.Chapter != "Chapter Two" {
		t.Errorf("chapter = %q, want it untouched", returned.Chapter)
	}
}

// TestAPIEditAnnotationRejectsQuote: the verbatim text is not writable, and a
// consumer that tries must be told so rather than left believing it worked.
func TestAPIEditAnnotationRejectsQuote(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "A harvested passage")

	response := patch(t, server, apiAnnotationPath(id), `{"quote":"Rewritten"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}

	stored, err := db.ElementByID(id)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if stored.Quote != "A harvested passage" {
		t.Errorf("quote = %q, want it untouched", stored.Quote)
	}
}

// TestAPIEditAnnotationRejectsEmptyingBoth mirrors the HTML editor's own
// guard: an annotation with neither a passage nor a note is not an annotation.
func TestAPIEditAnnotationRejectsEmptyingBoth(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "A harvested passage")

	// The quote itself cannot be blanked through this API, so emptying
	// everything means overriding the text with nothing while there is no
	// note — which still leaves the original showing, and so is allowed.
	// Blanking a note on an annotation that never had a passage is the case
	// the guard is really for.
	if err := db.UpdateAnnotation(id, "", "Only a note", "", time.Now()); err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}

	response := patch(t, server, apiAnnotationPath(id), `{"note":""}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
}

// TestAPIAnnotationRejectsRoot: /api/annotations addresses passages, and a
// document's root topic is not one of them.
func TestAPIAnnotationRejectsRoot(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	root, err := db.RootElement(1)
	if err != nil {
		t.Fatalf("RootElement: %v", err)
	}

	if code := get(t, server, apiAnnotationPath(root.ID)).Code; code != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404", code)
	}
	if code := patch(t, server, apiAnnotationPath(root.ID), `{"title":"x"}`).Code; code != http.StatusNotFound {
		t.Errorf("PATCH status = %d, want 404", code)
	}
}

func TestAPIAnnotationNotFound(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	if code := get(t, server, "/api/annotations/999").Code; code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestAPIEditAnnotationRejectsBadJSON(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "A harvested passage")

	if code := patch(t, server, apiAnnotationPath(id), `{"title":`).Code; code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// TestAPIAnnotationHTMLIsSanitised: the API is an exit from this program, and
// every exit goes through the sanitiser. A consumer rendering the markup must
// not be the first thing to meet a script tag.
func TestAPIAnnotationHTMLIsSanitised(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	id, err := db.CreateExtract(store.NewExtract{
		ParentID:    1,
		DocumentID:  1,
		Kind:        store.KindTopic,
		Title:       "Hostile",
		ContentHTML: `<p>Harmless<script>alert(1)</script></p>`,
		Quote:       "Harmless",
	}, time.Now())
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	annotation := decodeJSON[apiAnnotation](t, get(t, server, apiAnnotationPath(id)))
	if strings.Contains(annotation.HTML, "<script") {
		t.Errorf("html carried a script tag: %q", annotation.HTML)
	}
	if !strings.Contains(annotation.HTML, "Harmless") {
		t.Errorf("sanitising also removed the text: %q", annotation.HTML)
	}
}

// TestAPIAnnotationHTMLIsAFragment: a consumer asked for a passage and must
// get one, not a whole document — the sanitiser's parse/render round trip
// supplies html/head/body that nothing inside this program notices.
func TestAPIAnnotationHTMLIsAFragment(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "A harvested passage")

	annotation := decodeJSON[apiAnnotation](t, get(t, server, apiAnnotationPath(id)))
	for _, tag := range []string{"<html", "<head", "<body"} {
		if strings.Contains(annotation.HTML, tag) {
			t.Errorf("html carried %s: %q", tag, annotation.HTML)
		}
	}
	if !strings.Contains(annotation.HTML, "A harvested passage") {
		t.Errorf("html lost the passage: %q", annotation.HTML)
	}
}

// TestAPIAnnotationHTMLFollowsTheCorrection: text and html must not disagree,
// or a consumer rendering the markup shows text the reader already fixed.
func TestAPIAnnotationHTMLFollowsTheCorrection(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "Mangled text")

	patch(t, server, apiAnnotationPath(id), `{"text":"Corrected text"}`)

	annotation := decodeJSON[apiAnnotation](t, get(t, server, apiAnnotationPath(id)))
	if !strings.Contains(annotation.HTML, "Corrected text") {
		t.Errorf("html does not carry the correction: %q", annotation.HTML)
	}
	if strings.Contains(annotation.HTML, "Mangled text") {
		t.Errorf("html still carries the uncorrected text: %q", annotation.HTML)
	}
}

// TestAPIDoesNotOfferGrading guards the boundary this API was drawn at:
// scheduling runs through ir.Next and the HTML app, and a second surface over
// it is exactly the drift the "one caller, one implementation" rule in
// queue.go exists to prevent.
func TestAPIDoesNotOfferGrading(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "A harvested passage")

	for _, path := range []string{
		"/api/elements/" + strconv.FormatInt(id, 10) + "/grade",
		"/api/annotations/" + strconv.FormatInt(id, 10) + "/grade",
	} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, recorder.Code)
		}
	}
}

// TestCorrectedPassageIsWhatTheReaderReads: a correction made through the API
// has to reach the reader's own pages, or fixing mangled text has fixed
// nothing.
func TestCorrectedPassageIsWhatTheReaderReads(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "Mangled ligature ﬁ text")

	patch(t, server, apiAnnotationPath(id), `{"text":"Corrected fi text"}`)

	reader := get(t, server, "/read/"+strconv.FormatInt(id, 10)).Body.String()
	if !strings.Contains(reader, "Corrected fi text") {
		t.Errorf("the reader does not show the correction:\n%s", reader)
	}

	extracts := get(t, server, "/extracts").Body.String()
	if !strings.Contains(extracts, "Corrected fi text") {
		t.Error("the extracts browser does not show the correction")
	}
}

// TestHTMLEditorSupersedesTheOverride: the passage editor in the reader
// rewrites the original outright, so any override standing in front of it must
// go. Leaving one would mean correcting the text there and seeing no change.
func TestHTMLEditorSupersedesTheOverride(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	id := seedAnnotation(t, db, "The original")

	patch(t, server, apiAnnotationPath(id), `{"text":"An API correction"}`)

	form := url.Values{}
	form.Set("quote", "An editor correction")
	form.Set("note", "")
	form.Set("chapter", "")
	if code := post(t, server, "/elements/"+strconv.FormatInt(id, 10)+"/annotation", form).Code; code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", code)
	}

	annotation := decodeJSON[apiAnnotation](t, get(t, server, apiAnnotationPath(id)))
	if annotation.Edited {
		t.Error("the override survived a direct edit of the passage")
	}
	if annotation.Text != "An editor correction" {
		t.Errorf("text = %q, want the editor's version", annotation.Text)
	}
	if annotation.OriginalText != "An editor correction" {
		t.Errorf("original_text = %q, want the editor's version", annotation.OriginalText)
	}
}

func apiAnnotationPath(id int64) string {
	return "/api/annotations/" + strconv.FormatInt(id, 10)
}

func strptr(value string) *string { return &value }
