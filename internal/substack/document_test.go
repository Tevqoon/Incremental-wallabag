package substack

import (
	"testing"
	"time"
)

// TestToDocument pins toDocument's field mapping against
// internal/source/source.go's own field semantics.
func TestToDocument(t *testing.T) {
	published := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	p := postBody{
		ID:           4242,
		Slug:         "a-post",
		CanonicalURL: "https://example.substack.com/p/a-post",
		Title:        "A Post Title",
		Language:     "en",
		PostDate:     published,
		PublishedBylines: []byline{
			{Name: "First Author"},
			{Name: "Second Author"},
		},
	}

	doc := toDocument(p, "<p>cleaned body</p>")

	if doc.ExternalID != "4242" {
		t.Errorf("ExternalID = %q, want %q", doc.ExternalID, "4242")
	}
	if doc.URL != p.CanonicalURL {
		t.Errorf("URL = %q, want %q", doc.URL, p.CanonicalURL)
	}
	if doc.Title != p.Title {
		t.Errorf("Title = %q, want %q", doc.Title, p.Title)
	}
	if doc.Author != "First Author" {
		t.Errorf("Author = %q, want %q (the first byline)", doc.Author, "First Author")
	}
	if doc.Language != "en" {
		t.Errorf("Language = %q, want %q", doc.Language, "en")
	}
	if doc.ContentHTML != "<p>cleaned body</p>" {
		t.Errorf("ContentHTML = %q, want the cleaned body passed in", doc.ContentHTML)
	}
	if !doc.PublishedAt.Equal(published) {
		t.Errorf("PublishedAt = %v, want %v", doc.PublishedAt, published)
	}
	if !doc.UpdatedAt.Equal(published) {
		t.Errorf("UpdatedAt = %v, want %v (post_date is the only timestamp Substack offers)", doc.UpdatedAt, published)
	}
}

// TestToDocumentNoBylines checks the empty-bylines case does not panic and
// simply leaves Author blank, since not every post fixture in the wild is
// guaranteed to carry one (a co-authored or anonymously-posted piece,
// possibly).
func TestToDocumentNoBylines(t *testing.T) {
	doc := toDocument(postBody{ID: 1}, "")
	if doc.Author != "" {
		t.Errorf("Author = %q, want empty", doc.Author)
	}
}
