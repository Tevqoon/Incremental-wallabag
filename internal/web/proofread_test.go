package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/proofread"
	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/store"
)

// TestWordDiffHighlightsOnlyTheChangedWords covers the review page's own
// merged diff (see wordDiff): the dropcap example from the real book that
// prompted this feature, where only the displaced "U" and the corrected
// initial letter should be marked, not the whole passage.
func TestWordDiffHighlightsOnlyTheChangedWords(t *testing.T) {
	got := wordDiff(
		"NDERSTAND, MY SON, that as long as a man U lacks accomplishments",
		"UNDERSTAND, MY SON, that as long as a man lacks accomplishments",
	)
	want := `<del>NDERSTAND,</del> <ins>UNDERSTAND,</ins> MY SON, that as long as a man <del>U</del> lacks accomplishments`
	if string(got) != want {
		t.Errorf("wordDiff =\n%s\nwant\n%s", got, want)
	}
}

// TestWordDiffEscapesHTML guards against a passage that happens to contain
// something HTML-meaningful — a stray "<" an OCR pass produced, say — being
// interpreted as markup instead of shown as the character it is.
func TestWordDiffEscapesHTML(t *testing.T) {
	got := wordDiff("a <script> tag", "a <b>tag</b>")
	if strings.Contains(string(got), "<script>") || strings.Contains(string(got), "<b>tag</b>") {
		t.Errorf("wordDiff did not escape passage content: %s", got)
	}
}

// TestWordDiffIdenticalTextHasNoMarkup covers the case wordDiff is never
// actually asked to render in practice — handleProofreadExtracts only calls
// it when Proposed differs from Original — but should still degenerate to
// plain, unmarked text rather than something misleadingly diff-shaped.
func TestWordDiffIdenticalTextHasNoMarkup(t *testing.T) {
	got := wordDiff("the same passage", "the same passage")
	if strings.Contains(string(got), "<del>") || strings.Contains(string(got), "<ins>") {
		t.Errorf("wordDiff marked up identical text: %s", got)
	}
}

// newProofreadTestServer builds a server wired to a fake OpenAI-compatible
// endpoint, so the "Fix typos" path can be exercised without a real API key.
// respond decides what the fake model returns for a given request body.
func newProofreadTestServer(t *testing.T, respond func(body string) string) (*Server, *store.Store, int64) {
	t.Helper()

	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		type message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		response := struct {
			Choices []struct {
				Message message `json:"message"`
			} `json:"choices"`
		}{}
		response.Choices = []struct {
			Message message `json:"message"`
		}{{Message: message{Role: "assistant", Content: respond(string(body))}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(llm.Close)

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	document := source.Document{
		ExternalID: "scanned",
		Title:      "A Scanned Book",
		Highlights: []source.Highlight{
			{ExternalID: "h-1", Quote: "NDERSTAND, MY SON, that a man U lacks accomplishments", Ordinal: 1},
			{ExternalID: "h-2", Quote: "a passage with nothing wrong with it", Ordinal: 2},
		},
	}
	result, err := db.ImportAnnotations(document, store.ImportOptions{Triage: false}, time.Now())
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}

	server, err := New(Options{
		Store:       db,
		Sources:     map[string]source.Source{},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Proofreader: proofread.NewClient("test-key", llm.URL, ""),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server, db, result.DocumentID
}

// elementIDsFor returns the ids of every annotation on a document, in
// reading order — the same order ImportAnnotations assigned them.
func elementIDsFor(t *testing.T, db *store.Store, documentID int64) []int64 {
	t.Helper()
	annotations, err := db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	ids := make([]int64, len(annotations))
	for i, annotation := range annotations {
		ids[i] = annotation.ID
	}
	return ids
}

func TestProofreadReviewShowsProposedChangeWithoutSavingIt(t *testing.T) {
	server, db, documentID := newProofreadTestServer(t, func(body string) string {
		return `{"` + idFor(t, body, "NDERSTAND") + `": "UNDERSTAND, MY SON, that a man lacks accomplishments"}`
	})
	ids := elementIDsFor(t, db, documentID)

	response := post(t, server, "/documents/"+itoa(documentID)+"/proofread",
		url.Values{"ids": {itoa(ids[0]), itoa(ids[1])}})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
	}
	body := response.Body.String()

	if !strings.Contains(body, "UNDERSTAND, MY SON") {
		t.Error("the review page does not show the proposed correction")
	}
	if !strings.Contains(body, "NDERSTAND, MY SON") {
		t.Error("the review page does not show the original for comparison")
	}
	if !strings.Contains(body, "1 left as they were") {
		t.Errorf("the review page does not report the unchanged passage:\n%s", body)
	}

	// Nothing is written until the reader approves it.
	annotations, err := db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	for _, annotation := range annotations {
		if strings.HasPrefix(annotation.Quote, "UNDERSTAND") {
			t.Error("the passage was saved before the review page was ever submitted")
		}
	}
}

func TestProofreadApplySavesOnlyTheCheckedSuggestion(t *testing.T) {
	server, db, documentID := newProofreadTestServer(t, func(body string) string { return `{}` })
	ids := elementIDsFor(t, db, documentID)

	response := post(t, server, "/documents/"+itoa(documentID)+"/proofread/apply", url.Values{
		"ids":                   {itoa(ids[0])},
		"quote_" + itoa(ids[0]): {"UNDERSTAND, MY SON, that a man lacks accomplishments"},
	})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
	}

	element, err := db.ElementByID(ids[0])
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if element.Quote != "UNDERSTAND, MY SON, that a man lacks accomplishments" {
		t.Errorf("quote = %q, want the applied correction", element.Quote)
	}

	other, err := db.ElementByID(ids[1])
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if other.Quote != "a passage with nothing wrong with it" {
		t.Errorf("the second passage changed even though it was never checked: %q", other.Quote)
	}
}

// TestProofreadHiddenWhenUnconfigured covers the nil-Proofreader default:
// the endpoint 404s and the button never renders, the same convention
// ImportSubstackURL/RefreshSubstackFeed already use for an unconfigured
// optional feature.
func TestProofreadHiddenWhenUnconfigured(t *testing.T) {
	server, _, _ := newTestServer(t, false)

	body := get(t, server, "/documents/1").Body.String()
	if strings.Contains(body, "Fix typos") {
		t.Error("the Fix typos button renders with no proofreader configured")
	}

	response := post(t, server, "/documents/1/proofread", url.Values{"ids": {"1"}})
	if response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 with no proofreader configured", response.Code)
	}
}

// idFor recovers the id the fake request carried for a passage containing
// needle, so the fake server's own response can address it correctly
// without the test hard-coding increader's own id assignment.
func idFor(t *testing.T, requestBody, needle string) string {
	t.Helper()
	var request struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(requestBody), &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(request.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(request.Messages))
	}
	content := request.Messages[1].Content
	start := strings.Index(content, "[")
	if start == -1 {
		t.Fatalf("user message has no JSON array: %s", content)
	}
	var items []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content[start:]), &items); err != nil {
		t.Fatalf("decode items: %v", err)
	}
	for _, item := range items {
		if strings.Contains(item.Text, needle) {
			return item.ID
		}
	}
	t.Fatalf("no item containing %q in %v", needle, items)
	return ""
}
