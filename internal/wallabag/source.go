package wallabag

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Tevqoon/increader/internal/source"
)

// Source adapts a Client to the source.Source interface, which is what the
// syncer consumes. Keeping the adapter separate from the API client means the
// client stays a plain, reusable wallabag library.
type Source struct {
	client *Client
}

// Compile-time proof that *Source satisfies source.Source.
//
// Go note: this declares a discarded variable of the interface type and assigns
// a typed nil pointer to it. It costs nothing at runtime but turns "forgot a
// method" into a compile error here, rather than an error at the distant call
// site that tries to use the type as a Source.
var _ source.Source = (*Source)(nil)

// NewSource wraps a client as a content source.
func NewSource(client *Client) *Source {
	return &Source{client: client}
}

// Name identifies this provider in the database and logs.
func (s *Source) Name() string { return "wallabag" }

// Fetch returns entries updated at or after since, without article bodies.
//
// Bodies are deliberately left out: pulling full HTML for a whole library on
// every sync is slow and mostly wasted, since only articles that reach the top
// of the reading queue are ever read. Content fills them in on demand.
func (s *Source) Fetch(ctx context.Context, since time.Time) ([]source.Document, error) {
	entries, err := s.client.AllEntries(ctx, ListOptions{
		Since:  since,
		Detail: DetailMetadata,
	})
	if err != nil {
		return nil, err
	}

	documents := make([]source.Document, 0, len(entries))
	for _, entry := range entries {
		documents = append(documents, toDocument(entry))
	}
	return documents, nil
}

// Content fetches one article's HTML body.
func (s *Source) Content(ctx context.Context, externalID string) (string, error) {
	id, err := strconv.Atoi(externalID)
	if err != nil {
		return "", fmt.Errorf("wallabag: %q is not a valid entry id: %w", externalID, err)
	}
	entry, err := s.client.EntryByID(ctx, id)
	if err != nil {
		return "", err
	}
	return entry.Content, nil
}

// FullDocument fetches one entry as a Document, body and annotations included.
//
// This is beyond the source.Source interface because annotations only arrive
// with a full entry fetch, and the annotation importer needs them.
func (s *Source) FullDocument(ctx context.Context, externalID string) (source.Document, error) {
	id, err := strconv.Atoi(externalID)
	if err != nil {
		return source.Document{}, fmt.Errorf("wallabag: %q is not a valid entry id: %w", externalID, err)
	}
	entry, err := s.client.EntryByID(ctx, id)
	if err != nil {
		return source.Document{}, err
	}
	return toDocument(entry), nil
}

// toDocument maps a wallabag entry onto the provider-neutral Document shape.
func toDocument(entry Entry) source.Document {
	// wallabag records both the URL it fetched and the URL the user supplied;
	// the former is canonical, but it is empty for some entries.
	link := entry.URL
	if link == "" {
		link = entry.GivenURL
	}

	title := entry.Title
	if title == "" {
		title = link
	}

	highlights := make([]source.Highlight, 0, len(entry.Annotations))
	for _, annotation := range entry.Annotations {
		highlights = append(highlights, source.Highlight{
			ExternalID: strconv.Itoa(annotation.ID),
			Quote:      annotation.Quote,
			Note:       annotation.Text,
			CreatedAt:  annotation.CreatedAt.Time,
			UpdatedAt:  annotation.UpdatedAt.Time,
		})
	}

	return source.Document{
		ExternalID:  strconv.Itoa(entry.ID),
		URL:         link,
		Title:       title,
		Author:      entry.Author(),
		Language:    entry.Language,
		ContentHTML: entry.Content,
		Highlights:  highlights,
		PublishedAt: entry.PublishedAt.Time,
		UpdatedAt:   entry.UpdatedAt.Time,
	}
}
