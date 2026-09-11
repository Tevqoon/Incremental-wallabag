package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Tevqoon/increader/internal/source"
)

// Scan statuses. See migration 019 for what each means.
const (
	ScanPending    = "pending"
	ScanProcessing = "processing"
	ScanDone       = "done"
	ScanFailed     = "failed"
)

// PageScan is one uploaded photo of a marked page, on its way to becoming
// passages in the elements table.
type PageScan struct {
	ID, DocumentID int64

	Filename    string
	ContentType string

	Status   string
	Attempts int

	// Error holds the last attempt's failure, cleared on success. Kept
	// rather than dropped on retry so a scan stuck retrying still shows why.
	Error string

	// Result is the vision model's raw JSON, empty until Status is
	// ScanDone. See SetPageScanResult for how a hand-corrected reading of it
	// is saved back.
	Result string

	// Triage is this scan's own copy of the choice ImportOptions.Triage
	// offers an upload: whether the passages this scan eventually
	// contributes to should be parked for triage or queued outright. Carried
	// per scan, not just at upload time, because ApplyScanPassages runs once
	// per finished scan and needs it without the caller re-supplying it —
	// see migration 019.
	Triage bool

	CreatedAt, UpdatedAt time.Time
}

// ScanCounts is how far a document's photos have gotten through the worker.
type ScanCounts struct {
	Pending, Processing, Done, Failed int
}

// Total is every scan counted, regardless of status.
func (c ScanCounts) Total() int {
	return c.Pending + c.Processing + c.Done + c.Failed
}

// Busy reports whether the worker still has something left to do on this
// document — the signal a reader-facing status line polls on.
func (c ScanCounts) Busy() bool {
	return c.Pending+c.Processing > 0
}

// pageScanColumns lists every page_scans column except image, which must
// never be selected by a listing query — see migration 019. Shared by every
// read below so the scan order cannot drift from the query, the same
// discipline elementColumns keeps for elements.
const pageScanColumns = `
	id, document_id, filename, content_type, status, attempts, error, result,
	triage, created_at, updated_at`

// scanPageScan reads one pageScanColumns row. scan is a bound Scan method —
// either an *sql.Row's or an *sql.Rows' — so this one function serves both a
// single lookup and an iteration, the same trick elementColumns' callers use
// via scanTargets.
func scanPageScan(scan func(dest ...any) error) (PageScan, error) {
	var (
		row       PageScan
		createdAt sql.NullString
		updatedAt sql.NullString
	)
	err := scan(
		&row.ID, &row.DocumentID, &row.Filename, &row.ContentType,
		&row.Status, &row.Attempts, &row.Error, &row.Result,
		&row.Triage, &createdAt, &updatedAt,
	)
	if err != nil {
		return PageScan{}, err
	}
	row.CreatedAt = parseTime(createdAt)
	row.UpdatedAt = parseTime(updatedAt)
	return row, nil
}

// CreateScannedBook makes a new uploaded work with no annotations yet — the
// document a batch of page photos is added to before any of them have been
// read. Modelled on ImportAnnotations' own creation path (insertDocument,
// insertRootTopic) rather than duplicating it by hand, so a scanned book
// starts out exactly like an uploaded file would: same source, same
// suspended root topic with nothing to read yet, since the passages that
// will eventually fill it do not exist until the worker has been through
// every photo.
//
// external_id is a random token rather than anything derived from the title:
// unlike a KOReader or PDF export there is no file identity to key on, and
// two books with the same title must not collide the way ImportAnnotations'
// own (source, external_id) uniqueness would otherwise make them.
func (s *Store) CreateScannedBook(title, author, subtitle string, now time.Time) (int64, error) {
	if title == "" {
		return 0, fmt.Errorf("store: create scanned book: title is required")
	}

	externalID, err := scanExternalID()
	if err != nil {
		return 0, err
	}

	document := source.Document{
		ExternalID: externalID,
		Title:      title,
		Author:     author,
		UpdatedAt:  now,
	}

	var id int64
	err = s.inTransaction(func(tx *sql.Tx) error {
		documentID, err := insertDocument(tx, SourceUpload, document, now, now)
		if err != nil {
			return err
		}
		if subtitle != "" {
			if _, err := tx.Exec(`UPDATE documents SET subtitle = ? WHERE id = ?`, subtitle, documentID); err != nil {
				return fmt.Errorf("store: set subtitle of document %d: %w", documentID, err)
			}
		}
		// Always suspended, same as ImportAnnotations: there is nothing to
		// read yet, so putting a bodyless topic in the queue would offer a
		// page with nothing on it.
		if _, err := insertRootTopic(tx, documentID, title, true, now); err != nil {
			return err
		}
		id = documentID
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// scanExternalID returns a fresh random identity for a scanned book, "scan-"
// plus 16 hex characters (8 random bytes).
func scanExternalID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: generate scanned book id: %w", err)
	}
	return "scan-" + hex.EncodeToString(buf), nil
}

// AddPageScan stores one uploaded photo as pending, awaiting a worker.
func (s *Store) AddPageScan(documentID int64, filename, contentType string, image []byte, triage bool, now time.Time) (int64, error) {
	var id int64
	err := s.inTransaction(func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM documents WHERE id = ?`, documentID).Scan(&exists); err != nil {
			return fmt.Errorf("store: look up document %d: %w", documentID, err)
		}
		if exists == 0 {
			return fmt.Errorf("store: document %d: %w", documentID, ErrNotFound)
		}

		outcome, err := tx.Exec(`
			INSERT INTO page_scans
			    (document_id, filename, content_type, image, status, triage, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			documentID, filename, contentType, image, ScanPending, triage,
			formatTime(now), formatTime(now),
		)
		if err != nil {
			return fmt.Errorf("store: add page scan to document %d: %w", documentID, err)
		}
		id, err = outcome.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: read id of new page scan: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ClaimPageScan atomically takes the oldest pending scan and marks it
// processing, for a worker about to send it to the model.
//
// The select-then-conditional-update-then-retry shape defends against two
// workers claiming the same row: if the UPDATE's WHERE ... AND status =
// 'pending' touches zero rows, some other claim already got there first
// between this call's SELECT and its UPDATE, and the loop tries whatever is
// now oldest instead of returning a scan that was never really claimed. (The
// store's connection pool is capped at one connection — see Open — which
// already serializes every transaction against every other; this exists so
// the guarantee holds by construction rather than by an implementation
// detail of how the pool happens to be configured today.)
func (s *Store) ClaimPageScan(now time.Time) (PageScan, []byte, bool, error) {
	var (
		scan  PageScan
		image []byte
		ok    bool
	)
	err := s.inTransaction(func(tx *sql.Tx) error {
		for {
			var id int64
			err := tx.QueryRow(`
				SELECT id FROM page_scans WHERE status = ? ORDER BY id LIMIT 1`,
				ScanPending,
			).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("store: find pending scan: %w", err)
			}

			result, err := tx.Exec(`
				UPDATE page_scans SET status = ?, attempts = attempts + 1, updated_at = ?
				WHERE id = ? AND status = ?`,
				ScanProcessing, formatTime(now), id, ScanPending,
			)
			if err != nil {
				return fmt.Errorf("store: claim scan %d: %w", id, err)
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: count claimed scan rows: %w", err)
			}
			if changed == 0 {
				continue
			}

			row, err := scanPageScan(tx.QueryRow(`SELECT `+pageScanColumns+` FROM page_scans WHERE id = ?`, id).Scan)
			if err != nil {
				return fmt.Errorf("store: read claimed scan %d: %w", id, err)
			}
			if err := tx.QueryRow(`SELECT image FROM page_scans WHERE id = ?`, id).Scan(&image); err != nil {
				return fmt.Errorf("store: read image of scan %d: %w", id, err)
			}
			scan, ok = row, true
			return nil
		}
	})
	if err != nil {
		return PageScan{}, nil, false, err
	}
	return scan, image, ok, nil
}

// CompletePageScan records the model's answer and clears any earlier error.
func (s *Store) CompletePageScan(id int64, result string, now time.Time) error {
	outcome, err := s.db.Exec(`
		UPDATE page_scans SET status = ?, result = ?, error = '', updated_at = ?
		WHERE id = ?`,
		ScanDone, result, formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: complete page scan %d: %w", id, err)
	}
	if n, _ := outcome.RowsAffected(); n == 0 {
		return fmt.Errorf("store: page scan %d: %w", id, ErrNotFound)
	}
	return nil
}

// FailPageScan records a failed attempt. retry puts the scan back to pending
// for another try; otherwise it becomes failed for good, until the reader
// asks for RetryPageScan by hand.
func (s *Store) FailPageScan(id int64, message string, retry bool, now time.Time) error {
	status := ScanFailed
	if retry {
		status = ScanPending
	}
	outcome, err := s.db.Exec(`
		UPDATE page_scans SET status = ?, error = ?, updated_at = ?
		WHERE id = ?`,
		status, message, formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: fail page scan %d: %w", id, err)
	}
	if n, _ := outcome.RowsAffected(); n == 0 {
		return fmt.Errorf("store: page scan %d: %w", id, ErrNotFound)
	}
	return nil
}

// ResetProcessingScans puts every processing scan back to pending. Meant to
// run once at startup: a scan left processing belonged to a worker goroutine
// that no longer exists, since nothing marks one processing except a claim
// this same process made, and a fresh start has made none yet.
func (s *Store) ResetProcessingScans(now time.Time) (int, error) {
	outcome, err := s.db.Exec(`
		UPDATE page_scans SET status = ?, updated_at = ? WHERE status = ?`,
		ScanPending, formatTime(now), ScanProcessing,
	)
	if err != nil {
		return 0, fmt.Errorf("store: reset processing scans: %w", err)
	}
	n, err := outcome.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count reset scans: %w", err)
	}
	return int(n), nil
}

// RetryPageScan asks for a scan to be read again: failed or done, back to
// pending, attempts and error cleared. done is allowed as a source state, not
// just failed, because a re-read is also how the reader asks the model to
// try again on a page it technically finished but misread.
func (s *Store) RetryPageScan(id int64, now time.Time) error {
	outcome, err := s.db.Exec(`
		UPDATE page_scans SET status = ?, attempts = 0, error = '', updated_at = ?
		WHERE id = ? AND status IN (?, ?)`,
		ScanPending, formatTime(now), id, ScanFailed, ScanDone,
	)
	if err != nil {
		return fmt.Errorf("store: retry page scan %d: %w", id, err)
	}
	if n, _ := outcome.RowsAffected(); n == 0 {
		return fmt.Errorf("store: page scan %d: %w", id, ErrNotFound)
	}
	return nil
}

// PageScan reads one scan's metadata, without its image — see
// pageScanColumns.
func (s *Store) PageScan(id int64) (PageScan, error) {
	scan, err := scanPageScan(s.db.QueryRow(`SELECT `+pageScanColumns+` FROM page_scans WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return PageScan{}, fmt.Errorf("store: page scan %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return PageScan{}, fmt.Errorf("store: read page scan %d: %w", id, err)
	}
	return scan, nil
}

// PageScans lists a document's scans in upload order, without their images —
// see pageScanColumns.
func (s *Store) PageScans(documentID int64) ([]PageScan, error) {
	rows, err := s.db.Query(`SELECT `+pageScanColumns+` FROM page_scans WHERE document_id = ? ORDER BY id`, documentID)
	if err != nil {
		return nil, fmt.Errorf("store: list page scans of document %d: %w", documentID, err)
	}
	defer rows.Close()

	var scans []PageScan
	for rows.Next() {
		scan, err := scanPageScan(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scan page scan row: %w", err)
		}
		scans = append(scans, scan)
	}
	return scans, rows.Err()
}

// PageScanImage reads back one photo's bytes — the one query in this file
// allowed to select image, for the one caller that actually needs it (the
// worker, and the reader's own review page).
func (s *Store) PageScanImage(id int64) (documentID int64, contentType string, data []byte, err error) {
	err = s.db.QueryRow(`SELECT document_id, content_type, image FROM page_scans WHERE id = ?`, id).
		Scan(&documentID, &contentType, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil, fmt.Errorf("store: page scan %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return 0, "", nil, fmt.Errorf("store: read image of page scan %d: %w", id, err)
	}
	return documentID, contentType, data, nil
}

// CountPageScans reports how far a document's photos have gotten through the
// worker.
func (s *Store) CountPageScans(documentID int64) (ScanCounts, error) {
	var counts ScanCounts
	err := s.db.QueryRow(`
		SELECT
		    COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
		    COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
		    COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
		    COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0)
		FROM page_scans WHERE document_id = ?`,
		ScanPending, ScanProcessing, ScanDone, ScanFailed, documentID,
	).Scan(&counts.Pending, &counts.Processing, &counts.Done, &counts.Failed)
	if err != nil {
		return ScanCounts{}, fmt.Errorf("store: count page scans of document %d: %w", documentID, err)
	}
	return counts, nil
}

// SetPageScanResult replaces a done scan's stored JSON — how a hand-corrected
// page number (the model misread it, or could not read it at all) is saved,
// without disturbing status, attempts or error.
func (s *Store) SetPageScanResult(id int64, result string, now time.Time) error {
	outcome, err := s.db.Exec(`
		UPDATE page_scans SET result = ?, updated_at = ? WHERE id = ?`,
		result, formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: set result of page scan %d: %w", id, err)
	}
	if n, _ := outcome.RowsAffected(); n == 0 {
		return fmt.Errorf("store: page scan %d: %w", id, ErrNotFound)
	}
	return nil
}

// DocumentsWithPageScans lists every document id that has at least one scan,
// for the worker's own startup re-assembly pass over books it was in the
// middle of.
func (s *Store) DocumentsWithPageScans() ([]int64, error) {
	rows, err := s.db.Query(`SELECT DISTINCT document_id FROM page_scans ORDER BY document_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list documents with page scans: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan document id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DismissNotePending clears note_pending without a note ever being typed in —
// the reader looked at the photo and decided there was nothing worth saving.
func (s *Store) DismissNotePending(id int64) error {
	outcome, err := s.db.Exec(`UPDATE elements SET note_pending = 0 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: dismiss note_pending on element %d: %w", id, err)
	}
	if n, _ := outcome.RowsAffected(); n == 0 {
		return fmt.Errorf("store: element %d: %w", id, ErrNotFound)
	}
	return nil
}

// ScanPassage is one assembled passage, as internal/pagescan produces it from
// a book's finished scans.
//
// Declared here rather than imported from that package so store stays
// independent of it, the same way this package never imports internal/web —
// dependencies only ever point down. The worker, which does depend on both,
// converts pagescan's own type into this one.
type ScanPassage struct {
	// Ref is this passage's stable external_ref, e.g. "scan:p140:m0" — the
	// identity ApplyScanPassages matches an assembled passage against a
	// document's existing elements by.
	Ref string

	// AbsorbedRefs are the refs of continuation fragments merged into this
	// passage — a sentence a page turn split across two photos, reassembled
	// into one passage here. Each one that still exists as its own row and
	// has nothing depending on it is removed; see ApplyScanPassages.
	AbsorbedRefs []string

	// Page is where in the book this passage sits, "140" or a straddled
	// "140–141".
	Page string

	Ordinal int
	Text    string

	// NotePending marks a passage whose photo showed handwriting the model
	// was not asked to read. Only acted on when this passage is first
	// inserted — see ApplyScanPassages rule 3.
	NotePending bool

	// Chapter is only used when inserting a passage that does not exist yet;
	// an existing row's chapter is the reader's to set (see
	// SetAnnotationChapter) and ApplyScanPassages never touches it again.
	Chapter string

	// ScanIDs are the photos this passage was assembled from. A passage is
	// touched by ApplyScanPassages only when the scan that triggered the
	// call is among them — see its doc comment for why.
	ScanIDs []int64
}

// ScanApplyOptions carries the scheduling choices a newly inserted passage
// needs — the same pair ImportOptions offers an upload.
type ScanApplyOptions struct {
	FloorDays  int
	SpreadDays int
}

// ScanApplyResult reports what one ApplyScanPassages call changed.
type ScanApplyResult struct {
	Inserted, Updated, Removed int
}

// ScanElementChapters returns external_ref → chapter for every imported
// element of a document whose external_ref came from a page scan — the
// worker's own way to remember which chapter a passage belonged to across
// an assembly that only ever supplies Chapter for a passage it is inserting
// for the first time (see ScanPassage.Chapter).
func (s *Store) ScanElementChapters(documentID int64) (map[string]string, error) {
	rows, err := s.db.Query(`
		SELECT external_ref, chapter FROM elements
		WHERE document_id = ? AND substr(COALESCE(external_ref, ''), 1, 5) = 'scan:'`,
		documentID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list scan chapters of document %d: %w", documentID, err)
	}
	defer rows.Close()

	chapters := map[string]string{}
	for rows.Next() {
		var ref, chapter string
		if err := rows.Scan(&ref, &chapter); err != nil {
			return nil, fmt.Errorf("store: scan chapter row: %w", err)
		}
		chapters[ref] = chapter
	}
	return chapters, rows.Err()
}

// RenameScanRefs rewrites the external_ref prefix of a document's elements —
// used when the reader types in a page number for a page the model could not
// read: refs built from the scan id ("scan:s12.0:...") become refs built
// from the label ("scan:p94:...") once there is a real one to use.
//
// Matches with substr rather than LIKE: oldPrefix is built from a reader-
// typed label, which may itself contain '_' or '%', both of which LIKE would
// read as wildcards rather than literal characters.
func (s *Store) RenameScanRefs(documentID int64, oldPrefix, newPrefix string, now time.Time) (int, error) {
	prefixLen := len(oldPrefix)
	outcome, err := s.db.Exec(`
		UPDATE elements SET
		    external_ref = ? || substr(external_ref, ?),
		    updated_at = ?
		WHERE document_id = ? AND substr(COALESCE(external_ref, ''), 1, ?) = ?`,
		newPrefix, prefixLen+1,
		formatTime(now),
		documentID, prefixLen, oldPrefix,
	)
	if err != nil {
		return 0, fmt.Errorf("store: rename scan refs of document %d: %w", documentID, err)
	}
	n, err := outcome.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count renamed scan refs of document %d: %w", documentID, err)
	}
	return int(n), nil
}

// ApplyScanPassages folds one finished scan's contribution to a book's
// assembled passages into the elements table.
//
// internal/pagescan re-derives *every* passage of a book from *every* one of
// its scans on each run, because a passage can straddle two photos that
// arrive in different upload batches — there is no way to know a passage is
// finished until the photo after it has been read too. That means most of
// what comes back in passages already exists here, quite possibly edited or
// deleted by the reader since the last run. The rules below exist entirely
// to protect that:
//
//  1. A passage is touched only when fromScan — the scan that just finished
//     and triggered this call — is among its ScanIDs. Every other passage in
//     the slice is left completely alone: no insert, no update. This is what
//     makes deleting a passage sticky (it does not come back just because
//     some *other* photo of the book finished processing) and what protects
//     a manual correction to quote (the annotation editor writes that
//     directly) from being silently overwritten by a re-assembly the reader
//     had nothing to do with.
//  2. A touched passage whose Ref already exists gets its quote, page,
//     ordinal and (only while unanchored) content_html brought in line, and
//     nothing else — never chapter, note, note_pending, edited_quote, or any
//     schedule or triage column. No UPDATE is even issued unless something
//     actually differs, so a re-run with unchanged input is a true no-op.
//  3. A touched passage whose Ref does not exist yet is inserted through the
//     same insertHighlights path an ordinary upload uses, so priority,
//     scheduling fuzz, suspended-for-triage and the "same quote under a new
//     ref" adoption behaviour are all shared rather than reimplemented here.
//  4. Each of a touched passage's AbsorbedRefs that still exists as its own
//     imported element with no children of its own is removed, carrying its
//     note across first if the passage's own row has none yet. A ref that
//     has grown a child (a cloze, an extract someone pulled from it) is left
//     standing rather than deleted out from under it.
//
// fromScan's own triage flag (page_scans.triage) decides suspended-for-
// triage vs. queued-outright for every passage this call inserts.
func (s *Store) ApplyScanPassages(documentID, fromScan int64, passages []ScanPassage, options ScanApplyOptions, now time.Time) (ScanApplyResult, error) {
	var result ScanApplyResult

	err := s.inTransaction(func(tx *sql.Tx) error {
		var triage bool
		err := tx.QueryRow(`SELECT triage FROM page_scans WHERE id = ?`, fromScan).Scan(&triage)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: page scan %d: %w", fromScan, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: read triage flag of scan %d: %w", fromScan, err)
		}

		rootID, err := rootTopicID(tx, documentID)
		if err != nil {
			return err
		}

		for _, passage := range passages {
			if !containsScanID(passage.ScanIDs, fromScan) {
				continue
			}
			if err := applyOneScanPassage(tx, documentID, rootID, triage, options, passage, now, &result); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ScanApplyResult{}, err
	}
	return result, nil
}

// applyOneScanPassage runs rules 2-4 of ApplyScanPassages' doc comment for a
// single passage the caller has already confirmed is touched.
func applyOneScanPassage(tx *sql.Tx, documentID, rootID int64, triage bool,
	options ScanApplyOptions, passage ScanPassage, now time.Time, result *ScanApplyResult) error {

	var (
		id          int64
		quote, page string
		ordinal     int
		note        string
		contentHTML string
		startBlock  sql.NullInt64
	)
	err := tx.QueryRow(`
		SELECT id, quote, page, ordinal, note, content_html, start_block
		FROM elements WHERE document_id = ? AND external_ref = ?`,
		documentID, passage.Ref,
	).Scan(&id, &quote, &page, &ordinal, &note, &contentHTML, &startBlock)

	switch {
	case err == nil:
		changed := quote != passage.Text || page != passage.Page || ordinal != passage.Ordinal
		newContentHTML := contentHTML
		if !startBlock.Valid {
			newContentHTML = annotationHTML(passage.Text, note)
			if newContentHTML != contentHTML {
				changed = true
			}
		}
		if changed {
			if _, err := tx.Exec(`
				UPDATE elements SET
				    quote = ?, page = ?, ordinal = ?, updated_at = ?,
				    content_html = CASE WHEN start_block IS NULL THEN ? ELSE content_html END
				WHERE id = ?`,
				passage.Text, passage.Page, passage.Ordinal, formatTime(now), newContentHTML, id,
			); err != nil {
				return fmt.Errorf("store: update passage %s: %w", passage.Ref, err)
			}
			result.Updated++
		}

	case errors.Is(err, sql.ErrNoRows):
		highlight := source.Highlight{
			ExternalID: passage.Ref,
			Quote:      passage.Text,
			Page:       passage.Page,
			Chapter:    passage.Chapter,
			Ordinal:    passage.Ordinal,
		}
		inserted, err := insertHighlights(tx, documentID, rootID, []source.Highlight{highlight},
			highlightImport{
				floorDays:  options.FloorDays,
				spreadDays: options.SpreadDays,
				suspended:  triage,
				triaged:    !triage,
			}, now)
		if err != nil {
			return err
		}
		result.Inserted += inserted

		if passage.NotePending {
			if _, err := tx.Exec(`
				UPDATE elements SET note_pending = 1
				WHERE document_id = ? AND external_ref = ?`,
				documentID, passage.Ref,
			); err != nil {
				return fmt.Errorf("store: mark note pending for %s: %w", passage.Ref, err)
			}
		}

	default:
		return fmt.Errorf("store: look up passage %s: %w", passage.Ref, err)
	}

	if len(passage.AbsorbedRefs) == 0 {
		return nil
	}

	// The row above may have just been inserted, or adopted from a row that
	// already existed under a different ref (insertHighlights' own "same
	// quote under a new ref" path) — either way its id and note are read
	// fresh here rather than assumed, since an adopted row can carry a note
	// from well before this call ever ran.
	if err := tx.QueryRow(`
		SELECT id, note FROM elements WHERE document_id = ? AND external_ref = ?`,
		documentID, passage.Ref,
	).Scan(&id, &note); err != nil {
		return fmt.Errorf("store: look up passage %s: %w", passage.Ref, err)
	}

	for _, absorbedRef := range passage.AbsorbedRefs {
		removed, err := absorbScanRef(tx, documentID, absorbedRef, passage.Text, note, id, now)
		if err != nil {
			return err
		}
		if removed {
			result.Removed++
		}
	}
	return nil
}

// absorbScanRef removes one continuation fragment the assembler merged into
// a touched passage — a page turn split a sentence across two photos, and
// the second half's own row has done its job once its text is folded into
// the first.
//
// Scoped to origin = 'import': a row the reader has since turned into
// something of their own (adopted a manual extract onto, say) is not this
// assembler's to remove. Scoped to having no children: a cloze or a pulled
// extract hanging off this row means something depends on it continuing to
// exist, so it is left standing even though the passage that grew out of it
// has moved on.
func absorbScanRef(tx *sql.Tx, documentID int64, ref, passageQuote, passageNote string, passageID int64, now time.Time) (bool, error) {
	var (
		id   int64
		note string
	)
	err := tx.QueryRow(`
		SELECT id, note FROM elements
		WHERE document_id = ? AND external_ref = ? AND origin = ?`,
		documentID, ref, OriginImport,
	).Scan(&id, &note)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: look up absorbed ref %s: %w", ref, err)
	}

	var children int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM elements WHERE parent_id = ?`, id).Scan(&children); err != nil {
		return false, fmt.Errorf("store: count children of absorbed ref %s: %w", ref, err)
	}
	if children > 0 {
		return false, nil
	}

	if note != "" && passageNote == "" {
		newContentHTML := annotationHTML(passageQuote, note)
		if _, err := tx.Exec(`
			UPDATE elements SET
			    note = ?, note_pending = 0, updated_at = ?,
			    content_html = CASE WHEN start_block IS NULL THEN ? ELSE content_html END
			WHERE id = ?`,
			note, formatTime(now), newContentHTML, passageID,
		); err != nil {
			return false, fmt.Errorf("store: copy note from absorbed ref %s: %w", ref, err)
		}
	}

	if _, err := tx.Exec(`DELETE FROM elements WHERE id = ?`, id); err != nil {
		return false, fmt.Errorf("store: delete absorbed ref %s: %w", ref, err)
	}
	return true, nil
}

// containsScanID reports whether target is among ids.
func containsScanID(ids []int64, target int64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}
