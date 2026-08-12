package substack

import (
	"strconv"

	"github.com/Tevqoon/increader/internal/source"
)

// toDocument maps one fetched, cleaned post onto the provider-neutral
// source.Document shape — see internal/source/source.go for what each field
// means to the rest of increader.
func toDocument(p postBody, cleaned string) source.Document {
	var author string
	if len(p.PublishedBylines) > 0 {
		author = p.PublishedBylines[0].Name
	}

	return source.Document{
		ExternalID:  strconv.Itoa(p.ID),
		URL:         p.CanonicalURL,
		Title:       p.Title,
		Author:      author,
		Language:    p.Language,
		ContentHTML: cleaned,

		// PublishedAt and UpdatedAt are both set from Substack's own
		// post_date: neither the archive listing nor the per-post endpoint
		// exposes a separate "last edited" timestamp, so post_date is the
		// only time value this package actually has. A caller built on top
		// of this that wants incremental "changed since" behaviour the way
		// wallabag.Source.Fetch offers would need to know that an edited
		// Substack post does not move this forward the way a wallabag
		// entry's own updated_at does on a real edit — but Ingest itself
		// does no such filtering (there is no archive endpoint to filter
		// with; every run walks the whole archive and relies on post's own
		// cache to skip work already done), so nothing in this package
		// currently depends on that distinction.
		PublishedAt: p.PostDate,
		UpdatedAt:   p.PostDate,
	}
}
