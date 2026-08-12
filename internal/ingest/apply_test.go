package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/wallabag"
)

// requestRecorder is a minimal thread-safe log of "METHOD /path" strings, in
// arrival order — what the ordering test asserts on directly, and what the
// others use to count how many requests a given endpoint saw.
type requestRecorder struct {
	mu   sync.Mutex
	logs []string
}

func (r *requestRecorder) record(entry string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, entry)
}

func (r *requestRecorder) count(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, entry := range r.logs {
		if strings.HasPrefix(entry, prefix) {
			n++
		}
	}
	return n
}

func (r *requestRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.logs))
	copy(out, r.logs)
	return out
}

// fakeWallabagOptions configures the one thing each Apply test actually
// varies: which writes the fake server should fail.
type fakeWallabagOptions struct {
	// patchStatus, keyed by entry id, overrides PATCH /api/entries/{id}.json
	// away from its default 200 — the content-write-failure test's whole
	// mechanism.
	patchStatus map[int]int

	// failingQuotes marks specific annotation quote texts that
	// POST /api/annotations/{id}.json (CreateHighlight's endpoint, the first
	// half of UpdateHighlightLocation) should fail for. Keyed by quote text
	// rather than by id because every annotation being re-anchored onto the
	// same entry shares that entry's single path — the quote in the request
	// body is the only thing distinguishing which one this call is.
	failingQuotes map[string]bool
}

// newFakeWallabag starts an httptest server implementing exactly the
// endpoints Apply's write path exercises: OAuth, PATCH/POST on entries,
// GET on an entry (CreateHighlight's best-effort ranges lookup), and
// POST/DELETE on annotations (UpdateHighlightLocation's create-then-delete).
// Every request is appended to the returned recorder, in arrival order.
//
// Routed by hand with a single catch-all handler rather than net/http's own
// pattern matching: ServeMux's {wildcard} syntax requires a wildcard segment
// to end the pattern (or be followed by a literal '/'), which rejects
// "/api/entries/{id}.json" outright — wallabag's own paths all end in a
// literal ".json" suffix glued straight onto the id, a shape the standard
// router simply cannot express.
func newFakeWallabag(t *testing.T, opts fakeWallabagOptions) (*httptest.Server, *requestRecorder) {
	t.Helper()
	recorder := &requestRecorder{}
	nextAnnotationID := 600

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/oauth/v2/token":
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "test-token", "expires_in": 3600, "token_type": "bearer",
			})

		case r.Method == "POST" && r.URL.Path == "/api/entries.json":
			recorder.record("POST /api/entries.json")
			json.NewEncoder(w).Encode(wallabag.Entry{ID: 900})

		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/api/entries/"):
			id := entryIDFromPath(r.URL.Path, "/api/entries/")
			recorder.record("PATCH /api/entries/" + fmt.Sprint(id) + ".json")
			if status, overridden := opts.patchStatus[id]; overridden && status != http.StatusOK {
				http.Error(w, "simulated failure", status)
				return
			}
			json.NewEncoder(w).Encode(wallabag.Entry{ID: id})

		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/entries/"):
			id := entryIDFromPath(r.URL.Path, "/api/entries/")
			recorder.record("GET /api/entries/" + fmt.Sprint(id) + ".json")
			json.NewEncoder(w).Encode(wallabag.Entry{ID: id, Content: "<p>Full content.</p>"})

		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/annotations/"):
			id := entryIDFromPath(r.URL.Path, "/api/annotations/")
			recorder.record("POST /api/annotations/" + fmt.Sprint(id) + ".json")

			body, _ := io.ReadAll(r.Body)
			var decoded struct {
				Quote string `json:"quote"`
			}
			json.Unmarshal(body, &decoded)

			if opts.failingQuotes[decoded.Quote] {
				http.Error(w, "simulated failure", http.StatusInternalServerError)
				return
			}
			nextAnnotationID++
			json.NewEncoder(w).Encode(wallabag.Annotation{ID: nextAnnotationID})

		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/annotations/"):
			id := entryIDFromPath(r.URL.Path, "/api/annotations/")
			recorder.record("DELETE /api/annotations/" + fmt.Sprint(id) + ".json")
			json.NewEncoder(w).Encode(wallabag.Annotation{})

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, recorder
}

// entryIDFromPath extracts the numeric id out of a wallabag-shaped path —
// prefix + "{id}.json" — the same "{id}.json" glued-together shape
// newFakeWallabag's own doc comment explains ServeMux's pattern syntax
// cannot route directly.
func entryIDFromPath(path, prefix string) int {
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.TrimSuffix(rest, ".json")
	var id int
	fmt.Sscanf(rest, "%d", &id)
	return id
}

func testClientAndSource(t *testing.T, serverURL string) (*wallabag.Client, *wallabag.Source) {
	t.Helper()
	client, err := wallabag.New(wallabag.Config{
		URL: serverURL, ClientID: "id", ClientSecret: "secret", Username: "user", Password: "pass",
	})
	if err != nil {
		t.Fatalf("wallabag.New: %v", err)
	}
	return client, wallabag.NewSource(client)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestApplyContentFailureIssuesNoAnnotationRequests pins step 1 of Apply's
// ordering contract: when the content write for an entry fails, that
// entry's annotations must never be touched at all, not even attempted and
// logged as failures — the whole point being that increader must never
// build on a content write it does not actually know succeeded.
func TestApplyContentFailureIssuesNoAnnotationRequests(t *testing.T) {
	server, recorder := newFakeWallabag(t, fakeWallabagOptions{
		patchStatus: map[int]int{1: http.StatusInternalServerError},
	})
	client, src := testClientAndSource(t, server.URL)

	plan := Plan{Items: []Item{{
		Post:    source.Document{URL: "https://example.substack.com/p/a-post", ContentHTML: "<p>New body.</p>", Author: "An Author"},
		EntryID: 1,
		Action:  ActionUpdate,
		Annotations: []AnnotationPlan{
			{AnnotationID: 500, Quote: "stale quote one", Verdict: VerdictUnique},
			{AnnotationID: 501, Quote: "stale quote two", Verdict: VerdictUnique},
		},
	}}}

	applied, err := Apply(context.Background(), client, src, plan, discardLogger())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := recorder.count("POST /api/annotations/"); got != 0 {
		t.Errorf("annotation create requests = %d, want 0 after a content failure", got)
	}
	if got := recorder.count("DELETE /api/annotations/"); got != 0 {
		t.Errorf("annotation delete requests = %d, want 0 after a content failure", got)
	}
	if applied.Updated != 0 {
		t.Errorf("Updated = %d, want 0", applied.Updated)
	}
	if applied.Reanchored != 0 {
		t.Errorf("Reanchored = %d, want 0", applied.Reanchored)
	}
	if len(applied.Errors) != 1 {
		t.Errorf("Errors = %v, want exactly one (the content PATCH failure)", applied.Errors)
	}
	if _, touched := applied.Remaps[1]; touched {
		t.Error("Remaps carries entry 1 despite its content write having failed — Repair would wrongly treat it as done")
	}
}

// TestApplyMiddleAnnotationFailureStillReanchorsTheOthers covers step 2's
// own fail-safety: one annotation among several failing must not stop the
// rest, and must not roll back the content write that already succeeded.
func TestApplyMiddleAnnotationFailureStillReanchorsTheOthers(t *testing.T) {
	server, recorder := newFakeWallabag(t, fakeWallabagOptions{
		failingQuotes: map[string]bool{"the middle one fails": true},
	})
	client, src := testClientAndSource(t, server.URL)

	plan := Plan{Items: []Item{{
		Post:    source.Document{URL: "https://example.substack.com/p/a-post", ContentHTML: "<p>New body.</p>", Author: "An Author"},
		EntryID: 1,
		Action:  ActionUpdate,
		Annotations: []AnnotationPlan{
			{AnnotationID: 500, Quote: "the first one", Verdict: VerdictUnique},
			{AnnotationID: 501, Quote: "the middle one fails", Verdict: VerdictUnique},
			{AnnotationID: 502, Quote: "the third one", Verdict: VerdictUnique},
		},
	}}}

	applied, err := Apply(context.Background(), client, src, plan, discardLogger())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if applied.Updated != 1 {
		t.Errorf("Updated = %d, want 1 — the content write itself did not fail", applied.Updated)
	}
	if applied.Reanchored != 2 {
		t.Errorf("Reanchored = %d, want 2 (the two that succeeded)", applied.Reanchored)
	}
	if applied.AnnotationFailures != 1 {
		t.Errorf("AnnotationFailures = %d, want 1", applied.AnnotationFailures)
	}
	if got := len(applied.Remaps[1]); got != 2 {
		t.Errorf("Remaps[1] has %d entries, want 2 (the two successful re-anchors)", got)
	}
	// The delete half of UpdateHighlightLocation only fires for an
	// annotation whose create half actually succeeded.
	if got := recorder.count("DELETE /api/annotations/"); got != 2 {
		t.Errorf("annotation delete requests = %d, want 2", got)
	}
}

// TestApplyContentPrecedesAnnotationsInRequestOrder is the ordering half of
// the fail-safety design directly: regardless of how many annotations an
// entry has, its content PATCH must be the very first request Apply makes
// for it.
func TestApplyContentPrecedesAnnotationsInRequestOrder(t *testing.T) {
	server, recorder := newFakeWallabag(t, fakeWallabagOptions{})
	client, src := testClientAndSource(t, server.URL)

	plan := Plan{Items: []Item{{
		Post:    source.Document{URL: "https://example.substack.com/p/a-post", ContentHTML: "<p>New body.</p>", Author: "An Author"},
		EntryID: 1,
		Action:  ActionUpdate,
		Annotations: []AnnotationPlan{
			{AnnotationID: 500, Quote: "one", Verdict: VerdictUnique},
			{AnnotationID: 501, Quote: "two", Verdict: VerdictUnique},
		},
	}}}

	if _, err := Apply(context.Background(), client, src, plan, discardLogger()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	requests := recorder.all()
	var patchIndex, firstAnnotationIndex = -1, -1
	for i, entry := range requests {
		if strings.HasPrefix(entry, "PATCH /api/entries/") && patchIndex == -1 {
			patchIndex = i
		}
		if strings.HasPrefix(entry, "POST /api/annotations/") && firstAnnotationIndex == -1 {
			firstAnnotationIndex = i
		}
	}
	if patchIndex == -1 {
		t.Fatal("no content PATCH was recorded at all")
	}
	if firstAnnotationIndex == -1 {
		t.Fatal("no annotation create was recorded at all")
	}
	if patchIndex > firstAnnotationIndex {
		t.Errorf("content PATCH at index %d, first annotation create at index %d, want content first: %v",
			patchIndex, firstAnnotationIndex, requests)
	}
}

// TestApplyReanchorsWithTheTrimmedQuoteNotTheRawStoredOne pins Fix 3's own
// apply.go change directly: the quote Apply hands to UpdateHighlightLocation
// when re-anchoring must be wallabag.TrimTruncationMarker(ann.Quote), not
// AnnotationPlan.Quote verbatim. Sending the raw, still-marked form upstream
// would hand wallabag a literal trailing "…" that is not actually part of
// the article, defeating CreateHighlight's own quote-location lookup
// (computeRanges in internal/wallabag/ranges.go) for exactly the reason
// this whole change exists to fix — see plan.go's own comment on the
// 2026-08-12 finding this traces back to.
func TestApplyReanchorsWithTheTrimmedQuoteNotTheRawStoredOne(t *testing.T) {
	var sentQuote string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600, "token_type": "bearer"})
	})
	mux.HandleFunc("PATCH /api/entries/1.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wallabag.Entry{ID: 1})
	})
	mux.HandleFunc("GET /api/entries/1.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wallabag.Entry{ID: 1, Content: "<p>a passage worth keeping</p>"})
	})
	mux.HandleFunc("POST /api/annotations/1.json", func(w http.ResponseWriter, r *http.Request) {
		var decoded struct {
			Quote string `json:"quote"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &decoded)
		sentQuote = decoded.Quote
		json.NewEncoder(w).Encode(wallabag.Annotation{ID: 999})
	})
	mux.HandleFunc("DELETE /api/annotations/500.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wallabag.Annotation{})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client, src := testClientAndSource(t, server.URL)

	plan := Plan{Items: []Item{{
		Post:    source.Document{URL: "https://example.substack.com/p/a-post", ContentHTML: "<p>New body.</p>", Author: "An Author"},
		EntryID: 1,
		Action:  ActionUpdate,
		Annotations: []AnnotationPlan{
			// The raw stored quote as wallabag would actually hand it back:
			// truncateQuote's own trailing "…" still attached.
			{AnnotationID: 500, Quote: "a passage worth keeping…", Verdict: VerdictUnique},
		},
	}}}

	if _, err := Apply(context.Background(), client, src, plan, discardLogger()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := wallabag.TrimTruncationMarker("a passage worth keeping…")
	if sentQuote != want {
		t.Errorf("quote sent to re-anchor = %q, want the trimmed form %q, not the raw stored one", sentQuote, want)
	}
	if strings.HasSuffix(sentQuote, "…") {
		t.Error("quote sent to re-anchor still carries the truncation marker")
	}
}

// TestApplySkipsVerdictMissingAnnotations pins the 2026-08-12 production fix
// directly: an annotation classified VerdictMissing must never be
// re-anchored, even though VerdictUnique and VerdictAmbiguous annotations on
// the very same entry are. A live backfill run re-anchored 34 annotations
// against a plan that had classified only 32 as VerdictUnique — the extra
// two were VerdictMissing, and Apply re-anchored them anyway, wiping their
// ranges in the process. See apply.go's own doc comment on why that is data
// loss waiting to happen, not merely cosmetic.
func TestApplySkipsVerdictMissingAnnotations(t *testing.T) {
	server, recorder := newFakeWallabag(t, fakeWallabagOptions{})
	client, src := testClientAndSource(t, server.URL)

	plan := Plan{Items: []Item{{
		Post:    source.Document{URL: "https://example.substack.com/p/a-post", ContentHTML: "<p>New body.</p>", Author: "An Author"},
		EntryID: 1,
		Action:  ActionAnnotationsOnly,
		Annotations: []AnnotationPlan{
			{AnnotationID: 500, Quote: "the unique one", Verdict: VerdictUnique},
			{AnnotationID: 501, Quote: "the ambiguous one", Verdict: VerdictAmbiguous},
			{AnnotationID: 502, Quote: "the missing one", Verdict: VerdictMissing},
		},
	}}}

	applied, err := Apply(context.Background(), client, src, plan, discardLogger())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := recorder.count("POST /api/annotations/"); got != 2 {
		t.Errorf("annotation create requests = %d, want exactly 2 (unique + ambiguous, not missing)", got)
	}
	if applied.Reanchored != 2 {
		t.Errorf("Reanchored = %d, want 2 — only what was actually re-anchored", applied.Reanchored)
	}
	if applied.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the VerdictMissing annotation)", applied.Skipped)
	}
	if applied.AnnotationFailures != 0 {
		t.Errorf("AnnotationFailures = %d, want 0 — skipping is not a failure", applied.AnnotationFailures)
	}

	// The missing annotation's id must appear in no outbound request at all
	// — not a create, not a delete (UpdateHighlightLocation's own
	// create-then-delete never even starts for a skipped annotation).
	for _, req := range recorder.all() {
		if strings.Contains(req, "/502") {
			t.Errorf("request %q references annotation 502 (VerdictMissing), want it untouched", req)
		}
	}
	if got := len(applied.Remaps[1]); got != 2 {
		t.Errorf("Remaps[1] has %d entries, want 2 (the two actually re-anchored, not the skipped one)", got)
	}
}

// TestApplySkipsConflictAndSkipItems covers the other half of Apply's
// dispatch: ActionSkip and ActionConflict items must produce no requests at
// all, matching BuildPlan's own promise that a conflict "writes nothing".
func TestApplySkipsConflictAndSkipItems(t *testing.T) {
	server, recorder := newFakeWallabag(t, fakeWallabagOptions{})
	client, src := testClientAndSource(t, server.URL)

	plan := Plan{
		Items: []Item{
			{Post: source.Document{URL: "https://example.substack.com/p/skip-me"}, EntryID: 1, Action: ActionSkip},
			{Post: source.Document{URL: "https://example.substack.com/p/conflicted"}, Action: ActionConflict,
				Notes: []string{"entry 1 and entry 2 both carry annotations"}},
		},
		Conflicts: 1,
	}

	applied, err := Apply(context.Background(), client, src, plan, discardLogger())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(recorder.all()) != 0 {
		t.Errorf("requests = %v, want none for skip/conflict items", recorder.all())
	}
	if applied.Created != 0 || applied.Updated != 0 || applied.Reanchored != 0 {
		t.Errorf("Applied = %+v, want an entirely zero result", applied)
	}
}
