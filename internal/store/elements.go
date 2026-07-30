package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
)

// Element kinds.
const (
	KindTopic = "topic"
	KindItem  = "item"
)

// Element origins.
const (
	OriginManual = "manual"
	OriginImport = "import"
)

// Element is one node of the incremental-reading tree: an article, an extract
// taken from one, or a cloze item made from an extract.
type Element struct {
	ID         int64
	DocumentID int64

	// ParentID is 0 for a document's root topic. SQLite rowids start at 1, so
	// zero is unambiguous and saves threading sql.NullInt64 through the
	// handlers.
	ParentID int64

	Kind        string
	Title       string
	ContentHTML string
	Quote       string

	// Range locates an extract inside its parent. Meaningful only when
	// HasRange is set, which is false for root topics.
	Range    ir.Range
	HasRange bool

	// Schedule is embedded rather than flattened so handlers can hand it
	// straight to ir.Next and store the result back.
	Schedule ir.Schedule

	ReadBlock   int
	Origin      string
	ExternalRef string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsRoot reports whether this element is a whole document rather than an
// extract taken from one.
func (e Element) IsRoot() bool { return e.ParentID == 0 }

// QueueItem is an element together with the document it came from, which is
// what the queue view needs to show.
type QueueItem struct {
	Element
	DocumentTitle string
	DocumentURL   string
}

// elementColumns is shared by every read so the scan order cannot drift apart
// from the query.
const elementColumns = `
	e.id, e.document_id, COALESCE(e.parent_id, 0), e.kind, e.title,
	e.content_html, e.quote,
	e.start_block, e.start_offset, e.end_block, e.end_offset,
	e.priority, e.state, e.due_on, e.interval_days, e.afactor, e.reps,
	e.read_block, e.origin, COALESCE(e.external_ref, ''),
	e.created_at, e.updated_at`

// nullableElement holds the columns that can be NULL, which cannot be scanned
// straight into the Element fields they populate.
type nullableElement struct {
	startBlock  sql.NullInt64
	startOffset sql.NullInt64
	endBlock    sql.NullInt64
	endOffset   sql.NullInt64
	dueOn       sql.NullString
	createdAt   sql.NullString
	updatedAt   sql.NullString
}

// scanTargets returns Scan destinations for a row laid out as elementColumns,
// in that exact order.
//
// Returning the slice rather than calling Scan is what lets the queue query
// append its two joined document columns without duplicating this list — and
// keeping the list in one place is what stops the scan order from drifting
// away from elementColumns, a mismatch the compiler cannot catch.
func scanTargets(element *Element, nullable *nullableElement) []any {
	return []any{
		&element.ID, &element.DocumentID, &element.ParentID, &element.Kind,
		&element.Title, &element.ContentHTML, &element.Quote,
		&nullable.startBlock, &nullable.startOffset,
		&nullable.endBlock, &nullable.endOffset,
		&element.Schedule.Priority, &element.Schedule.State, &nullable.dueOn,
		&element.Schedule.IntervalDays, &element.Schedule.AFactor, &element.Schedule.Reps,
		&element.ReadBlock, &element.Origin, &element.ExternalRef,
		&nullable.createdAt, &nullable.updatedAt,
	}
}

// apply folds the nullable columns into the element.
func (n nullableElement) apply(element *Element) {
	if n.startBlock.Valid && n.endBlock.Valid {
		element.HasRange = true
		element.Range = ir.Range{
			StartBlock:  int(n.startBlock.Int64),
			StartOffset: int(n.startOffset.Int64),
			EndBlock:    int(n.endBlock.Int64),
			EndOffset:   int(n.endOffset.Int64),
		}
	}

	if n.dueOn.Valid && n.dueOn.String != "" {
		// Parsed in the local zone, which main pins to the configured
		// timezone at startup so that "today" means the reader's today.
		if parsed, err := time.ParseInLocation(dateFormat, n.dueOn.String, time.Local); err == nil {
			element.Schedule.DueOn = parsed
		}
	}

	element.CreatedAt = parseTime(n.createdAt)
	element.UpdatedAt = parseTime(n.updatedAt)
}

// Queue returns the elements due on the given day, most important first.
//
// This is the whole scheduling read path: one ordered query. Because topics and
// items live in the same table, articles and extracts interleave by priority
// with no merging step — which is exactly SuperMemo's behaviour and the reason
// the schema unifies them.
func (s *Store) Queue(day time.Time, limit int) ([]QueueItem, error) {
	rows, err := s.db.Query(`
		SELECT `+elementColumns+`, d.title, d.url
		FROM elements e
		JOIN documents d ON d.id = e.document_id
		WHERE e.state NOT IN ('done', 'dismissed')
		  AND (e.due_on IS NULL OR e.due_on <= ?)
		ORDER BY e.priority ASC, e.due_on ASC, e.id ASC
		LIMIT ?`,
		day.Format(dateFormat), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: read queue: %w", err)
	}
	defer rows.Close()

	var queue []QueueItem
	for rows.Next() {
		var (
			item     QueueItem
			nullable nullableElement
		)
		targets := append(scanTargets(&item.Element, &nullable),
			&item.DocumentTitle, &item.DocumentURL)

		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("store: scan queue row: %w", err)
		}
		nullable.apply(&item.Element)
		queue = append(queue, item)
	}
	return queue, rows.Err()
}

// ElementByID reads one element.
func (s *Store) ElementByID(id int64) (Element, error) {
	var (
		element  Element
		nullable nullableElement
	)

	err := s.db.
		QueryRow(`SELECT `+elementColumns+` FROM elements e WHERE e.id = ?`, id).
		Scan(scanTargets(&element, &nullable)...)

	if errors.Is(err, sql.ErrNoRows) {
		return Element{}, fmt.Errorf("store: element %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Element{}, fmt.Errorf("store: read element %d: %w", id, err)
	}

	nullable.apply(&element)
	return element, nil
}

// ChildrenOf returns an element's direct children, oldest first.
func (s *Store) ChildrenOf(parentID int64) ([]Element, error) {
	rows, err := s.db.Query(
		`SELECT `+elementColumns+` FROM elements e WHERE e.parent_id = ? ORDER BY e.id`,
		parentID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: read children of %d: %w", parentID, err)
	}
	defer rows.Close()

	var children []Element
	for rows.Next() {
		var (
			child    Element
			nullable nullableElement
		)
		if err := rows.Scan(scanTargets(&child, &nullable)...); err != nil {
			return nil, fmt.Errorf("store: scan child of %d: %w", parentID, err)
		}
		nullable.apply(&child)
		children = append(children, child)
	}
	return children, rows.Err()
}

// NewExtract describes an extract about to be created.
type NewExtract struct {
	ParentID    int64
	DocumentID  int64
	Kind        string
	Title       string
	ContentHTML string
	Quote       string
	Range       ir.Range
	HasRange    bool
	Priority    float64
	Origin      string
	ExternalRef string
}

// CreateExtract inserts a child element and returns its id.
//
// A new extract is due immediately: having just decided a passage matters, the
// reader should see it again in this session rather than tomorrow.
func (s *Store) CreateExtract(extract NewExtract, now time.Time) (int64, error) {
	if extract.Kind == "" {
		extract.Kind = KindTopic
	}
	if extract.Origin == "" {
		extract.Origin = OriginManual
	}

	var (
		startBlock, startOffset any
		endBlock, endOffset     any
	)
	if extract.HasRange {
		startBlock, startOffset = extract.Range.StartBlock, extract.Range.StartOffset
		endBlock, endOffset = extract.Range.EndBlock, extract.Range.EndOffset
	}

	var externalRef any
	if extract.ExternalRef != "" {
		externalRef = extract.ExternalRef
	}

	outcome, err := s.db.Exec(`
		INSERT INTO elements
		    (document_id, parent_id, kind, title, content_html, quote,
		     start_block, start_offset, end_block, end_offset,
		     priority, state, due_on, interval_days, afactor, reps, read_block,
		     origin, external_ref, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'new', ?, 0, 2.0, 0, 0, ?, ?, ?, ?)`,
		extract.DocumentID, extract.ParentID, extract.Kind, extract.Title,
		extract.ContentHTML, extract.Quote,
		startBlock, startOffset, endBlock, endOffset,
		extract.Priority, now.Format(dateFormat),
		extract.Origin, externalRef,
		formatTime(now), formatTime(now),
	)
	if err != nil {
		return 0, fmt.Errorf("store: create extract under %d: %w", extract.ParentID, err)
	}

	id, err := outcome.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: read id of new extract: %w", err)
	}
	return id, nil
}

// SaveSchedule writes back an element's scheduling state after grading.
func (s *Store) SaveSchedule(id int64, schedule ir.Schedule, now time.Time) error {
	var dueOn any
	if !schedule.DueOn.IsZero() {
		dueOn = schedule.DueOn.Format(dateFormat)
	}

	_, err := s.db.Exec(`
		UPDATE elements SET
		    state = ?, due_on = ?, interval_days = ?, afactor = ?, reps = ?,
		    priority = ?, updated_at = ?
		WHERE id = ?`,
		string(schedule.State), dueOn, schedule.IntervalDays, schedule.AFactor,
		schedule.Reps, schedule.Priority, formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: save schedule for element %d: %w", id, err)
	}
	return nil
}

// SetPriority changes an element's priority without otherwise rescheduling it.
func (s *Store) SetPriority(id int64, priority float64, now time.Time) error {
	_, err := s.db.Exec(
		`UPDATE elements SET priority = ?, updated_at = ? WHERE id = ?`,
		priority, formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: set priority of element %d: %w", id, err)
	}
	return nil
}

// SetKind changes an element's kind, which happens when an extract gains its
// first cloze deletion and becomes an item.
func (s *Store) SetKind(id int64, kind string, now time.Time) error {
	_, err := s.db.Exec(
		`UPDATE elements SET kind = ?, updated_at = ? WHERE id = ?`,
		kind, formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: set kind of element %d: %w", id, err)
	}
	return nil
}

// SetReadBlock records how far through a topic the reader got.
//
// Called often while reading, so it deliberately touches nothing else — in
// particular not updated_at, which would make every scroll look like an edit.
func (s *Store) SetReadBlock(id int64, block int) error {
	_, err := s.db.Exec(`UPDATE elements SET read_block = ? WHERE id = ?`, block, id)
	if err != nil {
		return fmt.Errorf("store: set read position of element %d: %w", id, err)
	}
	return nil
}

// AddCloze records a deletion on an item and returns its ordinal.
func (s *Store) AddCloze(elementID int64, start, end int, hint string) (int, error) {
	existing, err := s.ClozesOf(elementID)
	if err != nil {
		return 0, err
	}
	ordinal := ir.NextOrdinal(existing)

	_, err = s.db.Exec(`
		INSERT INTO cloze_ranges (element_id, ordinal, start_offset, end_offset, hint)
		VALUES (?, ?, ?, ?, ?)`,
		elementID, ordinal, start, end, hint,
	)
	if err != nil {
		return 0, fmt.Errorf("store: add cloze to element %d: %w", elementID, err)
	}
	return ordinal, nil
}

// ClozesOf returns an item's deletions in ordinal order.
func (s *Store) ClozesOf(elementID int64) ([]ir.Cloze, error) {
	rows, err := s.db.Query(`
		SELECT ordinal, start_offset, end_offset, hint
		FROM cloze_ranges WHERE element_id = ? ORDER BY ordinal`,
		elementID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: read clozes of element %d: %w", elementID, err)
	}
	defer rows.Close()

	var clozes []ir.Cloze
	for rows.Next() {
		var cloze ir.Cloze
		if err := rows.Scan(&cloze.Ordinal, &cloze.Start, &cloze.End, &cloze.Hint); err != nil {
			return nil, fmt.Errorf("store: scan cloze: %w", err)
		}
		clozes = append(clozes, cloze)
	}
	return clozes, rows.Err()
}

// insertRootTopic creates the queue entry for a newly imported document.
//
// Every document gets exactly one root topic: the thing you actually read.
// Extracts taken from it become child elements later. It is due today, so a
// freshly synced article is immediately available rather than waiting a cycle.
func insertRootTopic(tx *sql.Tx, documentID int64, title string, now time.Time) error {
	_, err := tx.Exec(`
		INSERT INTO elements
		    (document_id, parent_id, kind, title, priority, state,
		     due_on, interval_days, afactor, reps, read_block, origin,
		     created_at, updated_at)
		VALUES (?, NULL, 'topic', ?, 0.5, 'new', ?, 0, 2.0, 0, 0, 'manual', ?, ?)`,
		documentID, title, now.Format(dateFormat),
		formatTime(now), formatTime(now),
	)
	if err != nil {
		return fmt.Errorf("store: create root topic for document %d: %w", documentID, err)
	}
	return nil
}

// CountElements returns how many elements exist in a given state.
// An empty state counts every element.
func (s *Store) CountElements(state string) (int, error) {
	query := `SELECT COUNT(*) FROM elements`
	args := []any{}
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, state)
	}

	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count elements: %w", err)
	}
	return count, nil
}

// CountDue returns how many elements are due for review on or before day.
func (s *Store) CountDue(day time.Time) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM elements
		WHERE state NOT IN ('done', 'dismissed')
		  AND (due_on IS NULL OR due_on <= ?)`,
		day.Format(dateFormat),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count due elements: %w", err)
	}
	return count, nil
}
