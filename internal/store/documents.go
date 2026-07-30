package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Tevqoon/increader/internal/source"
)

// ErrNotFound is returned when a lookup by id matches no row.
var ErrNotFound = errors.New("store: not found")

// Document is a stored document, as read back from the database.
type Document struct {
	ID              int64
	Source          string
	ExternalID      string
	URL             string
	Title           string
	Author          string
	Language        string
	ContentHTML     string
	HasContent      bool
	PublishedAt     time.Time
	SourceUpdatedAt time.Time
	ImportedAt      time.Time
}

// UpsertResult reports what a batch import changed.
type UpsertResult struct {
	Created int
	Updated int

	// Watermark is the newest SourceUpdatedAt seen, which becomes the `since`
	// value for the next sync.
	Watermark time.Time
}

// UpsertDocuments imports a batch of documents from one source, creating a root
// topic for each new one so it enters the reading queue.
//
// Everything happens in a single transaction: a sync that fails halfway should
// leave no partial state, and in particular must not advance the watermark past
// documents that were never written.
func (s *Store) UpsertDocuments(sourceName string, documents []source.Document, now time.Time) (UpsertResult, error) {
	var result UpsertResult

	transaction, err := s.db.Begin()
	if err != nil {
		return result, fmt.Errorf("store: begin document import: %w", err)
	}
	// Go note: a deferred Rollback after a successful Commit is a no-op that
	// returns ErrTxDone, so this is the standard way to guarantee cleanup on
	// every early return without tracking whether the commit happened.
	defer transaction.Rollback()

	for _, document := range documents {
		updatedAt := document.UpdatedAt
		if updatedAt.IsZero() {
			// Without a provider timestamp the watermark cannot advance
			// correctly, so fall back to now and accept re-fetching it once.
			updatedAt = now
		}

		var existingID int64
		err := transaction.QueryRow(
			`SELECT id FROM documents WHERE source = ? AND external_id = ?`,
			sourceName, document.ExternalID,
		).Scan(&existingID)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			id, err := insertDocument(transaction, sourceName, document, updatedAt, now)
			if err != nil {
				return result, err
			}
			if err := insertRootTopic(transaction, id, document.Title, now); err != nil {
				return result, err
			}
			result.Created++

		case err != nil:
			return result, fmt.Errorf("store: look up document %s/%s: %w",
				sourceName, document.ExternalID, err)

		default:
			if err := updateDocument(transaction, existingID, document, updatedAt); err != nil {
				return result, err
			}
			result.Updated++
		}

		if updatedAt.After(result.Watermark) {
			result.Watermark = updatedAt
		}
	}

	if err := transaction.Commit(); err != nil {
		return result, fmt.Errorf("store: commit document import: %w", err)
	}
	return result, nil
}

func insertDocument(tx *sql.Tx, sourceName string, document source.Document, updatedAt, now time.Time) (int64, error) {
	hasContent := document.ContentHTML != ""

	outcome, err := tx.Exec(`
		INSERT INTO documents
		    (source, external_id, url, title, author, language,
		     content_html, has_content, published_at, source_updated_at, imported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sourceName, document.ExternalID, document.URL, document.Title,
		document.Author, document.Language, document.ContentHTML, hasContent,
		formatTime(document.PublishedAt), formatTime(updatedAt), formatTime(now),
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert document %s/%s: %w",
			sourceName, document.ExternalID, err)
	}

	id, err := outcome.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: read id of inserted document: %w", err)
	}
	return id, nil
}

func updateDocument(tx *sql.Tx, id int64, document source.Document, updatedAt time.Time) error {
	// content_html is only overwritten when the incoming document actually has
	// a body. Listings are synced with metadata only, so a plain assignment
	// here would wipe the article text of everything already fetched.
	_, err := tx.Exec(`
		UPDATE documents SET
		    url = ?, title = ?, author = ?, language = ?,
		    published_at = ?, source_updated_at = ?,
		    content_html = CASE WHEN ? <> '' THEN ? ELSE content_html END,
		    has_content  = CASE WHEN ? <> '' THEN 1  ELSE has_content  END
		WHERE id = ?`,
		document.URL, document.Title, document.Author, document.Language,
		formatTime(document.PublishedAt), formatTime(updatedAt),
		document.ContentHTML, document.ContentHTML,
		document.ContentHTML,
		id,
	)
	if err != nil {
		return fmt.Errorf("store: update document %d: %w", id, err)
	}
	return nil
}

// SetDocumentContent stores an article body fetched on demand.
func (s *Store) SetDocumentContent(id int64, html string) error {
	_, err := s.db.Exec(
		`UPDATE documents SET content_html = ?, has_content = 1 WHERE id = ?`,
		html, id,
	)
	if err != nil {
		return fmt.Errorf("store: set content of document %d: %w", id, err)
	}
	return nil
}

// DocumentByID reads one document, returning ErrNotFound if it does not exist.
func (s *Store) DocumentByID(id int64) (Document, error) {
	var (
		document  Document
		published sql.NullString
		updated   sql.NullString
		imported  sql.NullString
	)
	err := s.db.QueryRow(`
		SELECT id, source, external_id, url, title, author, language,
		       content_html, has_content, published_at, source_updated_at, imported_at
		FROM documents WHERE id = ?`, id,
	).Scan(
		&document.ID, &document.Source, &document.ExternalID, &document.URL,
		&document.Title, &document.Author, &document.Language,
		&document.ContentHTML, &document.HasContent,
		&published, &updated, &imported,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, fmt.Errorf("store: document %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Document{}, fmt.Errorf("store: read document %d: %w", id, err)
	}

	document.PublishedAt = parseTime(published)
	document.SourceUpdatedAt = parseTime(updated)
	document.ImportedAt = parseTime(imported)
	return document, nil
}

// CountDocuments returns how many documents are stored for a source.
func (s *Store) CountDocuments(sourceName string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM documents WHERE source = ?`, sourceName,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count documents for %s: %w", sourceName, err)
	}
	return count, nil
}

// LibraryEntry is a document together with the id of its root topic, so the
// library can link straight into the reader.
type LibraryEntry struct {
	Document
	RootElementID int64
	State         string
	ExtractCount  int
}

// SearchDocuments lists documents whose title, author or URL matches query,
// most recently updated first. An empty query lists everything.
func (s *Store) SearchDocuments(query string, limit int) ([]LibraryEntry, error) {
	// LIKE with wildcards on both sides cannot use an index, which is fine at
	// personal-library scale and avoids carrying an FTS5 table that would have
	// to be kept in step with every write.
	pattern := "%" + query + "%"

	rows, err := s.db.Query(`
		SELECT d.id, d.source, d.external_id, d.url, d.title, d.author,
		       d.language, d.has_content, d.published_at, d.source_updated_at,
		       root.id, root.state,
		       (SELECT COUNT(*) FROM elements child WHERE child.parent_id = root.id)
		FROM documents d
		JOIN elements root ON root.document_id = d.id AND root.parent_id IS NULL
		WHERE ? = '' OR d.title LIKE ? OR d.author LIKE ? OR d.url LIKE ?
		ORDER BY d.source_updated_at DESC
		LIMIT ?`,
		query, pattern, pattern, pattern, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: search documents: %w", err)
	}
	defer rows.Close()

	var entries []LibraryEntry
	for rows.Next() {
		var (
			entry     LibraryEntry
			published sql.NullString
			updated   sql.NullString
		)
		err := rows.Scan(
			&entry.ID, &entry.Source, &entry.ExternalID, &entry.URL, &entry.Title,
			&entry.Author, &entry.Language, &entry.HasContent, &published, &updated,
			&entry.RootElementID, &entry.State, &entry.ExtractCount,
		)
		if err != nil {
			return nil, fmt.Errorf("store: scan library row: %w", err)
		}
		entry.PublishedAt = parseTime(published)
		entry.SourceUpdatedAt = parseTime(updated)
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
