package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/store"
)

// fakeSource stands in for wallabag. Content records whether the lazy body
// fetch actually happened.
type fakeSource struct {
	body         string
	contentCalls int
}

func (f *fakeSource) Name() string { return "wallabag" }

func (f *fakeSource) Fetch(context.Context, time.Time) ([]source.Document, error) {
	return nil, nil
}

func (f *fakeSource) Content(context.Context, string) (string, error) {
	f.contentCalls++
	return f.body, nil
}

const articleBody = `<p>The quick brown fox.</p>` +
	`<p>It jumps over the <a href="https://example.com/dog">lazy dog</a> daily.</p>` +
	`<p>A third paragraph exists.</p>`

// newTestServer builds a server over a fresh database holding one article.
func newTestServer(t *testing.T, withContent bool) (*Server, *store.Store, *fakeSource) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	document := source.Document{
		ExternalID: "1",
		Title:      "A test article",
		URL:        "https://example.com/article",
		Author:     "Someone",
		UpdatedAt:  time.Now(),
	}
	if withContent {
		document.ContentHTML = articleBody
	}
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, time.Now()); err != nil {
		t.Fatalf("seed document: %v", err)
	}

	provider := &fakeSource{body: articleBody}
	server, err := New(Options{
		Store:      db,
		Sources:    map[string]source.Source{"wallabag": provider},
		DailyLimit: 60,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server, db, provider
}

func get(t *testing.T, server *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func post(t *testing.T, server *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestQueueListsTheArticle(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	response := get(t, server, "/")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	body := response.Body.String()
	if !strings.Contains(body, "A test article") {
		t.Error("queue does not list the article")
	}
	if !strings.Contains(body, "1 due today") {
		t.Errorf("queue does not report the article as due:\n%s", body)
	}
}

// TestReaderFetchesBodyLazily covers the design that makes syncing cheap:
// listings store metadata only, and the article body arrives on first open.
func TestReaderFetchesBodyLazily(t *testing.T) {
	server, db, provider := newTestServer(t, false)

	document, err := db.DocumentByID(1)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.HasContent {
		t.Fatal("test premise is wrong: the document should start without a body")
	}

	if response := get(t, server, "/read/1"); response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if provider.contentCalls != 1 {
		t.Errorf("source was asked for content %d times, want 1", provider.contentCalls)
	}

	// The body must be cached, or every page view would re-fetch the article.
	if response := get(t, server, "/read/1"); response.Code != http.StatusOK {
		t.Fatalf("second read: status = %d", response.Code)
	}
	if provider.contentCalls != 1 {
		t.Errorf("source was asked again after caching (%d calls)", provider.contentCalls)
	}
}

func TestReaderEmitsBlockIndices(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	body := get(t, server, "/read/1").Body.String()

	for _, want := range []string{`data-b="0"`, `data-b="1"`, `data-b="2"`} {
		if !strings.Contains(body, want) {
			t.Errorf("reader output is missing %s — the browser cannot address blocks without it", want)
		}
	}
}

// TestExtractRoundTrip is the core workflow: highlight a passage, get an
// extract that appears marked in the parent and enters the queue on its own.
func TestExtractRoundTrip(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	// Warm the reader once so the article is parsed the same way the browser
	// would have seen it.
	get(t, server, "/read/1")

	response := post(t, server, "/elements/1/extract", url.Values{
		"start_block":  {"0"},
		"start_offset": {"4"},
		"end_block":    {"0"},
		"end_offset":   {"15"},
		"quote":        {"quick brown"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}

	// The returned fragment re-renders the article with the new highlight, so
	// the reader's scroll position survives.
	fragment := response.Body.String()
	if !strings.Contains(fragment, `<mark class="extract"`) {
		t.Errorf("returned fragment has no highlight:\n%s", fragment)
	}
	if !strings.Contains(fragment, "quick brown</mark>") {
		t.Errorf("the highlight does not cover the extracted text:\n%s", fragment)
	}

	children, err := db.ChildrenOf(1)
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("got %d extracts, want 1", len(children))
	}

	extract := children[0]
	if extract.Quote != "quick brown" {
		t.Errorf("stored quote = %q, want %q", extract.Quote, "quick brown")
	}
	if extract.ContentHTML != "<p>quick brown</p>" {
		t.Errorf("stored HTML = %q", extract.ContentHTML)
	}
	if !extract.HasRange {
		t.Error("the extract did not record its position in the parent")
	}

	// A new extract is due immediately, so it can be refined in this session.
	queue, err := db.Queue(time.Now(), 10)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(queue) != 2 {
		t.Errorf("queue holds %d elements, want the article plus its extract", len(queue))
	}
}

func TestExtractPreservesLinks(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	// "It jumps over the lazy dog daily." — select "lazy dog", which is a link.
	response := post(t, server, "/elements/1/extract", url.Values{
		"start_block":  {"1"},
		"start_offset": {"18"},
		"end_block":    {"1"},
		"end_offset":   {"26"},
		"quote":        {"lazy dog"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	children, err := db.ChildrenOf(1)
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if !strings.Contains(children[0].ContentHTML, `href="https://example.com/dog"`) {
		t.Errorf("the extract lost its link: %q", children[0].ContentHTML)
	}
}

// TestExtractRejectsStaleSelection is the guard against silent corruption: if
// the browser's idea of the text disagrees with the server's, the offsets are
// stale and saving would attach the extract to the wrong passage.
func TestExtractRejectsStaleSelection(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	response := post(t, server, "/elements/1/extract", url.Values{
		"start_block":  {"0"},
		"start_offset": {"4"},
		"end_block":    {"0"},
		"end_offset":   {"15"},
		// Offsets that no longer correspond to this text.
		"quote": {"something else entirely"},
	})

	if response.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", response.Code)
	}

	children, _ := db.ChildrenOf(1)
	if len(children) != 0 {
		t.Errorf("a mismatched selection was saved anyway (%d extracts)", len(children))
	}
}

func TestExtractRejectsOutOfRangeSelection(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	response := post(t, server, "/elements/1/extract", url.Values{
		"start_block":  {"99"},
		"start_offset": {"0"},
		"end_block":    {"99"},
		"end_offset":   {"5"},
		"quote":        {"anything"},
	})
	if response.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", response.Code)
	}
}

// TestClozePromotesExtractToItem covers the second stage: a refined extract
// gains a deletion and becomes a card destined for Anki.
func TestClozePromotesExtractToItem(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/extract", url.Values{
		"start_block":  {"0"},
		"start_offset": {"4"},
		"end_block":    {"0"},
		"end_offset":   {"19"},
		"quote":        {"quick brown fox"},
	})

	children, _ := db.ChildrenOf(1)
	if len(children) != 1 {
		t.Fatalf("expected one extract, got %d", len(children))
	}
	extractID := children[0].ID

	if children[0].Kind != store.KindTopic {
		t.Errorf("a fresh extract is kind %q, want %q", children[0].Kind, store.KindTopic)
	}

	// Delete "brown" from "quick brown fox", in the extract's own coordinates.
	response := post(t, server, "/elements/"+itoa(extractID)+"/cloze", url.Values{
		"start_block":  {"0"},
		"start_offset": {"6"},
		"end_block":    {"0"},
		"end_offset":   {"11"},
		"quote":        {"brown"},
	})
	if response.Code != http.StatusSeeOther && response.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	updated, err := db.ElementByID(extractID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if updated.Kind != store.KindItem {
		t.Errorf("kind = %q, want %q once it has a deletion", updated.Kind, store.KindItem)
	}

	clozes, err := db.ClozesOf(extractID)
	if err != nil {
		t.Fatalf("ClozesOf: %v", err)
	}
	if len(clozes) != 1 || clozes[0].Ordinal != 1 {
		t.Fatalf("got %+v, want one deletion numbered c1", clozes)
	}

	// The reader shows the card as Anki will receive it.
	body := get(t, server, "/read/"+itoa(extractID)).Body.String()
	if !strings.Contains(body, "quick {{c1::brown}} fox") {
		t.Errorf("reader does not preview the cloze:\n%s", body)
	}
}

func TestClozeRejectedOnWholeArticle(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	response := post(t, server, "/elements/1/cloze", url.Values{
		"start_block":  {"0"},
		"start_offset": {"0"},
		"end_block":    {"0"},
		"end_offset":   {"5"},
		"quote":        {"The q"},
	})
	if response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — clozes belong on extracts", response.Code)
	}
}

// TestClozeOnMultiBlockExtract is the case where block coordinates and stored
// offsets diverge. The extract's text is flat, with separators between what
// were separate paragraphs; taking the browser's block offset at face value
// would delete a different span entirely and give no sign of it.
func TestClozeOnMultiBlockExtract(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	// Extract across two paragraphs: "quick brown fox." + "It jumps".
	if response := post(t, server, "/elements/1/extract", url.Values{
		"start_block":  {"0"},
		"start_offset": {"4"},
		"end_block":    {"1"},
		"end_offset":   {"8"},
		"quote":        {"quick brown fox.\n\nIt jumps"},
	}); response.Code != http.StatusOK {
		t.Fatalf("extract: status = %d: %s", response.Code, response.Body.String())
	}

	children, _ := db.ChildrenOf(1)
	extract := children[0]
	if extract.Quote != "quick brown fox.\n\nIt jumps" {
		t.Fatalf("stored quote = %q", extract.Quote)
	}

	// "jumps" sits at offsets 3..8 of the extract's *second* block, which is
	// offsets 21..26 of its flat text.
	if response := post(t, server, "/elements/"+itoa(extract.ID)+"/cloze", url.Values{
		"start_block":  {"1"},
		"start_offset": {"3"},
		"end_block":    {"1"},
		"end_offset":   {"8"},
		"quote":        {"jumps"},
	}); response.Code != http.StatusSeeOther && response.Code != http.StatusNoContent {
		t.Fatalf("cloze: status = %d: %s", response.Code, response.Body.String())
	}

	clozes, err := db.ClozesOf(extract.ID)
	if err != nil {
		t.Fatalf("ClozesOf: %v", err)
	}
	if len(clozes) != 1 {
		t.Fatalf("got %d deletions, want 1", len(clozes))
	}

	deleted := extract.Quote[clozes[0].Start:clozes[0].End]
	if deleted != "jumps" {
		t.Errorf("the deletion covers %q, want %q — block offsets were not converted",
			deleted, "jumps")
	}

	rendered, err := ir.RenderCloze(extract.Quote, clozes)
	if err != nil {
		t.Fatalf("RenderCloze: %v", err)
	}
	if !strings.Contains(rendered, "It {{c1::jumps}}") {
		t.Errorf("card renders as %q", rendered)
	}
}

// TestClozeRejectsStaleSelection mirrors the same guard on extracts: if the
// browser's text disagrees with what is stored, the offsets are not usable.
func TestClozeRejectsStaleSelection(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/extract", url.Values{
		"start_block":  {"0"},
		"start_offset": {"4"},
		"end_block":    {"0"},
		"end_offset":   {"19"},
		"quote":        {"quick brown fox"},
	})
	children, _ := db.ChildrenOf(1)

	response := post(t, server, "/elements/"+itoa(children[0].ID)+"/cloze", url.Values{
		"start_block":  {"0"},
		"start_offset": {"6"},
		"end_block":    {"0"},
		"end_offset":   {"11"},
		"quote":        {"something else"},
	})
	if response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.Code)
	}

	clozes, _ := db.ClozesOf(children[0].ID)
	if len(clozes) != 0 {
		t.Errorf("a mismatched deletion was stored anyway")
	}
}

func TestGradeReschedulesAndMovesOn(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	before, _ := db.ElementByID(1)
	if before.Schedule.Reps != 0 {
		t.Fatalf("test premise is wrong: reps = %d", before.Schedule.Reps)
	}

	request := httptest.NewRequest(http.MethodPost, "/elements/1/grade",
		strings.NewReader("grade=next"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	// htmx must be told to navigate; a 303 would nest a whole page inside the
	// element the button targeted.
	if got := recorder.Header().Get("HX-Redirect"); got != "/next" {
		t.Errorf("HX-Redirect = %q, want %q", got, "/next")
	}

	after, _ := db.ElementByID(1)
	if after.Schedule.Reps != 1 {
		t.Errorf("reps = %d, want 1", after.Schedule.Reps)
	}
	if after.Schedule.State != "reading" {
		t.Errorf("state = %q, want %q", after.Schedule.State, "reading")
	}
	if after.Schedule.DueOn.IsZero() {
		t.Error("grading did not set a due date")
	}
	// Having just been read, it must not still be due today.
	if !after.Schedule.DueOn.After(time.Now()) {
		t.Errorf("due %v, want a future date", after.Schedule.DueOn)
	}
}

func TestGradeDismissRemovesFromQueue(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/grade", url.Values{"grade": {"dismiss"}})

	queue, err := db.Queue(time.Now(), 10)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(queue) != 0 {
		t.Errorf("dismissed material is still queued: %d elements", len(queue))
	}
}

func TestGradeRejectsUnknownValue(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	response := post(t, server, "/elements/1/grade", url.Values{"grade": {"excellent"}})
	if response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.Code)
	}
}

func TestPriorityUpdate(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if response := post(t, server, "/elements/1/priority", url.Values{
		"priority": {"0.1"},
	}); response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}

	element, _ := db.ElementByID(1)
	if element.Schedule.Priority != 0.1 {
		t.Errorf("priority = %v, want 0.1", element.Schedule.Priority)
	}

	// Out-of-range values must be rejected, not clamped silently.
	for _, bad := range []string{"-0.5", "1.5", "high"} {
		if response := post(t, server, "/elements/1/priority", url.Values{
			"priority": {bad},
		}); response.Code != http.StatusBadRequest {
			t.Errorf("priority %q: status = %d, want 400", bad, response.Code)
		}
	}
}

func TestProgressRecordsReadPosition(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if response := post(t, server, "/elements/1/progress", url.Values{
		"block": {"2"},
	}); response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}

	element, _ := db.ElementByID(1)
	if element.ReadBlock != 2 {
		t.Errorf("read_block = %d, want 2", element.ReadBlock)
	}

	// The reader passes the position to the browser so it can resume there.
	body := get(t, server, "/read/1").Body.String()
	if !strings.Contains(body, `data-read-block="2"`) {
		t.Error("the reader does not carry the resume position")
	}
}

func TestNextRedirectsToMostImportant(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	// Add a second, more important article.
	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "2",
		Title:      "More important",
		UpdatedAt:  time.Now(),
	}}, time.Now()); err != nil {
		t.Fatalf("seed second document: %v", err)
	}
	if err := db.SetPriority(2, 0.1, time.Now()); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	response := get(t, server, "/next")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", response.Code)
	}
	if got := response.Header().Get("Location"); got != "/read/2" {
		t.Errorf("Location = %q, want /read/2 (the higher-priority element)", got)
	}
}

func TestNextFallsBackToQueueWhenEmpty(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	post(t, server, "/elements/1/grade", url.Values{"grade": {"done"}})

	response := get(t, server, "/next")
	if got := response.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want / when nothing is due", got)
	}
}

func TestMissingElementIsNotFound(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	if response := get(t, server, "/read/999"); response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.Code)
	}
}

// TestSanitizerStripsScripts guards the reader against a hostile article. The
// deployment being private makes this more important, not less: a script here
// would run with full access to increader's own origin.
func TestSanitizerStripsScripts(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	hostile := `<p>Before.</p>` +
		`<script>alert(1)</script>` +
		`<p onclick="steal()">Clickable.</p>` +
		`<a href="javascript:alert(1)">bad link</a>` +
		`<iframe src="https://evil.example"></iframe>`
	if err := db.SetDocumentContent(1, hostile); err != nil {
		t.Fatalf("SetDocumentContent: %v", err)
	}

	body := get(t, server, "/read/1").Body.String()

	// Asserted against the hostile payload rather than against "<script",
	// because the page's own layout legitimately loads htmx and app.js.
	for _, forbidden := range []string{
		"alert(1)", "steal()", "onclick", "javascript:", "<iframe", "evil.example",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("rendered page contains %q:\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, "Before.") {
		t.Error("sanitising removed the legitimate text too")
	}
	if !strings.Contains(body, "Clickable.") {
		t.Error("sanitising dropped an element instead of just its handler")
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	for _, path := range []string{"/static/htmx.min.js", "/static/app.js", "/static/app.css"} {
		response := get(t, server, path)
		if response.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, response.Code)
		}
		if response.Body.Len() == 0 {
			t.Errorf("%s: served an empty body", path)
		}
	}
}

func TestHealthz(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	if response := get(t, server, "/healthz"); response.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", response.Code)
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// fakeEnricher is a source that can also supply highlights, the way wallabag
// does when a single entry is fetched.
type fakeEnricher struct {
	fakeSource
	highlights []source.Highlight
}

func (f *fakeEnricher) FullDocument(context.Context, string) (source.Document, error) {
	f.contentCalls++
	return source.Document{ContentHTML: f.body, Highlights: f.highlights}, nil
}

// newEnrichedServer builds a server whose source carries pre-existing highlights.
func newEnrichedServer(t *testing.T, highlights []source.Highlight) (*Server, *store.Store, *fakeEnricher) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1",
		Title:      "A test article",
		UpdatedAt:  time.Now(),
	}}, time.Now()); err != nil {
		t.Fatalf("seed document: %v", err)
	}

	provider := &fakeEnricher{
		fakeSource: fakeSource{body: articleBody},
		highlights: highlights,
	}
	server, err := New(Options{
		Store:      db,
		Sources:    map[string]source.Source{"wallabag": provider},
		DailyLimit: 60,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server, db, provider
}

// TestHighlightsImportOnFirstOpen covers the whole annotation-import path:
// highlights made in wallabag's own reader become extracts here, positioned in
// the article so they show as already harvested.
func TestHighlightsImportOnFirstOpen(t *testing.T) {
	server, db, _ := newEnrichedServer(t, []source.Highlight{
		{ExternalID: "97418", Quote: "quick brown"},
		// Whitespace differs from this copy of the article, as it would when
		// the other system wrapped its text differently.
		{ExternalID: "97419", Quote: "  It jumps   over the  "},
		// A passage that no longer appears, because the article was reworded.
		{ExternalID: "97420", Quote: "a sentence that is not in this article"},
	})

	if response := get(t, server, "/read/1"); response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	extracts, err := db.ChildrenOf(1)
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if len(extracts) != 3 {
		t.Fatalf("got %d extracts, want 3", len(extracts))
	}

	byRef := map[string]store.Element{}
	for _, extract := range extracts {
		byRef[extract.ExternalRef] = extract
		if extract.Origin != store.OriginImport {
			t.Errorf("extract %s has origin %q, want %q",
				extract.ExternalRef, extract.Origin, store.OriginImport)
		}
	}

	// A located highlight gets a position, so it renders as a highlight in the
	// parent article.
	located := byRef["97418"]
	if !located.HasRange {
		t.Error("an exactly matching highlight was not located in the article")
	}
	if located.Quote != "quick brown" {
		t.Errorf("quote = %q, want %q", located.Quote, "quick brown")
	}

	// Whitespace differences must not prevent a match.
	fuzzy := byRef["97419"]
	if !fuzzy.HasRange {
		t.Error("a highlight differing only in whitespace was not located")
	}

	// A highlight whose text is gone still becomes an extract — the passage
	// mattered once — just without a position.
	orphan := byRef["97420"]
	if orphan.HasRange {
		t.Error("a highlight that does not appear in the article was given a position")
	}
	if orphan.Quote != "a sentence that is not in this article" {
		t.Errorf("orphan quote = %q", orphan.Quote)
	}

	// Located highlights show as marks when the article is rendered.
	body := get(t, server, "/read/1").Body.String()
	if !strings.Contains(body, `<mark class="extract"`) {
		t.Error("imported highlights are not marked in the article")
	}
}

// TestHighlightsAreNotImportedTwice covers the deduplication that makes a
// re-fetch safe: the unique index on the provider's annotation id is what does
// the work, and hitting it means "already have this", not a failure.
func TestHighlightsAreNotImportedTwice(t *testing.T) {
	highlights := []source.Highlight{
		{ExternalID: "97418", Quote: "quick brown"},
		{ExternalID: "97419", Quote: "lazy dog"},
	}
	server, db, provider := newEnrichedServer(t, highlights)

	get(t, server, "/read/1")

	// Simulate the article being re-fetched upstream, which is the only way
	// the import runs a second time.
	if _, err := db.DB().Exec(`UPDATE documents SET has_content = 0 WHERE id = 1`); err != nil {
		t.Fatalf("reset content flag: %v", err)
	}

	if response := get(t, server, "/read/1"); response.Code != http.StatusOK {
		t.Fatalf("second open: status = %d", response.Code)
	}
	if provider.contentCalls != 2 {
		t.Fatalf("test premise is wrong: the article was fetched %d times", provider.contentCalls)
	}

	extracts, err := db.ChildrenOf(1)
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if len(extracts) != 2 {
		t.Errorf("got %d extracts after a re-fetch, want 2 — highlights were duplicated", len(extracts))
	}
}

// TestManualExtractsAreNotDeduplicated guards the partial index: extracts made
// here carry no provider id, and many of them must be able to coexist.
func TestManualExtractsAreNotDeduplicated(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	selections := []url.Values{
		{"start_block": {"0"}, "start_offset": {"4"}, "end_block": {"0"},
			"end_offset": {"9"}, "quote": {"quick"}},
		{"start_block": {"0"}, "start_offset": {"10"}, "end_block": {"0"},
			"end_offset": {"15"}, "quote": {"brown"}},
		{"start_block": {"2"}, "start_offset": {"0"}, "end_block": {"2"},
			"end_offset": {"7"}, "quote": {"A third"}},
	}
	for i, selection := range selections {
		if response := post(t, server, "/elements/1/extract", selection); response.Code != http.StatusOK {
			t.Fatalf("extract %d: status = %d: %s", i, response.Code, response.Body.String())
		}
	}

	extracts, err := db.ChildrenOf(1)
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if len(extracts) != 3 {
		t.Errorf("got %d extracts, want 3", len(extracts))
	}
}

func TestLibraryListsAndSearches(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "2", Title: "On the difficulty of reading", Author: "A. Writer",
			UpdatedAt: time.Now()},
		{ExternalID: "3", Title: "Something unrelated", UpdatedAt: time.Now()},
	}, time.Now()); err != nil {
		t.Fatalf("seed documents: %v", err)
	}

	all := get(t, server, "/library").Body.String()
	for _, want := range []string{"A test article", "On the difficulty of reading", "Something unrelated"} {
		if !strings.Contains(all, want) {
			t.Errorf("library does not list %q", want)
		}
	}

	// Title search.
	filtered := get(t, server, "/library?q=difficulty").Body.String()
	if !strings.Contains(filtered, "On the difficulty of reading") {
		t.Error("search did not find a matching title")
	}
	if strings.Contains(filtered, "Something unrelated") {
		t.Error("search returned a document that does not match")
	}

	// Author search hits the same row.
	byAuthor := get(t, server, "/library?q="+url.QueryEscape("A. Writer")).Body.String()
	if !strings.Contains(byAuthor, "On the difficulty of reading") {
		t.Error("search by author did not find the document")
	}

	// Library entries link into the reader, not to the document id — the two
	// are only equal by coincidence in small databases.
	if !strings.Contains(all, `href="/read/`) {
		t.Error("library entries do not link into the reader")
	}
}
