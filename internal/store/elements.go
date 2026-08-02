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

	// MissingUpstream is set by Reconcile when a full listing's annotations no
	// longer include this extract's ExternalRef — the counterpart to
	// Document.MissingUpstream, for an individual highlight rather than a
	// whole article. Only ever meaningful when ExternalRef is set: increader
	// keeps a superset of wallabag's data, so a highlight deleted there stays
	// here, flagged rather than removed.
	MissingUpstream bool

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
	e.read_block, e.origin, COALESCE(e.external_ref, ''), e.missing_upstream,
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
		&element.ReadBlock, &element.Origin, &element.ExternalRef, &element.MissingUpstream,
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

	element.Schedule.DueOn = parseDate(n.dueOn)

	if n.buriedOn.Valid {
		element.BuriedOn = n.buriedOn.String
	}
	element.CreatedAt = parseTime(n.createdAt)
	element.UpdatedAt = parseTime(n.updatedAt)
}

// Queue returns the elements due on the given day, most important first.
//
// This is the whole scheduling read path. Because topics and items live in
// the same table, articles and extracts interleave with no merging step —
// which is exactly SuperMemo's behaviour and the reason the schema unifies
// them.
//
// The ordering itself lives in queue_rank, a column rather than a computed
// value — see assignQueueRanks for why. Here it is just one more ORDER BY
// term, alongside priority, due date and a hash as tie-breaks for whatever
// (rarely) shares a rank.
//
// Anything buried today sorts behind everything else still due, so working
// through the rest of the queue brings it back around rather than losing it for
// the day.
func (s *Store) Queue(day time.Time, limit int) ([]QueueItem, error) {
	if err := s.assignQueueRanks(day); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`
		SELECT `+elementColumns+`, d.title, d.url, d.reading_time
		FROM elements e
		JOIN documents d ON d.id = e.document_id
		WHERE e.state NOT IN ('done', 'dismissed', 'suspended')
		  AND (e.due_on IS NULL OR e.due_on <= ?)
		ORDER BY (CASE WHEN e.buried_on = ? THEN 1 ELSE 0 END) ASC,
		         e.queue_rank ASC, e.priority ASC, e.due_on ASC,
		         (e.id * 2654435761) % 1000003 ASC
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
			item     QueueItem
			nullable nullableElement
		)
		targets := append(scanTargets(&item.Element, &nullable),
			&item.DocumentTitle, &item.DocumentURL, &item.ReadingTime)

		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("store: scan queue row: %w", err)
		}
		nullable.apply(&item.Element)
		queue = append(queue, item)
	}
	return queue, rows.Err()
}

// assignQueueRanks fills in queue_rank for any due, non-terminal element that
// does not have one yet — an element newly due since the last read, or one
// whose schedule just changed and had its rank cleared (see SaveSchedule).
//
// This is the whole fairness mechanism, and it only ever runs for additions.
// Each unranked row gets a fractional position — (rank within its group -
// 0.5) / size of that group, root articles and everything taken from one
// being the two groups, both counted among what's due today — which spreads
// the rarer group evenly through the more common one in proportion to
// whatever is actually due. Crucially, the candidate rank is computed over
// the *whole* due population, ranked and unranked together, but only ever
// written to rows that were NULL: an element that already has a rank keeps
// it untouched, no matter how the population around it changes. That is what
// stops grading one element from reshuffling everything else — the previous,
// fully-recomputed version of this query shrank every remaining element's
// denominator on every single grade, which could visibly jump an unrelated
// item across another for no reason connected to it.
//
// The hash is a multiplicative scramble of the id rather than the id itself.
// Within one group, items sharing a priority — a batch of highlights
// imported together, say — would otherwise rank by insertion order, which
// groups the oldest imports first.
func (s *Store) assignQueueRanks(day time.Time) error {
	_, err := s.db.Exec(`
		UPDATE elements
		SET queue_rank = ranked.candidate_rank
		FROM (
			SELECT e.id AS id,
			       (CAST(ROW_NUMBER() OVER (
			           PARTITION BY (e.parent_id IS NULL)
			           ORDER BY e.priority ASC, e.due_on ASC,
			                    (e.id * 2654435761) % 1000003 ASC
			       ) AS REAL) - 0.5)
			       / COUNT(*) OVER (PARTITION BY (e.parent_id IS NULL)) AS candidate_rank
			FROM elements e
			WHERE e.state NOT IN ('done', 'dismissed', 'suspended')
			  AND (e.due_on IS NULL OR e.due_on <= ?)
		) AS ranked
		WHERE elements.id = ranked.id
		  AND elements.queue_rank IS NULL`,
		day.Format(dateFormat),
	)
	if err != nil {
		return fmt.Errorf("store: assign queue ranks: %w", err)
	}
	return nil
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
//
// queue_rank is cleared, not carried over: a due date earned by this grade
// (or a backlog button — see Backlog) is a new position in time, and the
// rank belongs to whatever is due when it gets there, not a leftover from
// wherever it happened to sit before. assignQueueRanks fills it back in the
// next time this element is actually due and read.
func (s *Store) SaveSchedule(id int64, schedule ir.Schedule, now time.Time) error {
	var dueOn any
	if !schedule.DueOn.IsZero() {
		dueOn = schedule.DueOn.Format(dateFormat)
	}

	_, err := s.db.Exec(`
		UPDATE elements SET
		    state = ?, due_on = ?, interval_days = ?, afactor = ?, reps = ?,
		    priority = ?, queue_rank = NULL, updated_at = ?
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
		`UPDATE elements SET priority = ?, queue_rank = NULL, updated_at = ? WHERE id = ?`,
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

// DeleteCloze removes one deletion from an item, identified by its ordinal
// within that element — the number Anki turns into a card, and the number
// the reader actually sees, rather than a database row id nothing outside
// this package ever handles.
//
// Deleting every deletion an item has does not delete the item itself, nor
// does it touch its kind: whether an item with zero clozes should revert to
// being a plain extract is the caller's call to make, the same way
// promoting a plain extract to an item on its first cloze is handleCloze's
// call and not AddCloze's.
func (s *Store) DeleteCloze(elementID int64, ordinal int) error {
	result, err := s.db.Exec(
		`DELETE FROM cloze_ranges WHERE element_id = ? AND ordinal = ?`,
		elementID, ordinal,
	)
	if err != nil {
		return fmt.Errorf("store: delete cloze %d of element %d: %w", ordinal, elementID, err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("store: element %d has no cloze %d: %w", elementID, ordinal, ErrNotFound)
	}
	return nil
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
		SET state = ?, due_on = NULL, queue_rank = NULL, updated_at = ?
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

		// Not found under this exact ref — but is it the same passage under
		// a *different*, newer one? UpdateHighlightLocation replaces an
		// annotation by creating a new one and best-effort deleting the old,
		// and if that delete has not gone through by the next full listing,
		// the same quote shows up twice: once under the ref already stored
		// here, once under the new one nothing local matches yet. Checking
		// external_ref alone would treat the second as a brand new
		// highlight and duplicate it. Adopting the new ref onto the row
		// that is already here instead keeps one local row per highlight no
		// matter how many times its upstream id churns underneath it.
		var existingID int64
		err = tx.QueryRow(`
			SELECT id FROM elements
			WHERE document_id = ? AND parent_id = ? AND quote = ?
			LIMIT 1`,
			documentID, parentID, highlight.Quote,
		).Scan(&existingID)
		switch {
		case err == nil:
			if _, err := tx.Exec(`
				UPDATE elements SET external_ref = ?, missing_upstream = 0, updated_at = ?
				WHERE id = ?`,
				highlight.ExternalID, formatTime(now), existingID,
			); err != nil {
				return imported, fmt.Errorf("store: adopt new ref for highlight %s: %w", highlight.ExternalID, err)
			}
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return imported, fmt.Errorf("store: check highlight quote %s: %w", highlight.ExternalID, err)
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
		`UPDATE elements SET state = ?, due_on = NULL, queue_rank = NULL, updated_at = ? WHERE id = ?`,
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
		`UPDATE elements SET state = ?, due_on = ?, queue_rank = NULL, updated_at = ? WHERE id = ?`,
		string(ir.StateReading), today.Format(dateFormat), formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: unsuspend element %d: %w", id, err)
	}
	return nil
}

// ReconcileMissingHighlights flags extracts whose annotation is absent from
// present as missing upstream, and clears the flag on any that have
// reappeared in it — the same mechanism as Store.ReconcileMissing, one level
// down: an individual highlight rather than a whole document.
//
// present must be every annotation external_ref a full listing of the source
// currently reports, gathered the same way Reconcile gathers present document
// ids: an incremental "changed since" fetch can never notice a deletion.
//
// Only extracts belonging to a document of the given source, and actually
// carrying an external_ref, are ever touched — a plain manual extract was
// never meant to correspond to anything upstream, and this is not the place
// to decide otherwise. Origin does not matter beyond that: an imported
// highlight and a manual extract successfully pushed to wallabag are both,
// from this point on, just an extract with a real annotation behind it.
func (s *Store) ReconcileMissingHighlights(sourceName string, present []string) (marked, cleared int, err error) {
	err = s.inTransaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TEMP TABLE present_refs (external_ref TEXT PRIMARY KEY)`); err != nil {
			return fmt.Errorf("store: create temp table: %w", err)
		}
		defer tx.Exec(`DROP TABLE present_refs`)

		insert, err := tx.Prepare(`INSERT OR IGNORE INTO present_refs VALUES (?)`)
		if err != nil {
			return fmt.Errorf("store: prepare temp insert: %w", err)
		}
		defer insert.Close()
		for _, ref := range present {
			if _, err := insert.Exec(ref); err != nil {
				return fmt.Errorf("store: populate temp table: %w", err)
			}
		}

		result, err := tx.Exec(`
			UPDATE elements SET missing_upstream = 1
			WHERE missing_upstream = 0
			  AND external_ref IS NOT NULL AND external_ref <> ''
			  AND external_ref NOT IN (SELECT external_ref FROM present_refs)
			  AND document_id IN (SELECT id FROM documents WHERE source = ?)`,
			sourceName)
		if err != nil {
			return fmt.Errorf("store: mark missing highlights: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil {
			marked = int(n)
		}

		result, err = tx.Exec(`
			UPDATE elements SET missing_upstream = 0
			WHERE missing_upstream = 1
			  AND external_ref IN (SELECT external_ref FROM present_refs)
			  AND document_id IN (SELECT id FROM documents WHERE source = ?)`,
			sourceName)
		if err != nil {
			return fmt.Errorf("store: clear missing highlights: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil {
			cleared = int(n)
		}
		return nil
	})
	return marked, cleared, err
}

// BackfillHighlightPushes gives a missed extract push a fresh chance, two
// ways: queuing one for an extract that has never had one queued at all —
// made before the push-back feature existed, say — and separately resetting
// any that exhausted every retry attempt before a bug in this pipeline was
// fixed (the wallabag quote-length limit discovered after shipping, or the
// database corruption incident) rather than because the write could never
// succeed. PendingWrites filters attempts >= maxWriteAttempts out of every
// future drain, so without this reset an extract caught by either bug would
// sit abandoned forever: the retries that exhausted it happened for a reason
// that no longer applies, but nothing else would ever look at it again.
//
// Idempotent by construction: an extract already carrying an external_ref
// matches none of this, and one with a live (not exhausted) write already
// queued is left to that write rather than duplicated. That is what makes it
// safe to run from Reconcile on every pass rather than as a one-off
// migration step.
func (s *Store) BackfillHighlightPushes(sourceName string) (queued int, err error) {
	err = s.inTransaction(func(tx *sql.Tx) error {
		reset, err := tx.Exec(`
			UPDATE pending_writes SET attempts = 0, last_error = ''
			WHERE operation = ? AND attempts >= ?
			  AND element_id IN (
			      SELECT e.id FROM elements e
			      JOIN documents d ON d.id = e.document_id
			      WHERE d.source = ?
			        AND (e.external_ref IS NULL OR e.external_ref = ''))`,
			OpHighlightCreate, maxWriteAttempts, sourceName)
		if err != nil {
			return fmt.Errorf("store: reset abandoned highlight pushes: %w", err)
		}
		if n, err := reset.RowsAffected(); err == nil {
			queued += int(n)
		}

		type candidate struct {
			id                 int64
			quote, documentRef string
		}

		// Collected into a slice and the Rows closed before any INSERT below,
		// rather than enqueueing from inside this loop: with the connection
		// pool capped at one, a write attempted while this SELECT is still
		// open would wait for a connection the loop itself is holding.
		candidates, err := func() ([]candidate, error) {
			rows, err := tx.Query(`
				SELECT e.id, e.quote, d.external_id
				FROM elements e
				JOIN documents d ON d.id = e.document_id
				WHERE d.source = ?
				  AND e.parent_id IS NOT NULL
				  AND e.kind = ?
				  AND e.origin = ?
				  AND (e.external_ref IS NULL OR e.external_ref = '')
				  AND NOT EXISTS (
				      SELECT 1 FROM pending_writes pw
				      WHERE pw.element_id = e.id AND pw.operation = ?
				  )`,
				sourceName, KindTopic, OriginManual, OpHighlightCreate)
			if err != nil {
				return nil, fmt.Errorf("store: find extracts needing backfill: %w", err)
			}
			defer rows.Close()

			var found []candidate
			for rows.Next() {
				var c candidate
				if err := rows.Scan(&c.id, &c.quote, &c.documentRef); err != nil {
					return nil, fmt.Errorf("store: scan backfill candidate: %w", err)
				}
				found = append(found, c)
			}
			return found, rows.Err()
		}()
		if err != nil {
			return err
		}

		for _, c := range candidates {
			if err := enqueueHighlightCreate(tx, c.id, sourceName, c.documentRef, c.quote); err != nil {
				return err
			}
			queued++
		}
		return nil
	})
	return queued, err
}

// QueueLocationUpdates queues an OpHighlightUpdateLocation write for every
// local extract whose external_ref appears in locationless — annotations a
// full listing reports as still having no location for the provider's own
// reader to draw them at (Highlight.HasLocation false), almost always
// because they were pushed before increader computed one at all. Whether the
// extract came from an import or a manual push does not matter: both are,
// from this point on, indistinguishable — just an extract with a real
// annotation behind it that the provider cannot currently render.
//
// Idempotent: an extract that already has one queued is left alone rather
// than duplicated, the same guarantee BackfillHighlightPushes gives its own
// operation, and what makes this safe to run from Reconcile on every pass.
func (s *Store) QueueLocationUpdates(sourceName string, locationless []string) (queued int, err error) {
	if len(locationless) == 0 {
		return 0, nil
	}
	err = s.inTransaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TEMP TABLE locationless_refs (external_ref TEXT PRIMARY KEY)`); err != nil {
			return fmt.Errorf("store: create temp table: %w", err)
		}
		defer tx.Exec(`DROP TABLE locationless_refs`)

		insert, err := tx.Prepare(`INSERT OR IGNORE INTO locationless_refs VALUES (?)`)
		if err != nil {
			return fmt.Errorf("store: prepare temp insert: %w", err)
		}
		defer insert.Close()
		for _, ref := range locationless {
			if _, err := insert.Exec(ref); err != nil {
				return fmt.Errorf("store: populate temp table: %w", err)
			}
		}

		type candidate struct {
			id            int64
			quote, oldRef string
		}

		// Collected into a slice and the Rows closed before any INSERT
		// below, matching BackfillHighlightPushes: a write attempted while
		// this SELECT is still open would wait for the connection the loop
		// itself is holding, with the pool capped at one.
		candidates, err := func() ([]candidate, error) {
			rows, err := tx.Query(`
				SELECT e.id, e.quote, e.external_ref
				FROM elements e
				JOIN documents d ON d.id = e.document_id
				WHERE d.source = ?
				  AND e.external_ref IN (SELECT external_ref FROM locationless_refs)
				  AND NOT EXISTS (
				      SELECT 1 FROM pending_writes pw
				      WHERE pw.element_id = e.id AND pw.operation = ?
				  )`,
				sourceName, OpHighlightUpdateLocation)
			if err != nil {
				return nil, fmt.Errorf("store: find extracts needing a location: %w", err)
			}
			defer rows.Close()

			var found []candidate
			for rows.Next() {
				var c candidate
				if err := rows.Scan(&c.id, &c.quote, &c.oldRef); err != nil {
					return nil, fmt.Errorf("store: scan location-update candidate: %w", err)
				}
				found = append(found, c)
			}
			return found, rows.Err()
		}()
		if err != nil {
			return err
		}

		for _, c := range candidates {
			// external_id here is the annotation's own id, the same
			// convention OpHighlightDelete uses — not the document's, the
			// way OpHighlightCreate needs it, since this replaces something
			// that already exists rather than making something new.
			if _, err := tx.Exec(`
				INSERT INTO pending_writes (source, external_id, operation, payload, element_id, created_at)
				VALUES (?, ?, ?, ?, ?, ?)`,
				sourceName, c.oldRef, OpHighlightUpdateLocation, c.quote, c.id, formatTime(time.Now())); err != nil {
				return fmt.Errorf("store: queue location update: %w", err)
			}
			queued++
		}
		return nil
	})
	return queued, err
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

	// MissingOnly lists only extracts Reconcile could not find upstream any
	// more, the extract-level counterpart to LibraryFilter's "missing" state.
	MissingOnly bool

	// Sort is "" (newest first), "due" (soonest due first, NULLs last),
	// "priority" (most important first) or "oldest" (first-taken first).
	Sort string

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
// different question and needs its own ordering — newest first by default,
// because the thing you just pulled out is the thing you most likely want,
// but see ExtractFilter.Sort for the other orderings the browse page offers.
func (s *Store) Extracts(filter ExtractFilter) ([]ExtractRow, error) {
	if filter.Limit <= 0 {
		filter.Limit = 200
	}
	pattern := "%" + filter.Query + "%"

	// filter.Sort selects one of a fixed set of Go string literals below, so
	// it is safe to splice into the query directly — it never carries user
	// input as SQL text, only as the ordinary bound ? parameters above it.
	orderBy := "e.id DESC"
	switch filter.Sort {
	case "due":
		orderBy = "(e.due_on IS NULL) ASC, e.due_on ASC, e.id DESC"
	case "priority":
		orderBy = "e.priority ASC, e.id DESC"
	case "oldest":
		orderBy = "e.id ASC"
	}

	rows, err := s.db.Query(`
		SELECT `+elementColumns+`, d.title, d.url,
		       (SELECT COUNT(*) FROM cloze_ranges c WHERE c.element_id = e.id)
		FROM elements e
		JOIN documents d ON d.id = e.document_id
		WHERE e.parent_id IS NOT NULL
		  AND e.state NOT IN ('dismissed')
		  AND (? = '' OR e.origin = ?)
		  AND (? = 0 OR e.kind = 'item')
		  AND (? = 0 OR e.missing_upstream = 1)
		  AND (? = '' OR e.quote LIKE ? OR d.title LIKE ?)
		ORDER BY `+orderBy+`
		LIMIT ?`,
		filter.Origin, filter.Origin,
		filter.WithClozes,
		filter.MissingOnly,
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

// CountMissingHighlights returns how many extracts Reconcile has flagged as
// no longer found upstream, for the "Missing" filter tab on the extracts page.
func (s *Store) CountMissingHighlights() (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM elements WHERE parent_id IS NOT NULL AND missing_upstream = 1`,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count missing highlights: %w", err)
	}
	return count, nil
}
