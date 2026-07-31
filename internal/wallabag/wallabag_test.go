package wallabag

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTimeUnmarshal pins the timestamp format. wallabag emits an offset without
// a colon, so time.RFC3339 rejects it — a mistake that would break every record
// rather than an obvious few, which is why it gets its own test.
func TestTimeUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Time
		wantErr bool
	}{
		{
			name:  "wallabag offset without colon",
			input: `"2026-07-18T00:11:52+0200"`,
			want:  time.Date(2026, 7, 18, 0, 11, 52, 0, time.FixedZone("", 2*60*60)),
		},
		{
			name:  "utc",
			input: `"2026-01-02T15:04:05+0000"`,
			want:  time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC),
		},
		{
			name:  "null is the zero time, not an error",
			input: `null`,
			want:  time.Time{},
		},
		{
			name:  "empty string is the zero time",
			input: `""`,
			want:  time.Time{},
		},
		{
			name:    "rfc3339 with a colon is not wallabag's format",
			input:   `"2026-07-18T00:11:52+02:00"`,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got Time
			err := json.Unmarshal([]byte(test.input), &got)

			if test.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", got.Time)
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !got.Equal(test.want) {
				t.Errorf("got %v, want %v", got.Time, test.want)
			}
		})
	}
}

// fakeServer stands in for wallabag. It records what the client asked for so
// the test can assert on the request, not just the response.
type fakeServer struct {
	*httptest.Server

	tokenRequests  int
	entryQueries   []string
	entriesPerPage int
	totalEntries   int
}

func newFakeServer(t *testing.T, totalEntries, perPage int) *fakeServer {
	t.Helper()

	fake := &fakeServer{entriesPerPage: perPage, totalEntries: totalEntries}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		fake.tokenRequests++
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		// Reject anything but the two grants the client is supposed to send.
		switch r.Form.Get("grant_type") {
		case "password", "refresh_token":
		default:
			http.Error(w, "unsupported grant", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "test-access-token",
			RefreshToken: "test-refresh-token",
			ExpiresIn:    3600,
			TokenType:    "bearer",
		})
	})

	mux.HandleFunc("GET /api/entries.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fake.entryQueries = append(fake.entryQueries, r.URL.RawQuery)

		page := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		pages := (fake.totalEntries + fake.entriesPerPage - 1) / fake.entriesPerPage

		var items []Entry
		start := (page - 1) * fake.entriesPerPage
		for i := start; i < start+fake.entriesPerPage && i < fake.totalEntries; i++ {
			items = append(items, Entry{
				ID:          i + 1,
				Title:       fmt.Sprintf("Article %d", i+1),
				URL:         fmt.Sprintf("https://example.com/%d", i+1),
				PublishedBy: []string{"Some Author"},
				UpdatedAt:   Time{time.Date(2026, 7, 18, 0, 0, i, 0, time.UTC)},
			})
		}

		var envelope entryPage
		envelope.Page = page
		envelope.Pages = pages
		envelope.Total = fake.totalEntries
		envelope.Limit = fake.entriesPerPage
		envelope.Embedded.Items = items
		json.NewEncoder(w).Encode(envelope)
	})

	fake.Server = httptest.NewServer(mux)
	t.Cleanup(fake.Close)
	return fake
}

func testClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	client, err := New(Config{
		URL:          serverURL,
		ClientID:     "id",
		ClientSecret: "secret",
		Username:     "user",
		Password:     "pass",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestAllEntriesWalksEveryPage(t *testing.T) {
	fake := newFakeServer(t, 250, 100)
	client := testClient(t, fake.URL)

	entries, err := client.AllEntries(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("AllEntries: %v", err)
	}

	if len(entries) != 250 {
		t.Errorf("got %d entries, want 250", len(entries))
	}
	if len(fake.entryQueries) != 3 {
		t.Errorf("got %d page requests, want 3", len(fake.entryQueries))
	}
	// The token should be fetched once and reused, not re-fetched per page.
	if fake.tokenRequests != 1 {
		t.Errorf("got %d token requests, want 1", fake.tokenRequests)
	}
}

func TestAllEntriesSendsSinceAndStableOrdering(t *testing.T) {
	fake := newFakeServer(t, 1, 100)
	client := testClient(t, fake.URL)

	since := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := client.AllEntries(context.Background(), ListOptions{Since: since}); err != nil {
		t.Fatalf("AllEntries: %v", err)
	}

	query := fake.entryQueries[0]
	for _, want := range []string{
		fmt.Sprintf("since=%d", since.Unix()),
		"detail=metadata",
		// Ascending update order is what keeps paging stable while `since`
		// filters a moving set; see AllEntries.
		"sort=updated",
		"order=asc",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %q", query, want)
		}
	}
}

// TestReauthenticatesOnExpiredToken checks the retry path: a token that the
// server rejects should trigger one re-authentication and one retry, not a
// failed sync.
func TestReauthenticatesOnExpiredToken(t *testing.T) {
	var (
		tokenRequests int
		entryRequests int
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		tokenRequests++
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: fmt.Sprintf("token-%d", tokenRequests),
			ExpiresIn:   3600,
		})
	})
	mux.HandleFunc("GET /api/entries.json", func(w http.ResponseWriter, r *http.Request) {
		entryRequests++
		// Reject the first token, accept the second.
		if r.Header.Get("Authorization") != "Bearer token-2" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		var envelope entryPage
		envelope.Page, envelope.Pages = 1, 1
		json.NewEncoder(w).Encode(envelope)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := testClient(t, server.URL)
	if _, err := client.AllEntries(context.Background(), ListOptions{}); err != nil {
		t.Fatalf("AllEntries: %v", err)
	}

	if entryRequests != 2 {
		t.Errorf("got %d entry requests, want 2 (one rejected, one retried)", entryRequests)
	}
	if tokenRequests != 2 {
		t.Errorf("got %d token requests, want 2 (initial plus refresh)", tokenRequests)
	}
}

func TestToDocumentMapsEntry(t *testing.T) {
	updated := time.Date(2026, 7, 18, 0, 11, 52, 0, time.UTC)
	entry := Entry{
		ID:          97418,
		Title:       "Are we becoming a post-literate society?",
		URL:         "https://www.ft.com/content/e2ddd496",
		PublishedBy: []string{"Someone", "Ignored"},
		Language:    "en",
		UpdatedAt:   Time{updated},
		Annotations: []Annotation{{
			ID:    1,
			Quote: "Thirty per cent of Americans read at a level...",
			Text:  "a note",
		}},
	}

	document := toDocument(entry)

	if document.ExternalID != "97418" {
		t.Errorf("ExternalID = %q, want \"97418\"", document.ExternalID)
	}
	if document.Author != "Someone" {
		t.Errorf("Author = %q, want \"Someone\"", document.Author)
	}
	if !document.UpdatedAt.Equal(updated) {
		t.Errorf("UpdatedAt = %v, want %v", document.UpdatedAt, updated)
	}
	if len(document.Highlights) != 1 {
		t.Fatalf("got %d highlights, want 1", len(document.Highlights))
	}
	if document.Highlights[0].ExternalID != "1" {
		t.Errorf("highlight ExternalID = %q, want \"1\"", document.Highlights[0].ExternalID)
	}
}

// TestToDocumentFallsBackToGivenURL covers entries wallabag could not fetch,
// where URL is empty but the user's original link survives.
func TestToDocumentFallsBackToGivenURL(t *testing.T) {
	document := toDocument(Entry{ID: 5, GivenURL: "https://example.com/original"})

	if document.URL != "https://example.com/original" {
		t.Errorf("URL = %q, want the given_url", document.URL)
	}
	if document.Title != "https://example.com/original" {
		t.Errorf("Title = %q, want the URL as a fallback", document.Title)
	}
}

// TestDeleteHighlight covers the annotation-delete path added for extract
// deletion: the endpoint addresses an annotation by its own id, not the entry
// it sits on, and the fake server here asserts exactly that — a request for
// the wrong resource would otherwise pass unnoticed.
func TestDeleteHighlight(t *testing.T) {
	var requestedPath string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "tok", ExpiresIn: 3600})
	})
	// net/http's pattern syntax cannot express a wildcard segment followed by a
	// literal suffix ("{id}.json" is rejected at registration), so the fixed
	// path for the one id under test is matched directly instead.
	mux.HandleFunc("DELETE /api/annotations/97418.json", func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{"id": 97418})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := testClient(t, server.URL)
	adapter := NewSource(client)

	if err := adapter.DeleteHighlight(context.Background(), "97418"); err != nil {
		t.Fatalf("DeleteHighlight: %v", err)
	}
	if requestedPath != "/api/annotations/97418.json" {
		t.Errorf("requested %q, want the annotation's own path", requestedPath)
	}
}

func TestDeleteHighlightRejectsNonNumericID(t *testing.T) {
	client := testClient(t, "https://example.invalid")
	adapter := NewSource(client)

	if err := adapter.DeleteHighlight(context.Background(), "not-a-number"); err == nil {
		t.Fatal("expected an error for a non-numeric annotation id")
	}
}
