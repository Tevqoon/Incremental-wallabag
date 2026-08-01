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
	"errors"
	"time"
)

// ErrGone reports that a document no longer exists at its provider.
//
// Declared here rather than in a provider package so the syncer can act on it —
// dropping a queued write that can never succeed — without importing any
// specific provider. Providers wrap it with %w.
var ErrGone = errors.New("source: document no longer exists")

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

// Enricher is an optional extension a Source may also satisfy: it returns a
// whole Document — body and highlights together — for one identifier.
//
// It is separate from Source because not every provider can do it cheaply. In
// wallabag's case the listing that drives syncing omits annotations, and only
// a per-entry fetch carries them, so highlights arrive with the article body
// rather than during the sync.
//
// Go note: this is the "optional interface" pattern. A caller holding a Source
// asks whether it also satisfies Enricher with a type assertion —
// `enricher, ok := provider.(Enricher)` — and falls back when it does not. That
// is how Go extends a published interface without breaking implementations
// that predate the extension.
type Enricher interface {
	Source

	// FullDocument returns the document with everything the provider knows
	// about it, including Highlights.
	FullDocument(ctx context.Context, externalID string) (Document, error)
}

// Writer is an optional extension for providers increader can change, not only
// read. It is the counterpart to Enricher, discovered the same way.
//
// Kept separate from Source because writing is a genuinely different
// capability: a KOReader export directory can be read but not written back to,
// and it should not have to grow stub methods that always fail. Callers ask
// with a type assertion and skip the write when the answer is no.
//
// Every method is expressed in provider-neutral terms. RemoveTag in particular
// takes a label rather than an identifier, even though wallabag's endpoint
// needs a numeric tag id — resolving one to the other is the adapter's problem,
// not the caller's.
type Writer interface {
	Source

	SetArchived(ctx context.Context, externalID string, archived bool) error
	SetStarred(ctx context.Context, externalID string, starred bool) error
	AddTags(ctx context.Context, externalID string, labels []string) error
	RemoveTag(ctx context.Context, externalID string, label string) error

	// DeleteHighlight removes one annotation at the provider, identified by its
	// own external id — a Highlight.ExternalID, not a document's. Deleting an
	// extract that came from an imported highlight must remove the highlight
	// upstream too, or the next sync recreates the very extract just deleted.
	DeleteHighlight(ctx context.Context, highlightExternalID string) error

	// CreateHighlight makes a new annotation at the provider on the entry
	// identified by documentExternalID, from a passage extracted locally. It
	// returns the provider's own id for the new annotation, so the caller can
	// record it — a later delete of the same local extract needs it to remove
	// the right thing upstream.
	CreateHighlight(ctx context.Context, documentExternalID, quote string) (string, error)
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

	// IsArchived is the provider's own "already read" flag. Archived material
	// stays in the library but does not belong in a reading queue, so it is
	// what decides whether a newly imported document is queued at all.
	IsArchived bool

	// IsStarred is the provider's "favourite" flag, kept for display.
	IsStarred bool

	// Tags are the provider's labels for this document.
	Tags []string

	// ReadingTime is the provider's estimate in minutes, zero when unknown.
	ReadingTime int

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
