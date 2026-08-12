package wallabag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCreateEntrySendsFormEncodedBody is the executable record of the
// findings in CreateEntry's own doc comment: every parameter name and
// encoding confirmed against the live app.wallabag.it API on 2026-08-12.
// This exists so a future change to entryForm that gets one of those
// encodings wrong — published_at as something other than Unix seconds,
// authors or tags split across multiple form keys instead of joined,
// archive/starred as "true"/"false" instead of "1"/"0" — fails loudly here
// rather than silently producing a write wallabag quietly ignores or
// misinterprets.
func TestCreateEntrySendsFormEncodedBody(t *testing.T) {
	var (
		contentType string
		form        url.Values
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "tok", ExpiresIn: 3600})
	})
	mux.HandleFunc("POST /api/entries.json", func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		form = r.Form
		json.NewEncoder(w).Encode(Entry{ID: 42})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := testClient(t, server.URL)

	published := time.Date(2019, 3, 14, 9, 26, 53, 0, time.UTC)
	_, err := client.CreateEntry(context.Background(), NewEntry{
		URL:         "https://example.com/article",
		Title:       "A Title",
		Content:     "<p>Body.</p>",
		Language:    "en",
		Authors:     "Some Author",
		PublishedAt: published,
		Tags:        []string{"one", "two"},
		Archived:    true,
		Starred:     false,
	})
	if err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}

	if !strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want form-encoded", contentType)
	}

	tests := []struct {
		field string
		want  string
	}{
		{"url", "https://example.com/article"},
		{"title", "A Title"},
		{"content", "<p>Body.</p>"},
		{"language", "en"},
		{"authors", "Some Author"},
		// Unix seconds as a decimal string — confirmed against the live API,
		// not RFC3339 or any other timestamp shape.
		{"published_at", strconv.FormatInt(published.Unix(), 10)},
		// One comma-separated list, not repeated tags[] keys.
		{"tags", "one,two"},
		// 0/1, not "true"/"false" — the same convention boolParam already
		// applies for SetArchived/SetStarred in write.go.
		{"archive", "1"},
		{"starred", "0"},
	}
	for _, test := range tests {
		if got := form.Get(test.field); got != test.want {
			t.Errorf("form[%q] = %q, want %q", test.field, got, test.want)
		}
	}
}

// TestCreateEntryOmitsUnsetOptionalFields checks the other half of
// entryForm: a NewEntry that sets nothing but URL sends nothing but url
// (plus the always-explicit archive/starred pair), rather than blank values
// for title/content/language/authors/published_at/tags that could suppress
// wallabag's own extractor or otherwise be mistaken for an explicit choice.
func TestCreateEntryOmitsUnsetOptionalFields(t *testing.T) {
	var form url.Values

	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "tok", ExpiresIn: 3600})
	})
	mux.HandleFunc("POST /api/entries.json", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form = r.Form
		json.NewEncoder(w).Encode(Entry{ID: 1})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := testClient(t, server.URL)
	if _, err := client.CreateEntry(context.Background(), NewEntry{URL: "https://example.com/bare"}); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}

	for _, field := range []string{"title", "content", "language", "authors", "published_at", "tags"} {
		if _, present := form[field]; present {
			t.Errorf("form[%q] was sent = %v, want omitted for an unset field", field, form[field])
		}
	}
}

// TestCreateEntryRejectsTagWithComma guards the same rule AddTags already
// enforces in write.go: wallabag's tags parameter is one comma-separated
// list with no escaping convention for a comma inside a single label, so a
// label containing one would silently become two tags instead of the one
// the caller asked for. No server is needed here — entryForm validates
// before CreateEntry ever sends anything.
func TestCreateEntryRejectsTagWithComma(t *testing.T) {
	client := testClient(t, "https://example.invalid")

	_, err := client.CreateEntry(context.Background(), NewEntry{
		URL:  "https://example.com/a",
		Tags: []string{"fine", "not,fine"},
	})
	if err == nil {
		t.Fatal("expected an error for a tag label containing a comma")
	}
}

// TestUpdateEntryContentOnlyOmitsOtherFields covers the asymmetry documented
// on EntryUpdate.form: a content-only PATCH against the live API preserved
// the entry's title but blanked its authors, which is only possible because
// omitted fields are left out of the form entirely rather than sent blank.
// This pins that a content-only EntryUpdate does not send title= or
// authors= at all.
func TestUpdateEntryContentOnlyOmitsOtherFields(t *testing.T) {
	var form url.Values

	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "tok", ExpiresIn: 3600})
	})
	mux.HandleFunc("PATCH /api/entries/42.json", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		form = r.Form
		json.NewEncoder(w).Encode(Entry{ID: 42})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := testClient(t, server.URL)
	if _, err := client.UpdateEntry(context.Background(), 42, EntryUpdate{Content: "<p>New body.</p>"}); err != nil {
		t.Fatalf("UpdateEntry: %v", err)
	}

	if got := form.Get("content"); got != "<p>New body.</p>" {
		t.Errorf("content = %q, want the new body", got)
	}
	if _, present := form["title"]; present {
		t.Errorf("title = %v, want omitted — sending an explicit blank is what blanked authors on the live API", form["title"])
	}
	if _, present := form["authors"]; present {
		t.Errorf("authors = %v, want omitted for a content-only update", form["authors"])
	}
	if _, present := form["published_at"]; present {
		t.Errorf("published_at = %v, want omitted for a content-only update", form["published_at"])
	}
}

// TestUpdateEntrySendsAuthorsWhenSet is the other half: a caller that does
// set Authors gets it sent, exactly the escape hatch EntryUpdate.form's own
// comment says a caller must use on every write that should preserve them.
func TestUpdateEntrySendsAuthorsWhenSet(t *testing.T) {
	var form url.Values

	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "tok", ExpiresIn: 3600})
	})
	mux.HandleFunc("PATCH /api/entries/42.json", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form = r.Form
		json.NewEncoder(w).Encode(Entry{ID: 42})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := testClient(t, server.URL)
	if _, err := client.UpdateEntry(context.Background(), 42, EntryUpdate{Authors: "Carol Author"}); err != nil {
		t.Fatalf("UpdateEntry: %v", err)
	}

	if got := form.Get("authors"); got != "Carol Author" {
		t.Errorf("authors = %q, want it sent when the caller sets it", got)
	}
}

// TestEntryUpdateFormOmitsZeroValues checks form() directly, independent of
// any HTTP round trip, including the one field the table above does not
// otherwise cover on its own: PublishedAt.
func TestEntryUpdateFormOmitsZeroValues(t *testing.T) {
	got := EntryUpdate{}.form()
	if len(got) != 0 {
		t.Errorf("form() for a zero-value EntryUpdate = %v, want empty", got)
	}

	published := time.Date(2019, 3, 14, 9, 26, 53, 0, time.UTC)
	got = EntryUpdate{PublishedAt: published}.form()
	want := strconv.FormatInt(published.Unix(), 10)
	if got.Get("published_at") != want {
		t.Errorf("published_at = %q, want %q (Unix seconds)", got.Get("published_at"), want)
	}
	if len(got) != 1 {
		t.Errorf("form() with only PublishedAt set = %v, want exactly one field", got)
	}
}
