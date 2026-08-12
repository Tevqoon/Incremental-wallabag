package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/source"
)

// SourceUpload is the source name every uploaded annotation file imports
// under.
//
// One name for all three formats rather than one each, so that a book
// annotated on an ereader and the same book annotated in a PDF reader land on
// the same document instead of two — the annotations belong together, and
// which tool produced which is not a distinction worth a separate library.
//
// It is deliberately not registered as a source.Source. There is no server to
// fetch from, nothing to write back to, and Reconcile would mark every
// uploaded document missing upstream the first time it ran.
const SourceUpload = "upload"

// ImportOptions are the reader's choices for one upload.
type ImportOptions struct {
	// DisplayTitle overrides the title the file itself claims. Empty leaves
	// the file's own, which is what KOReader read out of ebook metadata or
	// what a PDF's producer happened to write — frequently not what anyone
	// would call the work.
	DisplayTitle string

	// Subtitle is the reader's own, and has no counterpart in any file.
	Subtitle string

	// IntoDocumentID merges this file into an existing document instead of
	// keying on the file's own derived identity. This is how a work whose
	// exports disagree about its title — the usual case for a book read in
	// one tool and annotated in another — is kept as one thing.
	IntoDocumentID int64

	// Triage parks the annotations out of the queue to be gone through one
	// by one, rather than queueing them outright. See highlightImport.
	Triage bool

	// FloorDays is the fewest days ahead a queued annotation can first become
	// due; SpreadDays is how much further out on top of that it might land,
	// drawn per highlight rather than applied uniformly — see
	// ir.FuzzedAnnotationDelay. Both are ignored when Triage is set.
	FloorDays  int
	SpreadDays int
}

// ImportResult reports what one upload did.
type ImportResult struct {
	DocumentID int64
	RootID     int64

	// Created distinguishes a new work from more annotations on one already
	// stored, which is the only thing a reader wants to know after an upload
	// they expected to be one or the other.
	Created bool

	// Imported counts annotations new to this document. A re-upload of an
	// unchanged file imports none, which is the point.
	Imported int

	// Offered is how many annotations the file contained at all.
	Offered int
}

// ImportAnnotations stores an uploaded annotation file's document.
//
// Separate from UpsertDocuments rather than a flag on it: that is the sync
// path, and it does several things — advancing a watermark, honouring an
// upstream archive flag, replacing the tag set wholesale — that are either
// meaningless or actively wrong for a file someone chose by hand. What the two
// share is the part worth sharing, the row-level helpers underneath.
//
// The root topic is always created suspended. There is no body to read: the
// document exists so that a work's annotations have somewhere to live
// together, and putting a bodyless topic in the reading queue would offer the
// reader a page with nothing on it.
func (s *Store) ImportAnnotations(document source.Document, options ImportOptions, now time.Time) (ImportResult, error) {
	result := ImportResult{Offered: len(document.Highlights)}

	updatedAt := document.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}

	err := s.inTransaction(func(tx *sql.Tx) error {
		var rootID int64

		switch {
		case options.IntoDocumentID > 0:
			// Merging into a document the reader picked. Its identity is
			// theirs to assert, so nothing here second-guesses it — but the
			// file's own title must not overwrite the one already stored,
			// which is very often exactly why they picked it.
			var exists int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM documents WHERE id = ?`,
				options.IntoDocumentID).Scan(&exists); err != nil {
				return fmt.Errorf("store: look up document %d: %w", options.IntoDocumentID, err)
			}
			if exists == 0 {
				return fmt.Errorf("store: document %d: %w", options.IntoDocumentID, ErrNotFound)
			}
			result.DocumentID = options.IntoDocumentID

		default:
			var existingID int64
			err := tx.QueryRow(
				`SELECT id FROM documents WHERE source = ? AND external_id = ?`,
				SourceUpload, document.ExternalID,
			).Scan(&existingID)

			switch {
			case errors.Is(err, sql.ErrNoRows):
				id, err := insertDocument(tx, SourceUpload, document, updatedAt, now)
				if err != nil {
					return err
				}
				if _, err := insertRootTopic(tx, id, document.Title, true, now); err != nil {
					return err
				}
				result.DocumentID, result.Created = id, true

			case err != nil:
				return fmt.Errorf("store: look up upload %s: %w", document.ExternalID, err)

			default:
				result.DocumentID = existingID
				if err := updateDocument(tx, existingID, document, updatedAt); err != nil {
					return err
				}
			}
		}

		if err := setImportTitles(tx, result.DocumentID, options, document.Subtitle); err != nil {
			return err
		}

		rootID, err := rootTopicID(tx, result.DocumentID)
		if err != nil {
			return err
		}
		result.RootID = rootID

		imported, err := insertHighlights(tx, result.DocumentID, rootID, document.Highlights,
			highlightImport{
				floorDays:  options.FloorDays,
				spreadDays: options.SpreadDays,
				suspended:  options.Triage,
				triaged:    !options.Triage,
			}, now)
		if err != nil {
			return err
		}
		result.Imported = imported
		return nil
	})
	if err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

// setImportTitles applies an upload's title override and subtitle.
//
// Each is only written when the upload actually supplied it, so re-uploading
// a file without retyping the title does not silently clear a title that was
// set on an earlier one.
func setImportTitles(tx *sql.Tx, documentID int64, options ImportOptions, fileSubtitle string) error {
	subtitle := options.Subtitle
	if subtitle == "" {
		subtitle = fileSubtitle
	}

	_, err := tx.Exec(`
		UPDATE documents SET
		    display_title = CASE WHEN ? <> '' THEN ? ELSE display_title END,
		    subtitle      = CASE WHEN ? <> '' THEN ? ELSE subtitle      END
		WHERE id = ?`,
		options.DisplayTitle, options.DisplayTitle,
		subtitle, subtitle,
		documentID,
	)
	if err != nil {
		return fmt.Errorf("store: set titles of document %d: %w", documentID, err)
	}
	return nil
}

// UpdateDocumentTitles sets a document's title override and subtitle.
//
// Writes display_title rather than title on purpose. A synced document's
// title is overwritten wholesale by updateDocument on every sync, so an edit
// to the column itself would survive until the next one and then vanish with
// no explanation. Both values are written exactly as given, empty included —
// this is the editing path, and clearing the override to get the file's own
// title back has to be possible.
func (s *Store) UpdateDocumentTitles(id int64, displayTitle, subtitle string) error {
	result, err := s.db.Exec(
		`UPDATE documents SET display_title = ?, subtitle = ? WHERE id = ?`,
		displayTitle, subtitle, id,
	)
	if err != nil {
		return fmt.Errorf("store: update titles of document %d: %w", id, err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("store: document %d: %w", id, ErrNotFound)
	}
	return nil
}

// UpdateDocumentAuthor sets a document's author directly, overwriting
// whatever the import parsed.
//
// Safe only for an uploaded work, and document.html only offers this field
// for one: a wallabag document's author is overwritten wholesale by the next
// sync, the same as its title, and unlike title there is no display_author
// override column protecting an edit from that. An upload has no such sync
// to lose an edit to — the only thing that could overwrite it again is the
// reader re-uploading the same file, a deliberate act rather than a
// background one.
func (s *Store) UpdateDocumentAuthor(id int64, author string) error {
	result, err := s.db.Exec(`UPDATE documents SET author = ? WHERE id = ?`, author, id)
	if err != nil {
		return fmt.Errorf("store: update author of document %d: %w", id, err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("store: document %d: %w", id, ErrNotFound)
	}
	return nil
}

// DocumentAnnotations lists everything harvested from one document, in the
// order it appears in the original.
//
// Reading order, not import order: ordinal comes from the file, so an export
// redone after adding a highlight in the middle of a book puts it in the
// middle here too rather than at the end. Manual extracts have no ordinal and
// sort first, since a passage pulled out while reading has no position in the
// original's own sequence to claim.
func (s *Store) DocumentAnnotations(documentID int64) ([]ExtractRow, error) {
	rows, err := s.db.Query(`
		SELECT `+elementColumns+`, COALESCE(NULLIF(d.display_title, ''), d.title), d.url,
		       (SELECT COUNT(*) FROM cloze_ranges c WHERE c.element_id = e.id)
		FROM elements e
		JOIN documents d ON d.id = e.document_id
		WHERE e.document_id = ? AND e.parent_id IS NOT NULL
		ORDER BY e.ordinal ASC, e.id ASC`,
		documentID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list annotations of document %d: %w", documentID, err)
	}
	defer rows.Close()

	var annotations []ExtractRow
	for rows.Next() {
		var (
			row      ExtractRow
			nullable nullableElement
		)
		targets := append(scanTargets(&row.Element, &nullable),
			&row.DocumentTitle, &row.DocumentURL, &row.ClozeCount)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("store: scan annotation row: %w", err)
		}
		nullable.apply(&row.Element)
		annotations = append(annotations, row)
	}
	return annotations, rows.Err()
}

// RootElement returns a document's root topic — the queue entry standing for
// the document itself.
func (s *Store) RootElement(documentID int64) (Element, error) {
	var (
		element  Element
		nullable nullableElement
	)
	err := s.db.QueryRow(`
		SELECT `+elementColumns+`
		FROM elements e
		WHERE e.document_id = ? AND e.parent_id IS NULL`,
		documentID,
	).Scan(scanTargets(&element, &nullable)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Element{}, fmt.Errorf("store: document %d has no root topic: %w", documentID, ErrNotFound)
	}
	if err != nil {
		return Element{}, fmt.Errorf("store: read root topic of document %d: %w", documentID, err)
	}

	nullable.apply(&element)
	return element, nil
}

// TriageCounts reports how far through a document's triage pass the reader is.
type TriageCounts struct {
	Total     int
	Untriaged int
}

// Done reports whether there is nothing left to decide about.
func (t TriageCounts) Done() bool { return t.Untriaged == 0 }

// CountTriage counts a document's annotations and how many are undecided.
func (s *Store) CountTriage(documentID int64) (TriageCounts, error) {
	var counts TriageCounts
	err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN triaged_at IS NULL THEN 1 ELSE 0 END), 0)
		FROM elements
		WHERE document_id = ? AND parent_id IS NOT NULL`,
		documentID,
	).Scan(&counts.Total, &counts.Untriaged)
	if err != nil {
		return TriageCounts{}, fmt.Errorf("store: count triage for document %d: %w", documentID, err)
	}
	return counts, nil
}

// NextUntriaged returns the next annotation awaiting a decision, in reading
// order, or ErrNotFound when the pass is finished.
//
// Reading order rather than priority order, which is what makes this a
// different thing from the extract queue: going through a book once, front to
// back, is a pass over a work — the chapter you were just in is the context
// for the passage you are looking at now. Priority order would destroy exactly
// that, which is why triage is its own pass and not a filter on the queue.
func (s *Store) NextUntriaged(documentID int64) (Element, error) {
	var (
		element  Element
		nullable nullableElement
	)
	targets := scanTargets(&element, &nullable)

	err := s.db.QueryRow(`
		SELECT `+elementColumns+`
		FROM elements e
		WHERE e.document_id = ? AND e.parent_id IS NOT NULL AND e.triaged_at IS NULL
		ORDER BY e.ordinal ASC, e.id ASC
		LIMIT 1`,
		documentID,
	).Scan(targets...)
	if errors.Is(err, sql.ErrNoRows) {
		return Element{}, fmt.Errorf("store: document %d has nothing left to triage: %w", documentID, ErrNotFound)
	}
	if err != nil {
		return Element{}, fmt.Errorf("store: read next untriaged of document %d: %w", documentID, err)
	}

	nullable.apply(&element)
	return element, nil
}

// MarkTriaged records that an annotation has been decided about.
func (s *Store) MarkTriaged(id int64, now time.Time) error {
	result, err := s.db.Exec(
		`UPDATE elements SET triaged_at = ? WHERE id = ?`,
		formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: mark element %d triaged: %w", id, err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("store: element %d: %w", id, ErrNotFound)
	}
	return nil
}

// KeepTriaged puts an annotation into the reading queue on the given
// schedule and records the decision, in one transaction.
//
// The two belong together: a decision recorded without the schedule change
// silently drops the passage, and a schedule change recorded without the
// decision offers it again on the next pass.
//
// schedule is computed by the caller the same way an ordinary grade or
// backlog button computes one — see triageSchedule in the web package — so
// "keep" offers exactly those choices rather than a single fixed delay. It
// still comes back later rather than today, same as before: the value of a
// passage is re-reading it once its context has faded, not twice in one
// sitting, and triage is that first sitting.
func (s *Store) KeepTriaged(id int64, schedule ir.Schedule, now time.Time) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		if err := saveSchedule(tx, id, schedule, now); err != nil {
			return err
		}
		result, err := tx.Exec(`UPDATE elements SET triaged_at = ? WHERE id = ?`, formatTime(now), id)
		if err != nil {
			return fmt.Errorf("store: mark element %d triaged: %w", id, err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return fmt.Errorf("store: element %d: %w", id, ErrNotFound)
		}
		return nil
	})
}

// SuspendTriaged parks an annotation and records the decision together.
func (s *Store) SuspendTriaged(id int64, now time.Time) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		result, err := tx.Exec(`
			UPDATE elements SET
			    state = ?, due_on = NULL,
			    triaged_at = ?, updated_at = ?
			WHERE id = ?`,
			string(ir.StateSuspended), formatTime(now), formatTime(now), id,
		)
		if err != nil {
			return fmt.Errorf("store: suspend element %d: %w", id, err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return fmt.Errorf("store: element %d: %w", id, ErrNotFound)
		}
		return nil
	})
}

// ResetTriage forgets every decision made about a document's annotations, so
// the pass can be made again.
//
// Only the record of having decided is cleared, never the schedules those
// decisions produced: going through a book a second time is a chance to
// reconsider, not an undo. Anything kept last time stays in the queue until
// this pass says otherwise.
func (s *Store) ResetTriage(documentID int64) error {
	_, err := s.db.Exec(
		`UPDATE elements SET triaged_at = NULL WHERE document_id = ? AND parent_id IS NOT NULL`,
		documentID,
	)
	if err != nil {
		return fmt.Errorf("store: reset triage for document %d: %w", documentID, err)
	}
	return nil
}

// UploadedDocuments lists every document that came from an uploaded file,
// most recent first — the choices for "add these annotations to something
// already here".
func (s *Store) UploadedDocuments() ([]Document, error) {
	rows, err := s.db.Query(`
		SELECT id, title, display_title, subtitle, author
		FROM documents WHERE source = ?
		ORDER BY source_updated_at DESC, id DESC`,
		SourceUpload,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list uploaded documents: %w", err)
	}
	defer rows.Close()

	var documents []Document
	for rows.Next() {
		var document Document
		if err := rows.Scan(&document.ID, &document.Title,
			&document.DisplayTitle, &document.Subtitle, &document.Author); err != nil {
			return nil, fmt.Errorf("store: scan uploaded document: %w", err)
		}
		document.Source = SourceUpload
		documents = append(documents, document)
	}
	return documents, rows.Err()
}
