// Package source defines what a provider of readable content looks like.
//
// This package is deliberately a leaf: it imports nothing outside the standard
// library and knows nothing about wallabag, SQLite, or incremental reading.
// Every content provider — wallabag today, KOReader or Zotero later — produces
// the Document values defined here, so adding a provider never requires
// touching the reading core.
package source

import (
	"context"
	"time"
)

// Source is a provider of readable documents.
//
// Go note: interface satisfaction in Go is structural and implicit. There is no
// "implements" keyword and no registration step — a type satisfies Source
// purely by having matching methods, checked at compile time. Implementations
// do import this package, but only for the Document type they return, not to
// declare a relationship.
type Source interface {
	// Name identifies the provider in storage and logs, e.g. "wallabag".
	// It is used as a database key, so it must be stable across releases.
	Name() string

	// Fetch returns documents changed at or after since. A zero since means
	// "everything". Documents may arrive without ContentHTML if the provider
	// can list more cheaply than it can fetch bodies; callers then use Content
	// to retrieve a body on demand.
	Fetch(ctx context.Context, since time.Time) ([]Document, error)

	// Content returns the full HTML body of a single document.
	Content(ctx context.Context, externalID string) (string, error)
}

// Document is one importable piece of content, normalised across providers.
type Document struct {
	// ExternalID is the provider's own identifier. Combined with the source
	// name it forms the unique key for a document, so re-importing updates
	// rather than duplicates.
	ExternalID string

	URL      string
	Title    string
	Author   string
	Language string

	// ContentHTML is the article body. It may be empty when the document came
	// from a cheap metadata-only listing; it is untrusted and must be
	// sanitised before it is ever rendered.
	ContentHTML string

	// Highlights are annotations that already existed in the provider. They
	// seed the reading queue with passages the reader has already marked.
	Highlights []Highlight

	// PublishedAt is the original publication time, zero when unknown.
	PublishedAt time.Time

	// UpdatedAt is when the provider last changed this document. It drives the
	// incremental-sync watermark, so it must come from the provider rather
	// than from local clock time.
	UpdatedAt time.Time
}

// Highlight is a passage the reader marked in the provider's own interface.
type Highlight struct {
	// ExternalID is the provider's annotation identifier, used to avoid
	// importing the same highlight twice.
	ExternalID string

	// Quote is the highlighted text itself.
	Quote string

	// Note is the reader's comment on the passage, usually empty.
	Note string

	CreatedAt time.Time
	UpdatedAt time.Time
}
