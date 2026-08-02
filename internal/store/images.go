package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DocumentImage is one image fetched from an article's content, cached so it
// is never fetched from its original host more than once — see
// migrations/009_document_images.sql.
type DocumentImage struct {
	ID          int64
	DocumentID  int64
	URL         string
	ContentType string
	Data        []byte

	// Width and Height are the image's pixel dimensions, measured from its
	// header when it was fetched — see migrations/011_image_dimensions.sql.
	// Either one being 0 means "unknown", not "zero pixels": that is what a
	// row written before that migration has, and it is also what a format
	// the fetcher's decoder does not recognise (SVG, AVIF) gets. Callers
	// must treat 0 that way — see ir.RenderOptions and renderImage, which
	// fall back to emitting no width/height attributes at all rather than
	// writing zeros into the HTML.
	Width, Height int

	// OK is false for a cached failure: the fetch was attempted and did not
	// succeed, recorded so it is not retried on every render of the article.
	OK bool
}

// CachedImage looks up an already-resolved image by its document and
// original URL. The second return distinguishes "never attempted" from a
// cached failure (OK false, found true) — only the former is worth fetching.
func (s *Store) CachedImage(documentID int64, url string) (DocumentImage, bool, error) {
	image, found, err := s.imageByQuery(
		`SELECT id, document_id, url, content_type, data, width, height, ok
		 FROM document_images WHERE document_id = ? AND url = ?`,
		documentID, url,
	)
	if err != nil {
		return DocumentImage{}, false, fmt.Errorf("store: cached image %q for document %d: %w", url, documentID, err)
	}
	return image, found, nil
}

// DocumentImageByID reads one cached image by its row id, for serving it
// back over HTTP.
func (s *Store) DocumentImageByID(id int64) (DocumentImage, error) {
	image, found, err := s.imageByQuery(
		`SELECT id, document_id, url, content_type, data, width, height, ok
		 FROM document_images WHERE id = ?`,
		id,
	)
	if err != nil {
		return DocumentImage{}, fmt.Errorf("store: image %d: %w", id, err)
	}
	if !found {
		return DocumentImage{}, fmt.Errorf("store: image %d: %w", id, ErrNotFound)
	}
	return image, nil
}

func (s *Store) imageByQuery(query string, args ...any) (DocumentImage, bool, error) {
	var (
		image DocumentImage
		ok    int
	)
	err := s.db.QueryRow(query, args...).Scan(
		&image.ID, &image.DocumentID, &image.URL, &image.ContentType, &image.Data,
		&image.Width, &image.Height, &ok,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DocumentImage{}, false, nil
	}
	if err != nil {
		return DocumentImage{}, false, err
	}
	image.OK = ok == 1
	return image, true, nil
}

// SaveDocumentImage records a fetch outcome, success or failure alike (see
// DocumentImage.OK), and returns the row's id — which becomes the local URL
// an article's <img src> is rewritten to point at instead of the original
// host.
//
// width and height are the caller's best measurement of the image's pixel
// size — 0 for either means "unknown, treat as before this existed" — see
// DocumentImage.Width.
func (s *Store) SaveDocumentImage(documentID int64, url, contentType string, data []byte, ok bool, width, height int, now time.Time) (int64, error) {
	okValue := 0
	if ok {
		okValue = 1
	}
	if data == nil {
		// database/sql binds a nil []byte as SQL NULL, not an empty blob,
		// which the NOT NULL column rejects — a failed fetch has no bytes to
		// store, but it still needs to leave a queryable row behind so it is
		// recognised as "already tried" next time, not "never attempted".
		data = []byte{}
	}

	_, err := s.db.Exec(`
		INSERT INTO document_images (document_id, url, content_type, data, width, height, ok, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (document_id, url) DO UPDATE SET
		    content_type = excluded.content_type,
		    data = excluded.data,
		    width = excluded.width,
		    height = excluded.height,
		    ok = excluded.ok,
		    fetched_at = excluded.fetched_at`,
		documentID, url, contentType, data, width, height, okValue, formatTime(now),
	)
	if err != nil {
		return 0, fmt.Errorf("store: save image %q for document %d: %w", url, documentID, err)
	}

	// A plain LastInsertId does not reliably follow the UPDATE branch of an
	// upsert, so the id is read back rather than trusted from the Exec result.
	var id int64
	if err := s.db.QueryRow(
		`SELECT id FROM document_images WHERE document_id = ? AND url = ?`,
		documentID, url,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: read back image %q for document %d: %w", url, documentID, err)
	}
	return id, nil
}

// SetDocumentImageDimensions records a size measured after the row was
// already cached — see resolveOneImage's backfill path in the web package,
// for a row cached before migrations/011_image_dimensions.sql existed and
// so never got a measurement.
//
// This is deliberately its own statement rather than a call back through
// SaveDocumentImage: that upsert rewrites content_type, data, ok and
// fetched_at along with the row it touches, and fetched_at in particular
// must keep meaning "when the bytes were fetched" — reusing it here would
// make a plain later measurement look like a brand new fetch that never
// happened.
func (s *Store) SetDocumentImageDimensions(id int64, width, height int) error {
	if _, err := s.db.Exec(
		`UPDATE document_images SET width = ?, height = ? WHERE id = ?`,
		width, height, id,
	); err != nil {
		return fmt.Errorf("store: set dimensions for image %d: %w", id, err)
	}
	return nil
}
