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
	IsArchived      bool
	IsStarred       bool
	ReadingTime     int
	Tags            []string
	PublishedAt     time.Time
	SourceUpdatedAt time.Time
	ImportedAt      time.Time

	// MissingUpstream is set by Reconcile when a full listing no longer
	// includes this document — the provider's own signal for "deleted" that
	// an incremental sync can never observe, since nothing "changed" about a
	// document that stopped existing.
	MissingUpstream bool
}

// UpsertResult reports what a batch import changed.
type UpsertResult struct {
	Created int
	Updated int

	// Suspended counts articles that wallabag archived since the last sync and
	// which therefore left the reading queue.
	Suspended int

	// Highlights counts provider annotations turned into extracts.
	Highlights int

	// Watermark is the newest SourceUpdatedAt seen, which becomes the `since`
	// value for the next sync.
	Watermark time.Time
}

// UpsertDocuments imports a batch of documents from one source, creating a root
// topic for each new one so it enters the reading queue.
//
// delayDays is how far ahead imported highlights are scheduled. A highlight made
// in another client should join the rotation a little way out rather than
// arriving already due — and dated from the import, not from the provider's own
// timestamp, or a two-year-old highlight would land two years overdue.
//
// Everything happens in a single transaction: a sync that fails halfway should
// leave no partial state, and in particular must not advance the watermark past
// documents that were never written.
func (s *Store) UpsertDocuments(sourceName string, documents []source.Document, delayDays int, now time.Time) (UpsertResult, error) {
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

		var (
			existingID  int64
			wasArchived bool
		)
		err := transaction.QueryRow(
			`SELECT id, is_archived FROM documents WHERE source = ? AND external_id = ?`,
			sourceName, document.ExternalID,
		).Scan(&existingID, &wasArchived)

		var documentID, rootID int64

		switch {
		case errors.Is(err, sql.ErrNoRows):
			documentID, err = insertDocument(transaction, sourceName, document, updatedAt, now)
			if err != nil {
				return result, err
			}
			// An article already archived in wallabag has been read; it belongs
			// in the library, not the reading queue. Creating its root topic
			// suspended reuses the same mechanism as a manual suspension, so
			// "queue this" is just an unsuspend rather than a second concept.
			rootID, err = insertRootTopic(transaction, documentID, document.Title, document.IsArchived, now)
			if err != nil {
				return result, err
			}
			result.Created++

		case err != nil:
			return result, fmt.Errorf("store: look up document %s/%s: %w",
				sourceName, document.ExternalID, err)

		default:
			documentID = existingID
			if err := updateDocument(transaction, existingID, document, updatedAt); err != nil {
				return result, err
			}
			rootID, err = rootTopicID(transaction, existingID)
			if err != nil {
				return result, err
			}
			result.Updated++

			// Archiving in wallabag is an explicit "I am finished with this",
			// so honour it here too. Only on the transition, and never in
			// reverse: unsuspending is the reader's decision and a later sync
			// must not undo it, or the app ends up arguing with them.
			if document.IsArchived && !wasArchived {
				if err := suspendIfActive(transaction, rootID, now); err != nil {
					return result, err
				}
				result.Suspended++
			}
		}

		if err := setDocumentTags(transaction, sourceName, documentID, document.Tags); err != nil {
			return result, err
		}

		imported, err := insertHighlights(transaction, documentID, rootID, document.Highlights, delayDays, now)
		if err != nil {
			return result, err
		}
		result.Highlights += imported

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
		     content_html, has_content, is_archived, is_starred, reading_time,
		     published_at, source_updated_at, imported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sourceName, document.ExternalID, document.URL, document.Title,
		document.Author, document.Language, document.ContentHTML, hasContent,
		document.IsArchived, document.IsStarred, document.ReadingTime,
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
		    is_archived = ?, is_starred = ?, reading_time = ?,
		    published_at = ?, source_updated_at = ?,
		    content_html = CASE WHEN ? <> '' THEN ? ELSE content_html END,
		    has_content  = CASE WHEN ? <> '' THEN 1  ELSE has_content  END
		WHERE id = ?`,
		document.URL, document.Title, document.Author, document.Language,
		document.IsArchived, document.IsStarred, document.ReadingTime,
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
		       content_html, has_content, is_archived, is_starred, reading_time,
		       published_at, source_updated_at, imported_at, missing_upstream
		FROM documents WHERE id = ?`, id,
	).Scan(
		&document.ID, &document.Source, &document.ExternalID, &document.URL,
		&document.Title, &document.Author, &document.Language,
		&document.ContentHTML, &document.HasContent,
		&document.IsArchived, &document.IsStarred, &document.ReadingTime,
		&published, &updated, &imported, &document.MissingUpstream,
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

// ReconcileMissing flags documents whose external_id is absent from present
// as missing upstream, and clears the flag on any that have reappeared in it.
//
// present must be every external_id a full listing of the source currently
// reports — not an incremental "changed since" one. The distinction matters:
// an entry that was deleted at the provider produces no "changed" event for
// an incremental fetch to see, so only a full listing can reveal its absence.
//
// present is loaded into a temporary table rather than an IN (...) list built
// from placeholders: a personal library can run past SQLite's default bound
// parameter limit, and a temp table has no such ceiling.
func (s *Store) ReconcileMissing(sourceName string, present []string) (marked, cleared int, err error) {
	err = s.inTransaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TEMP TABLE present_ids (external_id TEXT PRIMARY KEY)`); err != nil {
			return fmt.Errorf("store: create temp table: %w", err)
		}
		defer tx.Exec(`DROP TABLE present_ids`)

		insert, err := tx.Prepare(`INSERT OR IGNORE INTO present_ids VALUES (?)`)
		if err != nil {
			return fmt.Errorf("store: prepare temp insert: %w", err)
		}
		defer insert.Close()
		for _, externalID := range present {
			if _, err := insert.Exec(externalID); err != nil {
				return fmt.Errorf("store: populate temp table: %w", err)
			}
		}

		result, err := tx.Exec(`
			UPDATE documents SET missing_upstream = 1
			WHERE source = ? AND missing_upstream = 0
			  AND external_id NOT IN (SELECT external_id FROM present_ids)`,
			sourceName)
		if err != nil {
			return fmt.Errorf("store: mark missing documents: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil {
			marked = int(n)
		}

		result, err = tx.Exec(`
			UPDATE documents SET missing_upstream = 0
			WHERE source = ? AND missing_upstream = 1
			  AND external_id IN (SELECT external_id FROM present_ids)`,
			sourceName)
		if err != nil {
			return fmt.Errorf("store: clear missing documents: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil {
			cleared = int(n)
		}
		return nil
	})
	return marked, cleared, err
}

// DeleteDocument permanently removes a document and everything under it — its
// root topic, every extract and cloze taken from it, its tags — via the
// schema's ON DELETE CASCADE.
//
// Meant for a document ReconcileMissing has already flagged gone upstream:
// increader has no way to keep a local copy in step with a provider that no
// longer has the original, so the only choices left are keeping a permanent
// orphan or removing it — and that is the reader's call, never automatic,
// which is why nothing in the sync path calls this on its own.
func (s *Store) DeleteDocument(id int64) error {
	result, err := s.db.Exec(`DELETE FROM documents WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete document %d: %w", id, err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("store: document %d: %w", id, ErrNotFound)
	}
	return nil
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
	DueOn         time.Time
	ExtractCount  int
}

// LibraryFilter selects which documents the library lists.
type LibraryFilter struct {
	Query string

	// State is "", "unread", "starred", "archived", "annotated", "missing",
	// "scheduled", "suspended" or "done" — the first four are the same
	// divisions wallabag's own sidebar offers, so the two read the same way.
	// The rest are increader's own: "missing" is documents the last
	// reconciliation could not find upstream any more; "scheduled" is
	// articles due later than today, sorted furthest out first. That is a
	// due date, not a state — Backlog deliberately leaves state alone (it is
	// not a grade), so a never-graded article pushed out by a preset button
	// is "scheduled" too, same as one grown out by ordinary reading.
	// Distinct from "archived" (wallabag's own "already read", due_on
	// cleared) and from a still-untouched import (due today, not later) —
	// so a reader can spot something they pushed out further than they
	// meant to.
	//
	// "suspended" and "done" read the root topic's own schedule state
	// rather than the document's wallabag flags: "archived" is everything
	// wallabag considers read, whether or not increader has ever looked at
	// it, while "suspended" is the parked backlog still waiting to be
	// gone through here — most of it arriving suspended straight from a
	// wallabag archive import — and "done" is what has actually been
	// worked through and graded in increader itself. The distinction is
	// what makes "suspended" useful as a rediscovery queue: browsing it and
	// pushing an old archive to "done" is a visible measure of progress
	// that "archived" alone cannot show.
	State string

	// Tag restricts to documents carrying this label.
	Tag string

	Limit int
}

// SearchDocuments lists documents matching a filter, most recently updated
// first — except filter.State "scheduled", which sorts furthest due date
// first instead; see LibraryFilter.State. An empty filter lists everything.
//
// today decides what "scheduled" means: due later than today, which is
// exactly the articles worth checking whether they drifted out further than
// intended. It is not needed for anything else SearchDocuments does, but
// Queue and CountDue take the same parameter for the same reason — the
// reader's own notion of today, not the server's — so this does too rather
// than reading the clock itself.
func (s *Store) SearchDocuments(filter LibraryFilter, today time.Time) ([]LibraryEntry, error) {
	if filter.Limit <= 0 {
		filter.Limit = 200
	}
	// LIKE with wildcards on both sides cannot use an index, which is fine at
	// personal-library scale and avoids carrying an FTS5 table that would have
	// to be kept in step with every write.
	pattern := "%" + filter.Query + "%"

	rows, err := s.db.Query(`
		SELECT d.id, d.source, d.external_id, d.url, d.title, d.author,
		       d.language, d.has_content, d.is_archived, d.is_starred,
		       d.reading_time, d.published_at, d.source_updated_at,
		       d.missing_upstream,
		       root.id, root.state, root.due_on,
		       (SELECT COUNT(*) FROM elements child WHERE child.parent_id = root.id)
		FROM documents d
		JOIN elements root ON root.document_id = d.id AND root.parent_id IS NULL
		WHERE (? = '' OR d.title LIKE ? OR d.author LIKE ? OR d.url LIKE ?)
		  AND (? = ''
		       OR (? = 'unread'    AND d.is_archived = 0)
		       OR (? = 'starred'   AND d.is_starred  = 1)
		       OR (? = 'archived'  AND d.is_archived = 1)
		       OR (? = 'missing'   AND d.missing_upstream = 1)
		       OR (? = 'scheduled' AND root.due_on > ?)
		       OR (? = 'suspended' AND root.state = 'suspended')
		       OR (? = 'done'      AND root.state = 'done')
		       OR (? = 'annotated' AND EXISTS (
		              SELECT 1 FROM elements child
		              WHERE child.parent_id = root.id AND child.origin = 'import')))
		  AND (? = '' OR EXISTS (
		          SELECT 1 FROM document_tags dt
		          JOIN tags t ON t.id = dt.tag_id
		          WHERE dt.document_id = d.id AND t.label = ?))
		ORDER BY CASE WHEN ? = 'scheduled' THEN root.due_on END DESC,
		         d.source_updated_at DESC
		LIMIT ?`,
		filter.Query, pattern, pattern, pattern,
		filter.State, filter.State, filter.State, filter.State, filter.State,
		filter.State, today.Format(dateFormat),
		filter.State,
		filter.State,
		filter.State,
		filter.Tag, filter.Tag,
		filter.State,
		filter.Limit,
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
			dueOn     sql.NullString
		)
		err := rows.Scan(
			&entry.ID, &entry.Source, &entry.ExternalID, &entry.URL, &entry.Title,
			&entry.Author, &entry.Language, &entry.HasContent,
			&entry.IsArchived, &entry.IsStarred, &entry.ReadingTime,
			&published, &updated, &entry.MissingUpstream,
			&entry.RootElementID, &entry.State, &dueOn, &entry.ExtractCount,
		)
		if err != nil {
			return nil, fmt.Errorf("store: scan library row: %w", err)
		}
		entry.PublishedAt = parseTime(published)
		entry.SourceUpdatedAt = parseTime(updated)
		entry.DueOn = parseDate(dueOn)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Tags are fetched only after the rows above are exhausted and closed.
	//
	// The connection pool is capped at one — SQLite tolerates a single writer —
	// so querying from inside the loop would wait for a connection the loop
	// itself is holding, and deadlock rather than fail. Any per-row query in
	// this package has to come after the iteration, not during it.
	for index := range entries {
		labels, err := s.TagsOf(entries[index].ID)
		if err != nil {
			return nil, err
		}
		entries[index].Tags = labels
	}
	return entries, nil
}

// CountByState returns the library counts behind the filter tabs — the same
// numbers wallabag shows in its own sidebar.
//
// today decides the "scheduled" count the same way SearchDocuments does: due
// later than today, not a state — see LibraryFilter.State.
func (s *Store) CountByState(sourceName string, today time.Time) (map[string]int, error) {
	counts := map[string]int{}
	row := s.db.QueryRow(`
		SELECT
		    COUNT(*),
		    SUM(CASE WHEN is_archived = 0 THEN 1 ELSE 0 END),
		    SUM(CASE WHEN is_starred  = 1 THEN 1 ELSE 0 END),
		    SUM(CASE WHEN is_archived = 1 THEN 1 ELSE 0 END),
		    SUM(CASE WHEN missing_upstream = 1 THEN 1 ELSE 0 END),
		    (SELECT COUNT(DISTINCT document_id) FROM elements WHERE origin = 'import'),
		    (SELECT COUNT(*) FROM elements root
		     WHERE root.parent_id IS NULL AND root.due_on > ?
		       AND root.document_id IN (SELECT id FROM documents WHERE source = ?)),
		    (SELECT COUNT(*) FROM elements root
		     WHERE root.parent_id IS NULL AND root.state = 'suspended'
		       AND root.document_id IN (SELECT id FROM documents WHERE source = ?)),
		    (SELECT COUNT(*) FROM elements root
		     WHERE root.parent_id IS NULL AND root.state = 'done'
		       AND root.document_id IN (SELECT id FROM documents WHERE source = ?))
		FROM documents WHERE source = ?`,
		today.Format(dateFormat), sourceName, sourceName, sourceName, sourceName)

	var all, unread, starred, archived, missing, annotated, scheduled, suspended, done int
	if err := row.Scan(&all, &unread, &starred, &archived, &missing, &annotated, &scheduled, &suspended, &done); err != nil {
		return nil, fmt.Errorf("store: count documents by state: %w", err)
	}
	counts["all"] = all
	counts["unread"] = unread
	counts["starred"] = starred
	counts["archived"] = archived
	counts["missing"] = missing
	counts["annotated"] = annotated
	counts["scheduled"] = scheduled
	counts["suspended"] = suspended
	counts["done"] = done
	return counts, nil
}
