package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
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
	return newTestServerWithDelay(t, 0, withContent)
}

// newTestServerWithDelay builds a server whose new extracts are scheduled
// delayDays ahead. Most tests want 0 so an extract is immediately assertable.
func newTestServerWithDelay(t *testing.T, delayDays int, withContent ...bool) (*Server, *store.Store, *fakeSource) {
	t.Helper()
	hasContent := true
	if len(withContent) > 0 {
		hasContent = withContent[0]
	}

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
	if hasContent {
		document.ContentHTML = articleBody
	}
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, time.Now()); err != nil {
		t.Fatalf("seed document: %v", err)
	}

	provider := &fakeSource{body: articleBody}
	server, err := New(Options{
		Store:        db,
		Sources:      map[string]source.Source{"wallabag": provider},
		DailyLimit:   60,
		ExtractDelay: delayDays,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
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

// del issues a DELETE request. path may carry a query string directly, which is
// how the templates pass swap_only — htmx sends DELETE parameters as a query
// string by default, so that is also the realistic shape of the request.
func del(t *testing.T, server *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, path, nil))
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

// TestReaderLoadsMathJax: self-hosted, like every other script this app
// loads — see the comment beside the script tag in reader.html.
func TestReaderLoadsMathJax(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	body := get(t, server, "/read/1").Body.String()
	if !strings.Contains(body, `<script src="/static/mathjax-tex-svg.js" defer></script>`) {
		t.Error("reader page does not load MathJax")
	}
}

// TestMathJaxExtensionsAreVendoredToo guards a real bug: tex-svg.js is not
// fully self-contained on its own. It lazy-loads a handful of less common
// TeX packages (\enclose, \cancel, mhchem, ...) at runtime, resolved
// relative to wherever tex-svg.js was loaded from, and a browser refuses to
// execute a missing one — its 404 page comes back as text/html, which fails
// strict MIME-type checking. Vendoring only mathjax-tex-svg.js looks correct
// until an article happens to use one of those packages, and that equation
// silently fails to render instead. See static/README.md.
func TestMathJaxExtensionsAreVendoredToo(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	// \enclose is the one that actually broke in production; the rest are
	// checked too since any of them failing is the same bug.
	for _, extension := range []string{"enclose", "cancel", "mhchem", "physics", "color"} {
		path := "/static/input/tex/extensions/" + extension + ".js"
		response := get(t, server, path)
		if response.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, response.Code)
		}
		if ct := response.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Errorf("%s: Content-Type = %q, want a JavaScript type", path, ct)
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

// TestExtractAcrossMultibyteCharacter guards the seam between a browser's
// selection offsets (JavaScript string .length, counting UTF-16 code units —
// one per rune for anything in the Basic Multilingual Plane) and this
// package's own byte-indexed strings. A soft hyphen, curly quote or em dash
// is more than one byte but exactly one rune; a real article is full of
// them, and a request built the way a browser actually builds one — offsets
// counted in runes, past a multi-byte character — used to come back a 409
// because the server re-derived a different, byte-misaligned substring.
func TestExtractAcrossMultibyteCharacter(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if err := db.SetDocumentContent(1, `<p>Amer­ican eco­nomists study markets.</p>`); err != nil {
		t.Fatalf("SetDocumentContent: %v", err)
	}

	// A browser selecting "eco­nomists" (soft hyphen included) reports these
	// as rune offsets: "Amer­ican " is 10 runes, the word itself 11 more.
	response := post(t, server, "/elements/1/extract", url.Values{
		"start_block":  {"0"},
		"start_offset": {"10"},
		"end_block":    {"0"},
		"end_offset":   {"21"},
		"quote":        {"eco­nomists"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}

	children, err := db.ChildrenOf(1)
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("got %d extracts, want 1", len(children))
	}
	if got := children[0].Quote; got != "eco­nomists" {
		t.Errorf("stored quote = %q, want %q", got, "eco­nomists")
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

// TestDeleteClozeDemotesItemBackToTopic is the reverse of the promotion
// above: an item is defined by having at least one deletion, so removing its
// only one must hand that distinction back, not leave a "cloze" badge on
// something that no longer has one.
func TestDeleteClozeDemotesItemBackToTopic(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"4"},
		"end_block": {"0"}, "end_offset": {"19"}, "quote": {"quick brown fox"},
	})
	children, _ := db.ChildrenOf(1)
	extractID := children[0].ID

	post(t, server, "/elements/"+itoa(extractID)+"/cloze", url.Values{
		"start_block": {"0"}, "start_offset": {"6"},
		"end_block": {"0"}, "end_offset": {"11"}, "quote": {"brown"},
	})
	promoted, _ := db.ElementByID(extractID)
	if promoted.Kind != store.KindItem {
		t.Fatalf("setup: kind = %q, want %q before the delete under test", promoted.Kind, store.KindItem)
	}

	response := del(t, server, "/elements/"+itoa(extractID)+"/cloze/1")
	if response.Code != http.StatusSeeOther && response.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	demoted, err := db.ElementByID(extractID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if demoted.Kind != store.KindTopic {
		t.Errorf("kind = %q, want %q once its last deletion is gone", demoted.Kind, store.KindTopic)
	}
	clozes, _ := db.ClozesOf(extractID)
	if len(clozes) != 0 {
		t.Errorf("got %d clozes remaining, want 0", len(clozes))
	}
}

// TestDeleteClozeLeavesSiblingsAndKindAlone covers the more common case:
// removing one of several deletions must not touch the others, and must not
// demote an item that still has clozes left.
func TestDeleteClozeLeavesSiblingsAndKindAlone(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"4"},
		"end_block": {"0"}, "end_offset": {"19"}, "quote": {"quick brown fox"},
	})
	children, _ := db.ChildrenOf(1)
	extractID := children[0].ID

	post(t, server, "/elements/"+itoa(extractID)+"/cloze", url.Values{
		"start_block": {"0"}, "start_offset": {"0"},
		"end_block": {"0"}, "end_offset": {"5"}, "quote": {"quick"},
	})
	post(t, server, "/elements/"+itoa(extractID)+"/cloze", url.Values{
		"start_block": {"0"}, "start_offset": {"6"},
		"end_block": {"0"}, "end_offset": {"11"}, "quote": {"brown"},
	})

	if response := del(t, server, "/elements/"+itoa(extractID)+"/cloze/1"); response.Code != http.StatusSeeOther && response.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	still, err := db.ElementByID(extractID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if still.Kind != store.KindItem {
		t.Errorf("kind = %q, want %q — one deletion is still left", still.Kind, store.KindItem)
	}
	clozes, _ := db.ClozesOf(extractID)
	if len(clozes) != 1 || clozes[0].Ordinal != 2 {
		t.Errorf("clozes = %+v, want only c2 (\"brown\") surviving", clozes)
	}
}

func TestDeleteClozeMissing(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"4"},
		"end_block": {"0"}, "end_offset": {"19"}, "quote": {"quick brown fox"},
	})
	children, _ := db.ChildrenOf(1)
	extractID := children[0].ID

	response := del(t, server, "/elements/"+itoa(extractID)+"/cloze/1")
	if response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an extract with no such cloze", response.Code)
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
		strings.NewReader("grade=next&block=1"))
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

// TestBacklogPutsAnElementOff is the schedule panel's preset buttons: unlike
// the slider they replaced, a click sets the due date directly and
// immediately, no grade required.
func TestBacklogPutsAnElementOff(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	before, _ := db.ElementByID(1)

	// Behaves like a grade: it is a complete decision, so it redirects on to
	// the next item rather than staying on the page.
	response := post(t, server, "/elements/1/backlog", url.Values{"days": {"30"}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", response.Code)
	}
	if got := response.Header().Get("Location"); got != "/next" {
		t.Errorf("Location = %q, want /next (the default with no redirect field)", got)
	}

	element, _ := db.ElementByID(1)
	wantDue := ir.Day(time.Now()).AddDate(0, 0, 30)
	if !element.Schedule.DueOn.Equal(wantDue) {
		t.Errorf("due = %v, want %v", element.Schedule.DueOn, wantDue)
	}
	if element.Schedule.IntervalDays != 30 {
		t.Errorf("interval = %.1f, want 30", element.Schedule.IntervalDays)
	}
	if element.Schedule.State != before.Schedule.State {
		t.Errorf("state = %q, want unchanged at %q — backlogging is not grading it",
			element.Schedule.State, before.Schedule.State)
	}
}

// TestBacklogRedirectsToGivenTarget is what lets the extracts and library
// pages offer rescheduling without dropping the reader into the reading
// queue: their reschedule control sends its own current URL back as
// redirect, and this is what honours it.
func TestBacklogRedirectsToGivenTarget(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	response := post(t, server, "/elements/1/backlog", url.Values{
		"days": {"7"}, "redirect": {"/library?state=unread"},
	})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", response.Code)
	}
	if got := response.Header().Get("Location"); got != "/library?state=unread" {
		t.Errorf("Location = %q, want /library?state=unread", got)
	}
}

// TestBacklogRejectsUnsafeRedirectTargets: redirect is meant for this site's
// own pages only, never an absolute or protocol-relative URL some other
// origin supplied — those fall back to the ordinary default instead of being
// trusted as given.
func TestBacklogRejectsUnsafeRedirectTargets(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	for _, bad := range []string{"https://evil.example/", "//evil.example/", "not-a-path"} {
		response := post(t, server, "/elements/1/backlog", url.Values{
			"days": {"7"}, "redirect": {bad},
		})
		if got := response.Header().Get("Location"); got != "/next" {
			t.Errorf("redirect %q: Location = %q, want the /next fallback", bad, got)
		}
	}
}

// TestBacklogCapturesReadBlock: putting an element off usually happens
// mid-read, not only from the very top, so the same scroll position grading
// records is recorded here too.
func TestBacklogCapturesReadBlock(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/backlog", url.Values{"days": {"7"}, "block": {"3"}})

	element, _ := db.ElementByID(1)
	if element.ReadBlock != 3 {
		t.Errorf("ReadBlock = %d, want 3", element.ReadBlock)
	}
}

// TestBacklogRejectsInvalidDays guards against silently accepting nonsense —
// a negative day count has no sensible due date to produce. Zero is not in
// this list: it is the "today" preset, for undoing a backlog.
func TestBacklogRejectsInvalidDays(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	for _, bad := range []string{"-5", "high"} {
		if response := post(t, server, "/elements/1/backlog", url.Values{
			"days": {bad},
		}); response.Code != http.StatusBadRequest {
			t.Errorf("days %q: status = %d, want 400", bad, response.Code)
		}
	}
}

// TestBacklogZeroDaysUnschedulesToToday is the reschedule control's "today"
// option: after backlogging something out and changing your mind, days=0
// brings it right back into today's queue instead of leaving no way back
// except grading it.
func TestBacklogZeroDaysUnschedulesToToday(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/backlog", url.Values{"days": {"30"}})
	post(t, server, "/elements/1/backlog", url.Values{"days": {"0"}})

	element, err := db.ElementByID(1)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	today := time.Now().Format("2006-01-02")
	if element.Schedule.DueOn.Format("2006-01-02") != today {
		t.Errorf("due_on = %s, want today (%s)", element.Schedule.DueOn, today)
	}
}

// TestBacklogAppliesToArticlesAndExtractsAlike is what the preset buttons
// replaced the slider to get: a whole article and an extract are put off the
// same way, with no special-casing for whichever one is still sitting at its
// import default. The slider needed one, to keep a stray drag from silently
// postponing a due-today article — a deliberate button click has no such
// accidental resting position to protect against.
func TestBacklogAppliesToArticlesAndExtractsAlike(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	extractID, err := db.CreateExtract(store.NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "A passage.",
		ContentHTML: "<p>A passage.</p>", Priority: 0.6,
	}, time.Now())
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	for _, id := range []int64{1, extractID} { // article, then extract
		before, _ := db.ElementByID(id)

		response := post(t, server, fmt.Sprintf("/elements/%d/backlog", id), url.Values{"days": {"7"}})
		if response.Code != http.StatusSeeOther {
			t.Fatalf("element %d: status = %d, want 303", id, response.Code)
		}

		element, _ := db.ElementByID(id)
		wantDue := ir.Day(time.Now()).AddDate(0, 0, 7)
		if !element.Schedule.DueOn.Equal(wantDue) {
			t.Errorf("element %d: due = %v, want %v", id, element.Schedule.DueOn, wantDue)
		}
		if element.Schedule.State != before.Schedule.State {
			t.Errorf("element %d: state changed from %q to %q", id, before.Schedule.State, element.Schedule.State)
		}
	}
}

// TestBacklogUpdatesSchedulePreview: Sooner and Next grow from whatever
// interval a backlog button just set — the same rule Previews always
// follows, that a button can never promise something grading would not
// actually do. Checked by reading the page back afterwards, since backlog
// itself now redirects rather than returning a fragment to inspect.
func TestBacklogUpdatesSchedulePreview(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	post(t, server, "/elements/1/backlog", url.Values{"days": {"30"}})

	body := get(t, server, "/read/1").Body.String()
	if strings.Contains(body, "Next<small>1d</small>") {
		t.Errorf("schedule preview still shows the unbacklogged 1d default: %s", body)
	}
}

// TestReaderShowsBacklogButtons is a smoke test for the schedule panel
// itself: every preset should render with a fuzzed, non-empty label, not the
// removed priority slider.
func TestReaderShowsBacklogButtons(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	body := get(t, server, "/read/1").Body.String()
	if strings.Contains(body, `class="priority"`) {
		t.Error("the reader still renders the old priority slider")
	}
	for _, option := range ir.BacklogOptions(1) {
		if !strings.Contains(body, option.Label) {
			t.Errorf("reader body does not contain backlog label %q", option.Label)
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
	}}, 0, time.Now()); err != nil {
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

// roundTripFunc adapts a plain function to http.RoundTripper, so a test can
// stand in for imageClient's transport without a real network call — and,
// just as importantly, without going through dialPublicOnly, which would
// refuse an httptest.Server's loopback address regardless of what it serves.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// withFakeImageClient swaps imageClient for the duration of a test, counting
// requests per URL and answering canned responses. It restores the original
// client on cleanup, since imageClient is package state shared by every test.
func withFakeImageClient(t *testing.T, responses map[string]*http.Response) *map[string]int {
	t.Helper()
	fetches := map[string]int{}

	original := imageClient
	imageClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		fetches[r.URL.String()]++
		if response, ok := responses[r.URL.String()]; ok {
			return response, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	t.Cleanup(func() { imageClient = original })

	return &fetches
}

func imageResponse(contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

var localImageSrc = regexp.MustCompile(`src="(/documents/\d+/images/\d+)"`)

// TestArticleImagesAreFetchedAndServedLocally is the point of the whole
// cache: an article's own page never leaks the original image host to a
// browser, and the image is actually reachable from where the page says it
// is.
func TestArticleImagesAreFetchedAndServedLocally(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	fetches := withFakeImageClient(t, map[string]*http.Response{
		"http://images.test/cat.png": imageResponse("image/png", "fake-png-bytes"),
	})

	if err := db.SetDocumentContent(1,
		`<p>Before.</p><img src="http://images.test/cat.png" alt="A cat"><p>After.</p>`,
	); err != nil {
		t.Fatalf("SetDocumentContent: %v", err)
	}

	body := get(t, server, "/read/1").Body.String()
	if strings.Contains(body, "images.test") {
		t.Errorf("rendered page leaked the original image host:\n%s", body)
	}
	if !strings.Contains(body, `alt="A cat"`) {
		t.Errorf("rendered page is missing the image:\n%s", body)
	}

	match := localImageSrc.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("no local image URL found in:\n%s", body)
	}

	image := get(t, server, match[1])
	if image.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200", match[1], image.Code)
	}
	if got := image.Body.String(); got != "fake-png-bytes" {
		t.Errorf("%s: body = %q, want %q", match[1], got, "fake-png-bytes")
	}
	if got := image.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("%s: Content-Type = %q, want image/png", match[1], got)
	}
	if !strings.Contains(image.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("%s: Cache-Control = %q, want a long-lived, immutable value",
			match[1], image.Header().Get("Cache-Control"))
	}

	// A second read must not fetch the image again — that is the entire
	// point of caching it.
	get(t, server, "/read/1")
	if got := (*fetches)["http://images.test/cat.png"]; got != 1 {
		t.Errorf("image was fetched %d times, want exactly 1", got)
	}
}

// TestFailedImageFetchIsCachedTooAndOmitted: a broken image link must not
// break the page, and must not be retried on every single render either —
// see store.DocumentImage.OK.
func TestFailedImageFetchIsCachedTooAndOmitted(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	fetches := withFakeImageClient(t, map[string]*http.Response{
		// deliberately no entry for the URL below — every request 404s
	})

	if err := db.SetDocumentContent(1,
		`<p>Before.</p><img src="http://images.test/gone.png" alt="Gone"><p>After.</p>`,
	); err != nil {
		t.Fatalf("SetDocumentContent: %v", err)
	}

	body := get(t, server, "/read/1").Body.String()
	if strings.Contains(body, "<img") {
		t.Errorf("a failed image fetch still rendered an <img>:\n%s", body)
	}
	if !strings.Contains(body, "Before.") || !strings.Contains(body, "After.") {
		t.Errorf("a failed image fetch broke the rest of the page:\n%s", body)
	}

	get(t, server, "/read/1")
	if got := (*fetches)["http://images.test/gone.png"]; got != 1 {
		t.Errorf("a failed fetch was retried %d times, want exactly 1", got)
	}
}

// TestArticleImageWithDisallowedContentTypeIsRejected: a URL that answers
// with something other than an image — a redirect chain ending on an HTML
// login page is the realistic case — must not be cached and served back as
// if it were one.
func TestArticleImageWithDisallowedContentTypeIsRejected(t *testing.T) {
	server, db, _ := newTestServer(t, true)
	withFakeImageClient(t, map[string]*http.Response{
		"http://images.test/not-an-image": imageResponse("text/html", "<html>nope</html>"),
	})

	if err := db.SetDocumentContent(1,
		`<img src="http://images.test/not-an-image" alt="Nope">`,
	); err != nil {
		t.Fatalf("SetDocumentContent: %v", err)
	}

	body := get(t, server, "/read/1").Body.String()
	if strings.Contains(body, "<img") {
		t.Errorf("a non-image response was rendered as an image:\n%s", body)
	}
}

// TestIsDisallowedHost guards the SSRF check dialPublicOnly applies to every
// image fetch and every redirect it follows. An article is untrusted
// content; this is what stops it from turning increader into a probe
// against its own Tailscale network or its own host.
func TestIsDisallowedHost(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"private RFC 1918", "192.168.1.1", true},
		{"private RFC 1918 10/8", "10.0.0.5", true},
		{"link-local", "169.254.1.1", true},
		{"unspecified", "0.0.0.0", true},
		{"tailscale / CGNAT range", "100.64.0.1", true},
		{"tailscale / CGNAT range, high end", "100.127.255.254", true},
		{"a real public address", "93.184.216.34", false},
		{"just outside the CGNAT range", "100.63.255.255", false},
		{"just past the CGNAT range", "100.128.0.0", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ip := net.ParseIP(test.ip)
			if ip == nil {
				t.Fatalf("test premise is wrong: %q does not parse as an IP", test.ip)
			}
			if got := isDisallowedHost(ip); got != test.want {
				t.Errorf("isDisallowedHost(%s) = %v, want %v", test.ip, got, test.want)
			}
		})
	}
}

// TestFetchImageRejectsNonHTTPSchemes guards against an article's <img src>
// pointing at something other than the web — file:// most importantly, which
// would otherwise let an article read the server's own filesystem.
func TestFetchImageRejectsNonHTTPSchemes(t *testing.T) {
	for _, scheme := range []string{"file:///etc/passwd", "ftp://example.com/x", "javascript:alert(1)"} {
		if _, _, err := fetchImage(context.Background(), scheme); err == nil {
			t.Errorf("fetchImage(%q) succeeded, want an error", scheme)
		}
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	for _, path := range []string{"/static/htmx.min.js", "/static/app.js", "/static/app.css", "/static/mathjax-tex-svg.js"} {
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
	}}, 0, time.Now()); err != nil {
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
	}, 0, time.Now()); err != nil {
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

// TestLibraryOffersRescheduling mirrors TestExtractsPageOffersRescheduling
// for whole articles: the library is where a reader notices something is
// scheduled further out (or sooner) than they'd like just as often as the
// extracts page is.
func TestLibraryOffersRescheduling(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	body := get(t, server, "/library").Body.String()
	if !strings.Contains(body, `hx-post="/elements/1/backlog"`) {
		t.Errorf("no reschedule control for the unread article:\n%s", body)
	}

	// Archived material (wallabag's own "already read") has nothing left to
	// reschedule, same reasoning as a finished extract.
	if err := db.SaveSchedule(1, ir.Schedule{State: ir.StateSuspended}, time.Now()); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	body = get(t, server, "/library").Body.String()
	if strings.Contains(body, `hx-post="/elements/1/backlog"`) {
		t.Error("a suspended article still offers rescheduling")
	}
	// It gets the unsuspend button instead.
	if !strings.Contains(body, `hx-post="/elements/1/unsuspend"`) {
		t.Error("a suspended article lost its \"queue it\" button")
	}
}

// TestReadPointSurvivesGrading is the mid-read pause: stopping records where
// you stopped, and returning shows it rather than silently scrolling.
func TestReadPointSurvivesGrading(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	response := post(t, server, "/elements/1/grade", url.Values{
		"grade": {"next"},
		"block": {"2"},
	})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	element, _ := db.ElementByID(1)
	if element.ReadBlock != 2 {
		t.Errorf("read point = %d, want 2", element.ReadBlock)
	}
	if element.Schedule.Reps != 1 {
		t.Errorf("pausing did not reschedule: reps = %d", element.Schedule.Reps)
	}

	// Reopening marks the block, so the boundary between read and unread is
	// visible rather than merely scrolled to.
	body := get(t, server, "/read/1").Body.String()
	if !strings.Contains(body, `class="read-point" data-b="2"`) {
		t.Errorf("read point is not marked in the article:\n%s", body)
	}
	if !strings.Contains(body, `data-read-block="2"`) {
		t.Error("the reader does not carry the resume position")
	}
}

func TestSuspendAndUnsuspend(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if response := post(t, server, "/elements/1/grade", url.Values{
		"grade": {"suspend"},
		"block": {"1"},
	}); response.Code != http.StatusSeeOther {
		t.Fatalf("suspend: status = %d", response.Code)
	}

	element, _ := db.ElementByID(1)
	if element.Schedule.State != "suspended" {
		t.Errorf("state = %q, want suspended", element.Schedule.State)
	}

	queue, _ := db.Queue(time.Now(), 10)
	if len(queue) != 0 {
		t.Errorf("suspended article is still queued")
	}

	// The reader still opens it and offers to put it back.
	body := get(t, server, "/read/1").Body.String()
	if !strings.Contains(body, "Put back in the queue") {
		t.Error("the reader does not offer to unsuspend")
	}

	if response := post(t, server, "/elements/1/unsuspend", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("unsuspend: status = %d", response.Code)
	}

	queue, _ = db.Queue(time.Now(), 10)
	if len(queue) != 1 {
		t.Errorf("unsuspending did not return the article to the queue")
	}
}

// TestArchivedArticleIsNotQueuedButIsReadable is the whole point of syncing
// wallabag's archive flag.
func TestArchivedArticleIsNotQueuedButIsReadable(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "2", Title: "Read long ago", IsArchived: true, UpdatedAt: time.Now(),
	}}, 0, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	queue := get(t, server, "/").Body.String()
	if strings.Contains(queue, "Read long ago") {
		t.Error("an archived article is in the queue")
	}

	library := get(t, server, "/library").Body.String()
	if !strings.Contains(library, "Read long ago") {
		t.Error("the archived article vanished from the library too")
	}
	if !strings.Contains(library, "queue it") {
		t.Error("the library offers no way to pull it back")
	}

	if response := get(t, server, "/read/2"); response.Code != http.StatusOK {
		t.Errorf("archived article is not readable: status %d", response.Code)
	}
}

// TestSyncImportedHighlightsAreAnchoredOnOpen closes the loop between the two
// import paths: highlights arrive during sync without a position, and opening
// the article gives them one so they render as marks.
func TestSyncImportedHighlightsAreAnchoredOnOpen(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	// As a sync would leave them: present, but with no position.
	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "A test article", UpdatedAt: time.Now(),
		Highlights: []source.Highlight{
			{ExternalID: "500", Quote: "quick brown"},
			{ExternalID: "501", Quote: "a passage that is not in this article"},
		},
	}}, 0, time.Now()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	before, _ := db.ChildrenOf(1)
	if len(before) != 2 {
		t.Fatalf("got %d extracts, want 2", len(before))
	}
	for _, extract := range before {
		if extract.HasRange {
			t.Fatal("test premise is wrong: extracts should start unanchored")
		}
	}

	body := get(t, server, "/read/1").Body.String()

	after, _ := db.ChildrenOf(1)
	if len(after) != 2 {
		t.Errorf("opening the article duplicated extracts: %d", len(after))
	}

	byRef := map[string]store.Element{}
	for _, extract := range after {
		byRef[extract.ExternalRef] = extract
	}
	if !byRef["500"].HasRange {
		t.Error("a locatable highlight was not anchored when the article was opened")
	}
	if byRef["501"].HasRange {
		t.Error("a highlight whose text is absent was given a position")
	}
	if !strings.Contains(body, `<mark class="extract"`) {
		t.Error("the anchored highlight does not render as a mark")
	}
}

func TestExtractsPage(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "A test article", UpdatedAt: time.Now(),
		Highlights: []source.Highlight{{ExternalID: "500", Quote: "An imported passage."}},
	}}, 0, time.Now()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"4"},
		"end_block": {"0"}, "end_offset": {"15"}, "quote": {"quick brown"},
	})

	all := get(t, server, "/extracts").Body.String()
	if !strings.Contains(all, "An imported passage.") || !strings.Contains(all, "quick brown") {
		t.Errorf("extracts page is missing entries:\n%s", all)
	}
	// Whole articles are not extracts.
	if strings.Contains(all, `href="/read/1"`) && !strings.Contains(all, "from") {
		t.Error("the extracts page appears to list the article itself")
	}

	mine := get(t, server, "/extracts?origin=manual").Body.String()
	if strings.Contains(mine, "An imported passage.") {
		t.Error("the manual filter returned an imported extract")
	}
	if !strings.Contains(mine, "quick brown") {
		t.Error("the manual filter dropped a manual extract")
	}

	if response := get(t, server, "/extracts?origin=bogus"); response.Code != http.StatusBadRequest {
		t.Errorf("unknown origin filter: status = %d, want 400", response.Code)
	}
}

// TestExtractsPageSorts checks the sort control actually reorders the rows,
// and that it survives into the filter tabs and search form so switching
// tabs or searching doesn't silently reset it back to newest-first.
func TestExtractsPageSorts(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	// articleBody is "The quick brown fox." — two non-overlapping extracts
	// from the seeded document.
	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"4"},
		"end_block": {"0"}, "end_offset": {"15"}, "quote": {"quick brown"},
	})
	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"16"},
		"end_block": {"0"}, "end_offset": {"19"}, "quote": {"fox"},
	})
	// Push "fox" out well past "quick brown" so a due-date sort can tell them apart.
	if response := post(t, server, "/elements/3/backlog", url.Values{"days": {"30"}}); response.Code != http.StatusSeeOther && response.Code != http.StatusOK {
		t.Fatalf("backlog: status = %d: %s", response.Code, response.Body.String())
	}

	body := get(t, server, "/extracts?sort=due").Body.String()
	if strings.Index(body, "quick brown") > strings.Index(body, "fox") || !strings.Contains(body, "quick brown") {
		t.Errorf("sort=due did not put the soonest-due extract first:\n%s", body)
	}
	if !strings.Contains(body, `value="due" selected`) {
		t.Errorf("sort select did not remember sort=due:\n%s", body)
	}

	tabs := get(t, server, "/extracts?sort=due&origin=manual").Body.String()
	if !strings.Contains(tabs, "sort=due") {
		t.Errorf("filter tabs dropped sort=due when combined with origin:\n%s", tabs)
	}

	if response := get(t, server, "/extracts?sort=bogus"); response.Code != http.StatusBadRequest {
		t.Errorf("unknown sort: status = %d, want 400", response.Code)
	}
}

// TestExtractsPageOffersRescheduling: browsing the harvest is also where a
// reader notices something is scheduled further out (or sooner) than they'd
// like, so the same backlog presets the reader page offers are available
// here too — see ir.BacklogOptions.
func TestExtractsPageOffersRescheduling(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	extractID, err := db.CreateExtract(store.NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "A passage.",
		ContentHTML: "<p>A passage.</p>", Priority: 0.6,
	}, time.Now())
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	body := get(t, server, "/extracts").Body.String()
	if !strings.Contains(body, time.Now().Format("2 Jan 2006")) {
		t.Errorf("no due-date status shown for a due-today extract:\n%s", body)
	}
	if !strings.Contains(body, `hx-post="/elements/`+itoa(extractID)+`/backlog"`) {
		t.Errorf("no reschedule control for the ungraded extract:\n%s", body)
	}
	if !strings.Contains(body, "reschedule…") {
		t.Error("reschedule control is missing its placeholder option")
	}
	// redirect sends the reader back to this list, not into the reading
	// queue — the reader page's own backlog buttons have no such field and
	// fall back to /next instead.
	if !strings.Contains(body, "redirect: window.location.pathname") {
		t.Error("reschedule control does not send the reader back to this page")
	}

	// Finished material has nothing left to reschedule: its due date does
	// not matter once grading has excluded it from the queue for good.
	if err := db.SaveSchedule(extractID, ir.Schedule{State: ir.StateDone}, time.Now()); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	body = get(t, server, "/extracts").Body.String()
	if strings.Contains(body, `hx-post="/elements/`+itoa(extractID)+`/backlog"`) {
		t.Error("a finished extract still offers rescheduling")
	}
}

// TestDoneArchivesUpstream is what the reader asked for: finishing an article
// here finishes it in wallabag, so the two views stop drifting.
func TestDoneArchivesUpstream(t *testing.T) {
	for _, grade := range []string{"done", "dismiss"} {
		t.Run(grade, func(t *testing.T) {
			server, db, _ := newTestServer(t, true)

			if response := post(t, server, "/elements/1/grade", url.Values{
				"grade": {grade},
			}); response.Code != http.StatusSeeOther {
				t.Fatalf("status = %d", response.Code)
			}

			document, _ := db.DocumentByID(1)
			if !document.IsArchived {
				t.Error("the article was not archived locally")
			}

			writes, err := db.PendingWrites("wallabag", 10)
			if err != nil {
				t.Fatalf("PendingWrites: %v", err)
			}
			if len(writes) != 1 || writes[0].Operation != store.OpArchive {
				t.Fatalf("queued %+v, want one archive write", writes)
			}
			if !store.PayloadBool(writes[0].Payload) {
				t.Error("the queued write does not say archived")
			}
		})
	}
}

// TestPauseDoesNotArchive keeps write-back to the two grades that mean "I am
// finished". Pausing an article must not remove it from wallabag's unread list.
func TestPauseDoesNotArchive(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/grade", url.Values{"grade": {"next"}, "block": {"1"}})

	document, _ := db.DocumentByID(1)
	if document.IsArchived {
		t.Error("pausing archived the article")
	}
	writes, _ := db.PendingWrites("wallabag", 10)
	if len(writes) != 0 {
		t.Errorf("pausing queued %d writes, want none", len(writes))
	}
}

// TestExtractsDoNotArchiveOnFinish: an extract is a passage, not a whole
// article, so finishing one must not touch the article's own archive state.
// Making the extract does queue its own highlight_create write — a manual
// extract is now pushed to wallabag as an annotation — but grading it must
// not add anything further.
func TestExtractsDoNotArchiveOnFinish(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"4"},
		"end_block": {"0"}, "end_offset": {"15"}, "quote": {"quick brown"},
	})
	children, _ := db.ChildrenOf(1)

	post(t, server, "/elements/"+itoa(children[0].ID)+"/grade", url.Values{"grade": {"done"}})

	document, _ := db.DocumentByID(1)
	if document.IsArchived {
		t.Error("finishing an extract archived its whole article")
	}
	writes, _ := db.PendingWrites("wallabag", 10)
	if len(writes) != 1 || writes[0].Operation != store.OpHighlightCreate {
		t.Errorf("queued writes = %+v, want exactly one highlight_create from creating the extract", writes)
	}
}

func TestUnsuspendUnarchivesUpstream(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "2", Title: "Read long ago", IsArchived: true, UpdatedAt: time.Now(),
	}}, 0, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if response := post(t, server, "/elements/2/unsuspend", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", response.Code)
	}

	document, _ := db.DocumentByID(2)
	if document.IsArchived {
		t.Error("re-queuing did not unarchive the article locally")
	}

	writes, _ := db.PendingWrites("wallabag", 10)
	if len(writes) != 1 || store.PayloadBool(writes[0].Payload) {
		t.Errorf("queued %+v, want one unarchive write", writes)
	}
}

func TestStarToggle(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if response := post(t, server, "/elements/1/star", url.Values{
		"starred": {"1"},
	}); response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", response.Code)
	}

	document, _ := db.DocumentByID(1)
	if !document.IsStarred {
		t.Error("starring did not take locally")
	}
	writes, _ := db.PendingWrites("wallabag", 10)
	if len(writes) != 1 || writes[0].Operation != store.OpStar {
		t.Fatalf("queued %+v, want one star write", writes)
	}

	body := get(t, server, "/read/1").Body.String()
	if !strings.Contains(body, "★") {
		t.Error("the reader does not show the article as starred")
	}
}

func TestTagEditing(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if response := post(t, server, "/elements/1/tags", url.Values{
		"label": {"philosophy"},
	}); response.Code != http.StatusSeeOther {
		t.Fatalf("add: status = %d", response.Code)
	}

	tags, _ := db.TagsOf(1)
	if len(tags) != 1 || tags[0] != "philosophy" {
		t.Fatalf("tags = %v, want [philosophy]", tags)
	}
	if !strings.Contains(get(t, server, "/read/1").Body.String(), "philosophy") {
		t.Error("the reader does not show the tag")
	}

	if response := post(t, server, "/elements/1/tags/remove", url.Values{
		"label": {"philosophy"},
	}); response.Code != http.StatusSeeOther {
		t.Fatalf("remove: status = %d", response.Code)
	}
	tags, _ = db.TagsOf(1)
	if len(tags) != 0 {
		t.Errorf("tags = %v after removal, want none", tags)
	}

	writes, _ := db.PendingWrites("wallabag", 10)
	if len(writes) != 2 {
		t.Errorf("queued %d writes, want an add and a remove", len(writes))
	}

	// A comma would silently become two tags in wallabag.
	if response := post(t, server, "/elements/1/tags", url.Values{
		"label": {"one,two"},
	}); response.Code != http.StatusBadRequest {
		t.Errorf("a comma-containing tag was accepted: status %d", response.Code)
	}
	if response := post(t, server, "/elements/1/tags", url.Values{
		"label": {"   "},
	}); response.Code != http.StatusBadRequest {
		t.Errorf("a blank tag was accepted: status %d", response.Code)
	}
}

func TestLibraryFilterTabs(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "2", Title: "Starred piece", IsStarred: true, UpdatedAt: time.Now()},
		{ExternalID: "3", Title: "Archived piece", IsArchived: true, UpdatedAt: time.Now(),
			Tags: []string{"philosophy"}},
	}, 0, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	starred := get(t, server, "/library?state=starred").Body.String()
	if !strings.Contains(starred, "Starred piece") || strings.Contains(starred, "Archived piece") {
		t.Error("the starred filter returned the wrong set")
	}

	tagged := get(t, server, "/library?tag=philosophy").Body.String()
	if !strings.Contains(tagged, "Archived piece") || strings.Contains(tagged, "Starred piece") {
		t.Error("the tag filter returned the wrong set")
	}

	if response := get(t, server, "/library?state=bogus"); response.Code != http.StatusBadRequest {
		t.Errorf("unknown state filter: status = %d, want 400", response.Code)
	}
}

// TestLibraryScheduledFilterFindsGradedArticles: "scheduled" is for finding
// something pushed out further than intended — distinct from a still-fresh
// import (never touched) and from archived (wallabag's own "already read").
// Furthest due date first, so an outlier is the very first thing shown.
func TestLibraryScheduledFilterFindsGradedArticles(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "2", Title: "Backlogged far out", UpdatedAt: time.Now()},
		{ExternalID: "3", Title: "Backlogged not as far", UpdatedAt: time.Now()},
		{ExternalID: "4", Title: "Never touched", UpdatedAt: time.Now()},
		{ExternalID: "5", Title: "Archived on wallabag", IsArchived: true, UpdatedAt: time.Now()},
	}, 0, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	post(t, server, "/elements/2/backlog", url.Values{"days": {"400"}})
	post(t, server, "/elements/3/backlog", url.Values{"days": {"10"}})

	body := get(t, server, "/library?state=scheduled").Body.String()
	if !strings.Contains(body, "Backlogged far out") || !strings.Contains(body, "Backlogged not as far") {
		t.Errorf("scheduled filter is missing a backlogged article:\n%s", body)
	}
	if strings.Contains(body, "Never touched") {
		t.Error("scheduled filter included an article that was never graded or backlogged")
	}
	if strings.Contains(body, "Archived on wallabag") {
		t.Error("scheduled filter included an archived article")
	}

	// Furthest out first.
	far := strings.Index(body, "Backlogged far out")
	near := strings.Index(body, "Backlogged not as far")
	if far == -1 || near == -1 || far > near {
		t.Error("scheduled filter is not sorted furthest due date first")
	}
}

// TestBuryKeepsItTodayButLast covers the skip case end to end.
func TestBuryKeepsItTodayButLast(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "2", Title: "Second", UpdatedAt: time.Now()},
		{ExternalID: "3", Title: "Third", UpdatedAt: time.Now()},
	}, 0, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, _ := db.Queue(time.Now(), 10)
	first := before[0].ID

	if response := post(t, server, "/elements/"+itoa(first)+"/grade", url.Values{
		"grade": {"bury"},
		"block": {"1"},
	}); response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	after, _ := db.Queue(time.Now(), 10)
	if len(after) != len(before) {
		t.Errorf("burying removed the element from today: %d then %d", len(before), len(after))
	}
	if after[len(after)-1].ID != first {
		t.Error("the buried element is not last in today's queue")
	}

	// Its schedule is untouched — burying is about position, not timing.
	element, _ := db.ElementByID(first)
	if element.Schedule.Reps != 0 {
		t.Errorf("burying counted as a repetition: reps = %d", element.Schedule.Reps)
	}
}

// TestGradeButtonsShowTheirIntervals is the point of the whole redesign: a
// grade you cannot see the effect of is one you have to memorise.
func TestGradeButtonsShowTheirIntervals(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	// Give it some history so the intervals differ from each other.
	if err := db.SaveSchedule(1, ir.Schedule{
		State: ir.StateReading, IntervalDays: 8, AFactor: 2.0, Reps: 3, Priority: 0.9,
	}, time.Now()); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}

	body := get(t, server, "/read/1").Body.String()

	element, _ := db.ElementByID(1)
	for _, grade := range []ir.Grade{ir.GradeNext, ir.GradeSooner} {
		want := ir.FormatInterval(ir.Next(element.Schedule, grade, time.Now()).IntervalDays)
		if !strings.Contains(body, ">"+want+"<") {
			t.Errorf("the page does not show the interval %q for grade %d", want, grade)
		}
	}

	// The three groups are labelled, so the cases are visible rather than implied.
	for _, label := range []string{"Finished", "Skip", "Schedule"} {
		if !strings.Contains(body, label) {
			t.Errorf("the grading bar has no %q group", label)
		}
	}
	// Dismiss must not sit among the everyday buttons.
	if strings.Contains(body, `grade-buttons">`+"\n"+`          <button class="danger"`) {
		t.Error("Dismiss is in the main button row")
	}
}

func TestNewExtractIsNotDueToday(t *testing.T) {
	server, db, _ := newTestServerWithDelay(t, 10)

	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"4"},
		"end_block": {"0"}, "end_offset": {"15"}, "quote": {"quick brown"},
	})

	children, _ := db.ChildrenOf(1)
	if len(children) != 1 {
		t.Fatalf("got %d extracts, want 1", len(children))
	}

	want := ir.Day(time.Now().AddDate(0, 0, 10))
	if !ir.Day(children[0].Schedule.DueOn).Equal(want) {
		t.Errorf("new extract due %v, want %v", children[0].Schedule.DueOn, want)
	}

	queue, _ := db.Queue(time.Now(), 10)
	for _, item := range queue {
		if item.ID == children[0].ID {
			t.Error("a freshly made extract is back in today's queue")
		}
	}
}

// TestDeleteManualExtract covers the "accidental entry" case directly: a
// stray selection turned into an extract, with nothing upstream to clean up.
func TestDeleteManualExtract(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"4"},
		"end_block": {"0"}, "end_offset": {"15"}, "quote": {"quick brown"},
	})
	children, _ := db.ChildrenOf(1)
	if len(children) != 1 {
		t.Fatalf("got %d extracts, want 1", len(children))
	}
	extractID := children[0].ID

	if response := del(t, server, "/elements/"+itoa(extractID)); response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	remaining, _ := db.ChildrenOf(1)
	if len(remaining) != 0 {
		t.Errorf("got %d extracts after delete, want 0", len(remaining))
	}

	// No provider identity, so nothing was queued to send anywhere.
	writes, _ := db.PendingWrites("wallabag", 10)
	if len(writes) != 0 {
		t.Errorf("a manual delete queued %d writes, want 0", len(writes))
	}
}

// TestDeleteImportedExtractQueuesUpstreamRemoval is the point of pairing the
// delete with an outbox write: without it, the highlight would survive at
// wallabag and the next sync would recreate the very extract just deleted.
func TestDeleteImportedExtractQueuesUpstreamRemoval(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "A test article", UpdatedAt: time.Now(),
		Highlights: []source.Highlight{{ExternalID: "500", Quote: "An imported passage."}},
	}}, 0, time.Now()); err != nil {
		t.Fatalf("seed highlight: %v", err)
	}

	children, _ := db.ChildrenOf(1)
	if len(children) != 1 || children[0].Origin != store.OriginImport {
		t.Fatalf("test premise is wrong: %+v", children)
	}
	extractID := children[0].ID

	if response := del(t, server, "/elements/"+itoa(extractID)); response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	remaining, _ := db.ChildrenOf(1)
	if len(remaining) != 0 {
		t.Errorf("got %d extracts after delete, want 0", len(remaining))
	}

	writes, err := db.PendingWrites("wallabag", 10)
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if len(writes) != 1 {
		t.Fatalf("got %d queued writes, want 1", len(writes))
	}
	if writes[0].Operation != store.OpHighlightDelete || writes[0].ExternalID != "500" {
		t.Errorf("queued %+v, want a highlight_delete for annotation 500", writes[0])
	}
}

func TestDeleteExtractRejectsWholeArticle(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if response := del(t, server, "/elements/1"); response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.Code)
	}

	// Untouched.
	if _, err := db.ElementByID(1); err != nil {
		t.Errorf("the article was removed despite the rejection: %v", err)
	}
}

// TestDeleteExtractSwapOnlyReturnsEmptyBody is the extracts-list path: htmx
// swaps the row's own <li> for whatever the response body contains, so it must
// be empty rather than a redirect, or the row would not disappear cleanly.
func TestDeleteExtractSwapOnlyReturnsEmptyBody(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"4"},
		"end_block": {"0"}, "end_offset": {"15"}, "quote": {"quick brown"},
	})
	children, _ := db.ChildrenOf(1)
	extractID := children[0].ID

	response := del(t, server, "/elements/"+itoa(extractID)+"?swap_only=1")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if response.Header().Get("HX-Redirect") != "" {
		t.Error("swap_only still redirected; the row would navigate away instead of vanishing")
	}
	if response.Body.Len() != 0 {
		t.Errorf("swap_only response carried a body: %q", response.Body.String())
	}
}

func TestDeleteExtractMissing(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	if response := del(t, server, "/elements/999"); response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.Code)
	}
}

// TestDeleteDocumentRequiresMissingFlag is what keeps the library's delete
// button from being a general-purpose "remove any article" action: a
// document still found upstream would just be re-created on the very next
// sync, so deleting one is refused until reconciliation has flagged it gone.
func TestDeleteDocumentRequiresMissingFlag(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	if response := del(t, server, "/documents/1"); response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — the document still exists upstream", response.Code)
	}
	if _, err := db.DocumentByID(1); err != nil {
		t.Errorf("the document was removed despite the rejection: %v", err)
	}
}

func TestDeleteDocumentRemovesAFlaggedOne(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	// An empty listing: nothing is present any more, so document 1 gets
	// flagged missing.
	if _, _, err := db.ReconcileMissing("wallabag", nil); err != nil {
		t.Fatalf("ReconcileMissing: %v", err)
	}

	response := del(t, server, "/documents/1")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", response.Code)
	}
	if _, err := db.DocumentByID(1); err == nil {
		t.Error("the document survived the delete")
	}
}

func TestDeleteDocumentMissing(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	if response := del(t, server, "/documents/999"); response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.Code)
	}
}
