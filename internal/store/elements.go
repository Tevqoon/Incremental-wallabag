package store

import (
	"database/sql"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/source"
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

// Default priorities. Lower is more important, following SuperMemo's
// convention that priority is a position in the queue rather than a rank.
const (
	// defaultPriority is what a newly synced article gets.
	defaultPriority = 0.5

	// importedPriority is what a highlight imported from the provider gets.
	//
	// Deliberately below an unread article. A real library yields far more
	// annotations than unread articles — 459 against 36 in the case this was
	// built for — so leaving them equal buries the reading list under a
	// backlog of passages from things already read, which is the same failure
	// the archive flag was added to fix, just inverted. Ranking them lower
	// puts the reading list first and lets the backlog follow behind it; the
	// priority slider overrides this per element whenever a passage deserves
	// to jump the queue.
	importedPriority = 0.6
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

	// BuriedOn is the date this element was skipped, as YYYY-MM-DD. Kept as a
	// string because it is only ever compared to today for equality, never
	// ordered or arithmetic'd.
	BuriedOn string

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
	ReadingTime   int
}

// elementColumns is shared by every read so the scan order cannot drift apart
// from the query.
const elementColumns = `
	e.id, e.document_id, COALESCE(e.parent_id, 0), e.kind, e.title,
	e.content_html, e.quote,
	e.start_block, e.start_offset, e.end_block, e.end_offset,
	e.priority, e.state, e.due_on, e.interval_days, e.afactor, e.reps,
	e.read_block, e.origin, COALESCE(e.external_ref, ''),
	e.buried_on, e.created_at, e.updated_at`

// nullableElement holds the columns that can be NULL, which cannot be scanned
// straight into the Element fields they populate.
type nullableElement struct {
	startBlock  sql.NullInt64
	startOffset sql.NullInt64
	endBlock    sql.NullInt64
	endOffset   sql.NullInt64
	dueOn       sql.NullString
	buriedOn    sql.NullString
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
		&nullable.buriedOn, &nullable.createdAt, &nullable.updatedAt,
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

	if n.buriedOn.Valid {
		element.BuriedOn = n.buriedOn.String
	}
	element.CreatedAt = parseTime(n.createdAt)
	element.UpdatedAt = parseTime(n.updatedAt)
}

// Queue returns the elements due on the given day, most important first.
//
// This is the whole scheduling read path: one ordered query. Because topics and
// items live in the same table, articles and extracts interleave with no
// merging step — which is exactly SuperMemo's behaviour and the reason the
// schema unifies them.
//
// Within the day, root articles (parent_id NULL) and everything taken from one
// — extracts and clozes alike — are interleaved fairly rather than sorted into
// two blocks: each row gets a fractional position — (rank within its group -
// 0.5) / size of that group, both among what's due today — and rows are merged
// by that position. This spreads the rarer group evenly through the more
// common one in proportion to whatever is actually due, instead of a fixed
// ratio that would either flood the queue with extracts when few are due or
// bury them when many are. Priority (and, tied on that, due date, then a hash)
// only decides rank inside a group — see importedPriority for why that still
// keeps an imported backlog from swamping the front of that group's rank
// order.
//
// The hash is a multiplicative scramble of the id rather than the id itself.
// Within one group, items sharing a priority — a batch of highlights imported
// together, say — would otherwise sort by insertion order, which groups the
// oldest imports first. The hash scatters them while staying deterministic, so
// the queue does not reshuffle itself between page loads.
//
// Anything buried today sorts behind everything else still due, so working
// through the rest of the queue brings it back around rather than losing it for
// the day.
func (s *Store) Queue(day time.Time, limit int) ([]QueueItem, error) {
	rows, err := s.db.Query(`
		WITH due AS (
			SELECT `+elementColumns+`, d.title AS doc_title, d.url AS doc_url,
			       d.reading_time AS doc_reading_time,
			       ROW_NUMBER() OVER (
			           PARTITION BY (e.parent_id IS NULL)
			           ORDER BY e.priority ASC, e.due_on ASC,
			                    (e.id * 2654435761) % 1000003 ASC
			       ) AS kind_rank,
			       COUNT(*) OVER (PARTITION BY (e.parent_id IS NULL)) AS kind_count
			FROM elements e
			JOIN documents d ON d.id = e.document_id
			WHERE e.state NOT IN ('done', 'dismissed', 'suspended')
			  AND (e.due_on IS NULL OR e.due_on <= ?)
		)
		SELECT * FROM due
		ORDER BY (CASE WHEN buried_on = ? THEN 1 ELSE 0 END) ASC,
		         (CAST(kind_rank AS REAL) - 0.5) / kind_count ASC,
		         priority ASC, due_on ASC, (id * 2654435761) % 1000003 ASC
		LIMIT ?`,
		day.Format(dateFormat), day.Format(dateFormat), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: read queue: %w", err)
	}
	defer rows.Close()

	var queue []QueueItem
	for rows.Next() {
		var (
			item                QueueItem
			nullable            nullableElement
			kindRank, kindCount float64
		)
		// The trailing two destinations discard the window-function columns
		// (kind_rank, kind_count) added by the CTE for the ORDER BY — they
		// exist only to compute the interleave position, not to be read back.
		targets := append(scanTargets(&item.Element, &nullable),
			&item.DocumentTitle, &item.DocumentURL, &item.ReadingTime,
			&kindRank, &kindCount)

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

	// DelayDays is how long before the extract first becomes due. Zero means
	// today, which is what a caller with no opinion gets.
	DelayDays int
}

// CreateExtract inserts a child element and returns its id.
//
// A new extract comes back after DelayDays rather than immediately. Putting it
// straight back in front of the reader is the opposite of the point: the value
// of an extract is re-reading it once the article has faded, not twice in the
// same sitting.
//
// A manually made passage extract also queues itself for push upstream, in
// the same transaction as the local insert — the counterpart to how an
// imported highlight arrives with ExternalRef already set. Excluded: clozes
// (wallabag has no such concept) and anything already carrying an
// ExternalRef, which means it came from wallabag in the first place and
// pushing it back would just recreate what is already there.
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

	var id int64
	err := s.inTransaction(func(tx *sql.Tx) error {
		outcome, err := tx.Exec(`
			INSERT INTO elements
			    (document_id, parent_id, kind, title, content_html, quote,
			     start_block, start_offset, end_block, end_offset,
			     priority, state, due_on, interval_days, afactor, reps, read_block,
			     origin, external_ref, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'new', ?, 0, 2.0, 0, 0, ?, ?, ?, ?)`,
			extract.DocumentID, extract.ParentID, extract.Kind, extract.Title,
			extract.ContentHTML, extract.Quote,
			startBlock, startOffset, endBlock, endOffset,
			extract.Priority, now.AddDate(0, 0, extract.DelayDays).Format(dateFormat),
			extract.Origin, externalRef,
			formatTime(now), formatTime(now),
		)
		if err != nil {
			return fmt.Errorf("store: create extract under %d: %w", extract.ParentID, err)
		}

		id, err = outcome.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: read id of new extract: %w", err)
		}

		if extract.Kind == KindTopic && extract.Origin == OriginManual && extract.ExternalRef == "" {
			var docSource, docExternalID string
			err := tx.QueryRow(`SELECT source, external_id FROM documents WHERE id = ?`, extract.DocumentID).
				Scan(&docSource, &docExternalID)
			if err != nil {
				return fmt.Errorf("store: look up document %d for highlight push: %w", extract.DocumentID, err)
			}
			if err := enqueueHighlightCreate(tx, id, docSource, docExternalID, extract.Quote); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
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

// Bury moves an element to the end of today's queue.
//
// Distinct from every other grade in leaving the schedule alone: it changes
// position within a day, not which day. Recording the date rather than a flag
// means it expires by itself — tomorrow the value no longer matches today.
func (s *Store) Bury(id int64, today time.Time) error {
	_, err := s.db.Exec(
		`UPDATE elements SET buried_on = ? WHERE id = ?`,
		today.Format(dateFormat), id,
	)
	if err != nil {
		return fmt.Errorf("store: bury element %d: %w", id, err)
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

// insertRootTopic creates the queue entry for a newly imported document and
// returns its id.
//
// Every document gets exactly one root topic: the thing you actually read.
// Extracts taken from it become child elements later. An unread article is due
// today, so it is immediately available rather than waiting a cycle; an already
// archived one starts suspended, present in the library but out of the queue.
func insertRootTopic(tx *sql.Tx, documentID int64, title string, archived bool, now time.Time) (int64, error) {
	state, dueOn := string(ir.StateNew), any(now.Format(dateFormat))
	if archived {
		state, dueOn = string(ir.StateSuspended), nil
	}

	outcome, err := tx.Exec(`
		INSERT INTO elements
		    (document_id, parent_id, kind, title, priority, state,
		     due_on, interval_days, afactor, reps, read_block, origin,
		     created_at, updated_at)
		VALUES (?, NULL, 'topic', ?, ?, ?, ?, 0, 2.0, 0, 0, 'manual', ?, ?)`,
		documentID, title, defaultPriority, state, dueOn,
		formatTime(now), formatTime(now),
	)
	if err != nil {
		return 0, fmt.Errorf("store: create root topic for document %d: %w", documentID, err)
	}

	id, err := outcome.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: read id of new root topic: %w", err)
	}
	return id, nil
}

// rootTopicID finds a document's root topic.
func rootTopicID(tx *sql.Tx, documentID int64) (int64, error) {
	var id int64
	err := tx.QueryRow(
		`SELECT id FROM elements WHERE document_id = ? AND parent_id IS NULL`,
		documentID,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: find root topic of document %d: %w", documentID, err)
	}
	return id, nil
}

// suspendIfActive suspends an element only if it is still in circulation.
//
// The guard is what stops a sync from resurrecting material the reader already
// finished or abandoned, and from re-suspending something they deliberately
// pulled back into the queue.
func suspendIfActive(tx *sql.Tx, id int64, now time.Time) error {
	_, err := tx.Exec(`
		UPDATE elements
		SET state = ?, due_on = NULL, updated_at = ?
		WHERE id = ? AND state IN (?, ?)`,
		string(ir.StateSuspended), formatTime(now), id,
		string(ir.StateNew), string(ir.StateReading),
	)
	if err != nil {
		return fmt.Errorf("store: suspend element %d: %w", id, err)
	}
	return nil
}

// insertHighlights turns a provider's annotations into extracts, skipping any
// already imported.
//
// The extracts are created unanchored: locating a quote needs the article body,
// and the listing that carries annotations deliberately omits it. That costs
// nothing while the parent stays archived, and AnchorExtract fills the position
// in later if the article is ever opened.
func insertHighlights(tx *sql.Tx, documentID, parentID int64, highlights []source.Highlight, delayDays int, now time.Time) (int, error) {
	imported := 0

	for _, highlight := range highlights {
		if strings.TrimSpace(highlight.Quote) == "" || highlight.ExternalID == "" {
			continue
		}

		// The partial unique index on (document_id, external_ref) is what makes
		// this idempotent; asking first avoids burning rowids on every re-sync.
		var exists int
		err := tx.QueryRow(`
			SELECT COUNT(*) FROM elements
			WHERE document_id = ? AND external_ref = ?`,
			documentID, highlight.ExternalID,
		).Scan(&exists)
		if err != nil {
			return imported, fmt.Errorf("store: check highlight %s: %w", highlight.ExternalID, err)
		}
		if exists > 0 {
			continue
		}

		_, err = tx.Exec(`
			INSERT INTO elements
			    (document_id, parent_id, kind, title, content_html, quote,
			     priority, state, due_on, interval_days, afactor, reps,
			     read_block, origin, external_ref, created_at, updated_at)
			VALUES (?, ?, 'topic', ?, ?, ?, ?, 'new', ?, 0, 2.0, 0, 0, 'import', ?, ?, ?)`,
			documentID, parentID,
			SummariseQuote(highlight.Quote),
			"<p>"+html.EscapeString(highlight.Quote)+"</p>",
			highlight.Quote,
			importedPriority,
			// Spread across the window rather than all landing on the same
			// day: a library's import is hundreds of highlights at once, and
			// stacking them on one date moves the pile instead of clearing it.
			// The multiplier matches the queue's tie-break so the two agree.
			now.AddDate(0, 0, spreadOffset(documentID, delayDays)).Format(dateFormat),
			highlight.ExternalID,
			formatTime(now), formatTime(now),
		)
		if err != nil {
			return imported, fmt.Errorf("store: import highlight %s: %w", highlight.ExternalID, err)
		}
		imported++
	}

	return imported, nil
}

// spreadOffset scatters a batch of imports deterministically across a window.
//
// The result runs 1..window rather than 0..window-1: a highlight scheduled
// "ten days out" should not have a one-in-ten chance of being due the same day
// it was imported, which is the immediacy the delay exists to remove.
//
// The multiplier is the one the queue's tie-break uses, so the two orderings
// agree instead of one scrambling what the other arranged.
func spreadOffset(seed int64, window int) int {
	if window <= 0 {
		return 0
	}
	return int(((seed*2654435761)%int64(window)+int64(window))%int64(window)) + 1
}

// AnchorExtract records where an extract sits in its parent, for one that was
// imported before the parent's text was available.
func (s *Store) AnchorExtract(id int64, position ir.Range, quote, contentHTML string, now time.Time) error {
	_, err := s.db.Exec(`
		UPDATE elements SET
		    start_block = ?, start_offset = ?, end_block = ?, end_offset = ?,
		    quote = ?, content_html = ?, updated_at = ?
		WHERE id = ?`,
		position.StartBlock, position.StartOffset, position.EndBlock, position.EndOffset,
		quote, contentHTML, formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: anchor extract %d: %w", id, err)
	}
	return nil
}

// Suspend takes an element out of circulation without discarding it.
//
// Distinct from Done and Dismiss, which are terminal: a suspended element keeps
// its interval and repetition count and resumes where it left off.
func (s *Store) Suspend(id int64, now time.Time) error {
	_, err := s.db.Exec(
		`UPDATE elements SET state = ?, due_on = NULL, updated_at = ? WHERE id = ?`,
		string(ir.StateSuspended), formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: suspend element %d: %w", id, err)
	}
	return nil
}

// Unsuspend returns an element to the queue, due today.
func (s *Store) Unsuspend(id int64, today time.Time, now time.Time) error {
	_, err := s.db.Exec(
		`UPDATE elements SET state = ?, due_on = ?, updated_at = ? WHERE id = ?`,
		string(ir.StateReading), today.Format(dateFormat), formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: unsuspend element %d: %w", id, err)
	}
	return nil
}

// DeleteExtract permanently removes an extract or item, and everything under
// it — its own extracts, clozes, and export ledger rows — via the schema's
// ON DELETE CASCADE. Refuses a root element: deleting a whole article is a
// different, much larger decision than the "accidental selection" this exists
// for, and belongs to a different action (Dismiss) if ever wanted at all.
//
// If the extract has an id upstream — imported from a wallabag highlight, or
// a manual one that was successfully pushed there — this also queues its
// removal upstream, in the same transaction as the local delete. Without that
// pairing the highlight would survive at the provider and the next sync would
// recreate the very extract just deleted — silently undoing the one thing the
// reader asked for.
func (s *Store) DeleteExtract(id int64) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		var (
			parentID               sql.NullInt64
			externalRef, docSource string
		)
		err := tx.QueryRow(`
			SELECT e.parent_id, COALESCE(e.external_ref, ''), d.source
			FROM elements e JOIN documents d ON d.id = e.document_id
			WHERE e.id = ?`, id,
		).Scan(&parentID, &externalRef, &docSource)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: element %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: look up element %d: %w", id, err)
		}
		if !parentID.Valid {
			return fmt.Errorf("store: element %d is a whole article, not an extract", id)
		}

		if externalRef != "" {
			if err := enqueueWrite(tx, docSource, externalRef, OpHighlightDelete, ""); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(`DELETE FROM elements WHERE id = ?`, id); err != nil {
			return fmt.Errorf("store: delete element %d: %w", id, err)
		}
		return nil
	})
}

// SummariseQuote builds a short title from a passage's opening words.
func SummariseQuote(text string) string {
	const limit = 80

	normalised := ir.NormalizeSpace(text)
	if len(normalised) <= limit {
		return normalised
	}
	truncated := normalised[:limit]
	if space := strings.LastIndex(truncated, " "); space > limit/2 {
		truncated = truncated[:space]
	}
	return truncated + "…"
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
		WHERE state NOT IN ('done', 'dismissed', 'suspended')
		  AND (due_on IS NULL OR due_on <= ?)`,
		day.Format(dateFormat),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count due elements: %w", err)
	}
	return count, nil
}

// ExtractFilter selects which extracts the browse page lists.
type ExtractFilter struct {
	// Origin restricts to OriginManual or OriginImport; empty means both.
	Origin string

	// WithClozes lists only extracts that have become items.
	WithClozes bool

	Query string
	Limit int
}

// ExtractRow is one row of the extracts browse page.
type ExtractRow struct {
	Element
	DocumentTitle string
	DocumentURL   string
	ClozeCount    int
}

// Extracts lists extracts independently of what is due.
//
// The queue answers "what should I read now" and deliberately interleaves
// articles with extracts; this answers "what have I harvested", which is a
// different question and needs its own ordering — newest first, because the
// thing you just pulled out is the thing you most likely want.
func (s *Store) Extracts(filter ExtractFilter) ([]ExtractRow, error) {
	if filter.Limit <= 0 {
		filter.Limit = 200
	}
	pattern := "%" + filter.Query + "%"

	rows, err := s.db.Query(`
		SELECT `+elementColumns+`, d.title, d.url,
		       (SELECT COUNT(*) FROM cloze_ranges c WHERE c.element_id = e.id)
		FROM elements e
		JOIN documents d ON d.id = e.document_id
		WHERE e.parent_id IS NOT NULL
		  AND e.state NOT IN ('dismissed')
		  AND (? = '' OR e.origin = ?)
		  AND (? = 0 OR e.kind = 'item')
		  AND (? = '' OR e.quote LIKE ? OR d.title LIKE ?)
		ORDER BY e.id DESC
		LIMIT ?`,
		filter.Origin, filter.Origin,
		filter.WithClozes,
		filter.Query, pattern, pattern,
		filter.Limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list extracts: %w", err)
	}
	defer rows.Close()

	var extracts []ExtractRow
	for rows.Next() {
		var (
			row      ExtractRow
			nullable nullableElement
		)
		targets := append(scanTargets(&row.Element, &nullable),
			&row.DocumentTitle, &row.DocumentURL, &row.ClozeCount)

		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("store: scan extract row: %w", err)
		}
		nullable.apply(&row.Element)
		extracts = append(extracts, row)
	}
	return extracts, rows.Err()
}

// CountExtracts returns how many extracts exist, by origin.
func (s *Store) CountExtracts(origin string) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM elements
		WHERE parent_id IS NOT NULL AND (? = '' OR origin = ?)`,
		origin, origin,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count extracts: %w", err)
	}
	return count, nil
}
