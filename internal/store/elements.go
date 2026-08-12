package store

import (
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
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

	// Ranges is the provider's own position record for this highlight, e.g.
	// wallabag's XPath ranges — stored opaquely, exactly as
	// source.Highlight.Ranges carried it in, for wallabag.Source's
	// ResolveRange to recover the highlight's full text from later, once
	// the article's own HTML is available. Empty for anything that is not
	// an imported highlight with one, which is most elements.
	Ranges string

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

	// Note is the reader's own comment on the passage, as opposed to the
	// passage itself. Articles rarely have one; a book annotation often is
	// one, with no passage at all.
	Note string

	// Chapter and Page are where in the work the passage came from. Both are
	// empty for anything that did not arrive from a book — see
	// source.Highlight for why Page is a string.
	Chapter string
	Page    string

	// Color is the annotation's own colour as "#rrggbb", when its provider
	// recorded one. Stored but not yet acted on.
	Color string

	// Ordinal is reading order within the document, counting from one, for
	// imported annotations. Zero for everything else, which sorts them ahead
	// of the imports on a contents page — where a manual extract, having been
	// made while reading rather than while exporting, has no place in the
	// original's own order anyway.
	Ordinal int

	// TriagedAt is when this element was last decided about in a document's
	// triage pass, zero if never. See Store.UntriagedAnnotations.
	TriagedAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Triaged reports whether this element has been through a triage pass.
func (e Element) Triaged() bool { return !e.TriagedAt.IsZero() }

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

// QueueKind names one of the two queues.
//
// The two partition the elements table exactly — a root element is a whole
// document, everything else was harvested out of one — so there is no third
// value and deliberately no "both". A combined read would have to decide how
// to order two populations against each other, and ordering them by priority
// alone sorts them into blocks: every article (defaultPriority) ahead of every
// imported highlight (importedPriority), which is the failure the queue used
// to spend a whole ranking column avoiding. Keeping them apart is what makes
// that column unnecessary rather than merely unused.
type QueueKind string

const (
	// QueueArticles is the reading queue: whole documents.
	QueueArticles QueueKind = "articles"

	// QueueExtracts is the review queue: passages taken from a document,
	// whichever route they arrived by. A book's own root topic is always
	// suspended (there is no body to read — see importAnnotations), so a
	// book contributes only to this queue, and its passages join it as soon
	// as a triage pass unsuspends them.
	QueueExtracts QueueKind = "extracts"
)

// predicate returns the SQL that selects this queue's half of the elements
// table, aliased e.
//
// A fixed string literal per kind, never a value spliced in from a request:
// the caller's job is to reject an unknown kind, which ParseQueueKind does,
// and this errors rather than silently widening if one gets through anyway.
func (k QueueKind) predicate() (string, error) {
	switch k {
	case QueueArticles:
		return "e.parent_id IS NULL", nil
	case QueueExtracts:
		return "e.parent_id IS NOT NULL", nil
	}
	return "", fmt.Errorf("store: unknown queue kind %q", string(k))
}

// ParseQueueKind maps a request's own spelling of a queue onto one, defaulting
// to the articles queue when nothing was asked for — the queue a reading
// session starts in.
func ParseQueueKind(value string) (QueueKind, bool) {
	switch value {
	case "", string(QueueArticles):
		return QueueArticles, true
	case string(QueueExtracts):
		return QueueExtracts, true
	}
	return "", false
}

// elementColumns is shared by every read so the scan order cannot drift apart
// from the query.
const elementColumns = `
	e.id, e.document_id, COALESCE(e.parent_id, 0), e.kind, e.title,
	e.content_html, e.quote,
	e.start_block, e.start_offset, e.end_block, e.end_offset,
	e.priority, e.state, e.due_on, e.interval_days, e.afactor, e.reps,
	e.read_block, e.origin, COALESCE(e.external_ref, ''), e.missing_upstream,
	e.buried_on, COALESCE(e.ranges, ''), e.created_at, e.updated_at,
	e.note, e.chapter, e.page, e.color, e.ordinal, e.triaged_at`

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
	triagedAt   sql.NullString
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
		&nullable.buriedOn, &element.Ranges, &nullable.createdAt, &nullable.updatedAt,
		&element.Note, &element.Chapter, &element.Page, &element.Color,
		&element.Ordinal, &nullable.triagedAt,
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
	element.TriagedAt = parseTime(n.triagedAt)
}

// Queue returns one queue's elements due on the given day, most important
// first.
//
// This is the whole scheduling read path, and it reads exactly one of the two
// queues — see QueueKind for why there is no combined mode.
//
// Ordering is priority, then due date, then a hash of the id. Every term is a
// value on the row itself, which is what makes the order stable as the queue
// drains: grading one element away cannot move any other, because nothing here
// is computed relative to the rest of the population. (It once was, and that
// is precisely what went wrong — see migration 017.) The hash is a
// multiplicative scramble rather than the id itself, so that a batch of
// highlights imported together, all sharing a priority, does not simply come
// back in insertion order with the oldest import always first.
//
// Anything buried today sorts behind everything else still due, so working
// through the rest of the queue brings it back around rather than losing it for
// the day. Within that bucket, whichever was buried longest ago comes back
// first — a round robin keyed on buried_at, so burying the same element again
// always sends it to the very back of the line rather than leaving it wherever
// it already was. Because the bury terms sit inside this kind-filtered query,
// "later today" is per queue: skipping an extract cycles it through the extract
// queue and leaves the reading queue's order untouched.
// A limit of zero or less returns everything due, which is the default: SQLite
// treats a negative LIMIT as no limit, so the two are the same query. Callers
// pass a cap when they are rendering a bounded list (a preview, a page the
// reader asked to trim), never to decide how much reading a day contains —
// nothing here has ever done that.
func (s *Store) Queue(day time.Time, kind QueueKind, limit int) ([]QueueItem, error) {
	predicate, err := kind.predicate()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = -1
	}

	rows, err := s.db.Query(`
		SELECT `+elementColumns+`, COALESCE(NULLIF(d.display_title, ''), d.title), d.url, d.reading_time
		FROM elements e
		JOIN documents d ON d.id = e.document_id
		WHERE `+predicate+`
		  AND e.state NOT IN ('done', 'dismissed', 'suspended')
		  AND (e.due_on IS NULL OR e.due_on <= ?)
		ORDER BY (CASE WHEN e.buried_on = ? THEN 1 ELSE 0 END) ASC,
		         (CASE WHEN e.buried_on = ? THEN e.buried_at END) ASC,
		         e.priority ASC, e.due_on ASC,
		         (e.id * 2654435761) % 1000003 ASC
		LIMIT ?`,
		day.Format(dateFormat), day.Format(dateFormat), day.Format(dateFormat), limit,
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

	// Ranges is the provider's own position record for this highlight,
	// carried through opaquely — see Element.Ranges.
	Ranges string

	// DelayDays is how long before the extract first becomes due. Zero means
	// today, which is what a caller with no opinion gets. Lightly fuzzed by a
	// few days either way (see ir.FuzzedFirstDueDays) so that pulling several
	// passages from one article in a sitting does not put them all back in
	// front of the reader on the same future date.
	DelayDays int
}

// CreateExtract inserts a child element and returns its id.
//
// A new extract comes back after DelayDays (fuzzed a little, see
// ir.FuzzedFirstDueDays) rather than immediately or on a suspiciously round
// date. Putting it straight back in front of the reader is the opposite of
// the point: the value of an extract is re-reading it once the article has
// faded, not twice in the same sitting.
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
	var ranges any
	if extract.Ranges != "" {
		ranges = extract.Ranges
	}

	// Seeded on (parentID, quote) rather than the extract's own id, which does
	// not exist yet — this runs before the INSERT that would assign one. Using
	// the parent and quote also means the fuzz is stable if this extract is
	// ever recreated from the same passage, rather than drifting on every call.
	fuzzedDelay := ir.FuzzedFirstDueDays(extractSeed(extract.ParentID, extract.Quote), extract.DelayDays)

	var id int64
	err := s.inTransaction(func(tx *sql.Tx) error {
		outcome, err := tx.Exec(`
			INSERT INTO elements
			    (document_id, parent_id, kind, title, content_html, quote,
			     start_block, start_offset, end_block, end_offset,
			     priority, state, due_on, interval_days, afactor, reps, read_block,
			     origin, external_ref, ranges, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'new', ?, 0, 2.0, 0, 0, ?, ?, ?, ?, ?)`,
			extract.DocumentID, extract.ParentID, extract.Kind, extract.Title,
			extract.ContentHTML, extract.Quote,
			startBlock, startOffset, endBlock, endOffset,
			extract.Priority, now.AddDate(0, 0, fuzzedDelay).Format(dateFormat),
			extract.Origin, externalRef, ranges,
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

		// Only a passage the reader actively pulled out while reading counts
		// as activity — a highlight arriving from a bulk sync (OriginImport)
		// is not something that happened today, whatever day it lands on.
		if extract.Origin == OriginManual {
			if err := logActivity(tx, ActivityExtract, id, "", now); err != nil {
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
// This also runs Backlog's reschedule, which is deliberately not a grade —
// see SaveScheduleReviewed for the write path that also logs activity.
func (s *Store) SaveSchedule(id int64, schedule ir.Schedule, now time.Time) error {
	return saveSchedule(s.db, id, schedule, now)
}

// SaveScheduleReviewed is SaveSchedule plus an activity_log row — the write
// path for an actual grading decision, as opposed to a backlog reschedule.
// Both happen in one transaction, so a review is never recorded without its
// schedule change landing, or vice versa.
func (s *Store) SaveScheduleReviewed(id int64, schedule ir.Schedule, now time.Time) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		if err := saveSchedule(tx, id, schedule, now); err != nil {
			return err
		}
		return logActivity(tx, ActivityReview, id, string(schedule.State), now)
	})
}

func saveSchedule(db dbtx, id int64, schedule ir.Schedule, now time.Time) error {
	var dueOn any
	if !schedule.DueOn.IsZero() {
		dueOn = schedule.DueOn.Format(dateFormat)
	}

	_, err := db.Exec(`
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

// Bury moves an element to the end of today's queue — or further, if it is
// already there; see Queue.
//
// Distinct from every other grade in leaving the schedule alone: it changes
// position within a day, not which day. buried_on records the date rather
// than a flag so it expires by itself — tomorrow the value no longer matches
// today. buried_at is the actual moment, always overwritten, so burying an
// element that is already buried today moves it again rather than doing
// nothing: it is what turns a single skip into a real round robin instead of
// a one-shot deferral.
func (s *Store) Bury(id int64, now time.Time) error {
	_, err := s.db.Exec(
		`UPDATE elements SET buried_on = ?, buried_at = ? WHERE id = ?`,
		now.Format(dateFormat), formatTime(now), id,
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

// RemapExternalRef repoints a local extract's provider identity from oldRef
// to newRef without touching anything else about the row — in particular
// none of the scheduling columns (due_on, interval_days, afactor, reps,
// priority, state) that anchor an incremental-reading history to it.
//
// This exists because wallabag has no way to update an annotation's location
// in place — UpdateHighlightLocation (internal/wallabag/write.go) can only
// give a highlight a position by creating a new annotation and deleting the
// old one, so the annotation id (increader's own external_ref) necessarily
// changes even though, semantically, it is still the very same highlight the
// reader already reviewed some number of times. Without this call, a
// re-anchor would look to increader exactly like the old annotation
// disappearing and an unrelated new one appearing — discarding the schedule
// built up on it — rather than what it actually is, one highlight moving.
//
// Both of the following are treated as success, not failure, which is what
// makes this safe to call again after a previous run died partway through:
//
//   - IsDuplicate: the partial unique index elements_external_ref
//     (document_id, external_ref) rejects the UPDATE when some other row
//     under this document already carries newRef. The most likely way that
//     happens is insertHighlights' own adopt-by-quote path (see "Not found
//     under this exact ref" above) having already matched oldRef's row by
//     its still-unchanged quote and moved it onto newRef itself, on a sync
//     that ran between the failed attempt and this retry. Either way the
//     end state this call exists to produce — this document's highlight
//     carrying newRef — already holds.
//   - Zero rows matched oldRef: either nothing local ever carried it (an
//     annotation that existed only upstream, never synced down before this
//     re-anchor touched it), or an earlier, since-interrupted run of this
//     exact call already completed it. Both are "nothing left to do".
//
// missing_upstream is cleared alongside the ref for the same reason
// insertHighlights' own adopt path clears it (elements.go, the `err == nil`
// case above): a highlight this call just repointed to a live, current
// upstream id is demonstrably not missing, whatever a Reconcile pass before
// this run may have flagged it as.
func (s *Store) RemapExternalRef(documentID int64, oldRef, newRef string) error {
	_, err := s.db.Exec(`
		UPDATE elements SET external_ref = ?, missing_upstream = 0
		WHERE document_id = ? AND external_ref = ?`,
		newRef, documentID, oldRef,
	)
	if err != nil {
		if IsDuplicate(err) {
			return nil
		}
		return fmt.Errorf("store: remap external ref %q to %q on document %d: %w",
			oldRef, newRef, documentID, err)
	}
	return nil
}

// RequeueDocumentRoot puts a document's root topic back in the reading
// queue after its body has been replaced with materially more text than it
// had before — the Substack backfill's actual point, not a side effect of
// it: a preview the operator dismissed, worked through to done, or parked
// suspended was a decision about the preview, not about the article that
// has since replaced it, and now that the real article exists there is
// something worth reading that was not there before.
//
// due_on is set to today unconditionally, regardless of whatever state this
// root was in beforehand — done, dismissed, suspended, anything. state
// becomes StateReading if the reader had already gotten partway through the
// preview (read_block > 0), so the queue treats this as "continue" rather
// than "start over", or StateNew otherwise.
//
// read_block itself is deliberately left untouched, and that is what makes
// the StateReading branch actually useful rather than cosmetic: Substack's
// paywall truncates the article body rather than re-rendering a shorter
// version of it (confirmed against real preview/full response pairs — the
// preview is a literal byte-prefix of the full body_html), so whatever
// block index the reader had reached in the preview sits at approximately
// the same point in the full article, right around where the paywall used
// to cut it off. Resuming there instead of at block 0 is the entire reason
// this call does not also reset reading progress.
//
// interval_days, afactor, reps and priority are left untouched for the same
// reason: this is a requeue, not a fresh import. The reader's past
// engagement with this article, however partial, is real history — the
// same history Suspend and Unsuspend already take care to preserve — not
// noise to discard just because the body underneath it grew.
//
// today carries no fuzz or spread at all, unlike an imported annotation's
// first due date (ir.FuzzedAnnotationDelay) or a fresh extract's
// (ir.FuzzedFirstDueDays). Both of those exist because extracts and
// imported highlights arrive in the hundreds per batch, and spreading them
// out is what stops one import from burying the reader's daily extract
// review under itself for weeks. A requeued article is not that kind of
// arrival: this call runs once per grown document, not hundreds of times
// per batch, and the article reading queue — unlike the bounded daily
// extract review — is allowed to be arbitrarily long. An operator who ran
// this backfill specifically to get an article's real text back in front of
// them gets exactly that by seeing it due today, not on some
// deterministically-scattered date nobody asked for. Fuzzing a value
// nothing here actually needs spread out would just be copying a pattern
// that solves a different problem.
//
// Only ever the root (parent_id IS NULL): an extract's own children keep
// their own schedule entirely untouched by this call — they are being
// re-anchored (see ClearExtractAnchors), not rescheduled, and requeuing the
// article itself has no bearing on when a highlight taken from it next
// comes up for review.
//
// Returns whether a root topic was actually found and updated — false for a
// document id with no root element at all, which should never happen for
// anything that went through UpsertDocuments, but is worth reporting to the
// caller rather than silently doing nothing.
func (s *Store) RequeueDocumentRoot(documentID int64, today time.Time) (bool, error) {
	result, err := s.db.Exec(`
		UPDATE elements SET
		    due_on = ?,
		    state = CASE WHEN read_block > 0 THEN ? ELSE ? END
		WHERE document_id = ? AND parent_id IS NULL`,
		today.Format(dateFormat), string(ir.StateReading), string(ir.StateNew),
		documentID,
	)
	if err != nil {
		return false, fmt.Errorf("store: requeue document %d: %w", documentID, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: count requeued rows for document %d: %w", documentID, err)
	}
	return changed > 0, nil
}

// ClearExtractAnchors forgets where a document's extracts sit in its body —
// NULLing start_block, start_offset, end_block, end_offset — so the next time
// the document is opened, anchorHighlights (internal/web/server.go) re-locates
// every one of them from scratch against whatever body is fetched then.
//
// This is the least obvious of the four store additions ingest needs, and the
// most important: skipping it after a content PATCH does not fail loudly, it
// fails silently.
//
// Why it is necessary. An extract's start_block/start_offset/end_block/
// end_offset were measured against the article body that existed in wallabag
// at the time it was anchored — for a Substack paywall preview that has since
// been backfilled with the real article, that is the *preview's* body, which
// is both shorter than and differently laid out from the full text about to
// replace it. anchorHighlights will not repair this on its own, for two
// independent reasons:
//
//   - Its pending filter (server.go, around line 363) only offers an extract
//     that has no start_block at all. One that is already anchored — however
//     stale that anchor now is — is skipped outright; there is no "anchored,
//     but check whether it should still be" path.
//   - Even routed past that filter somehow, the recovery check the same
//     function makes (server.go, around line 411) short-circuits when the
//     recovered *text* at the stored position is unchanged from what is
//     already saved. That check exists to catch a moved passage, but it
//     compares text, and text is exactly what does not change here — it is
//     the *position* that went stale, and a preview's offsets stay well
//     within bounds against a longer article (ir.Valid never trips), so
//     nothing about this ever surfaces as an error. The highlight silently
//     renders against whatever paragraph now happens to sit at the old
//     offset, which after an in-place preview-to-full-text swap is very
//     rarely the paragraph it was actually taken from.
//
// NULLing the four position columns is what defeats both guards at once:
// HasRange becomes false, which is exactly the "needs anchoring" state the
// pending filter is looking for, putting the extract back through
// AnchorExtract's own re-location on next open rather than through either of
// the checks above.
//
// Offsets only — content_html is deliberately left untouched. If the
// re-location that follows fails for any reason (the passage's quote no
// longer appears verbatim in the new body — the far edge of the same
// truncated-quote problem RemapExternalRef exists for, say), this extract
// degrades to a detached passage still showing its own last-known text,
// exactly as any other extract whose anchor search fails already does. Also
// clearing content_html here would instead degrade it to a blank one on that
// same failure, which is a strictly worse outcome for the reader for no
// benefit: the whole point of storing quote and content_html independently
// of the position is that the passage survives even when the position does
// not.
//
// Returns the number of extracts actually cleared — rows that already had no
// anchor (a root topic; an extract imported from a listing and never
// opened) do not count, which is also what makes a second call over the same
// document report zero rather than repeating work that already happened.
func (s *Store) ClearExtractAnchors(documentID int64) (int, error) {
	result, err := s.db.Exec(`
		UPDATE elements SET
		    start_block = NULL, start_offset = NULL, end_block = NULL, end_offset = NULL
		WHERE document_id = ? AND parent_id IS NOT NULL AND start_block IS NOT NULL`,
		documentID,
	)
	if err != nil {
		return 0, fmt.Errorf("store: clear extract anchors for document %d: %w", documentID, err)
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count cleared anchors for document %d: %w", documentID, err)
	}
	return int(cleared), nil
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

// highlightImport carries the choices insertHighlights needs beyond the
// highlights themselves.
//
// A sync leaves this zero: an annotation made in wallabag's own reader is one
// the reader already decided to make, and it goes straight into the queue on
// the usual delay. An uploaded book is the case this exists for. A single
// export routinely carries several hundred passages, and putting all of them
// in the queue at once buries everything else behind one book — the same
// failure importedPriority softens, at a scale it cannot. Parking them
// instead makes going through the book its own deliberate act.
type highlightImport struct {
	// floorDays is the fewest days ahead a queued annotation can first become
	// due; spreadDays is how much further out on top of that it might land,
	// drawn deterministically per highlight — see ir.FuzzedAnnotationDelay.
	// spreadDays of zero puts every highlight exactly on floorDays, with no
	// spread at all.
	floorDays  int
	spreadDays int

	// suspended parks new annotations out of the queue, awaiting triage.
	suspended bool

	// triaged marks new annotations as already decided about, so a triage
	// pass over the document does not offer them. Set when the reader chose
	// to queue the whole import outright, which is itself the decision.
	triaged bool
}

// insertHighlights turns a provider's annotations into extracts, skipping any
// already imported.
//
// The extracts are created unanchored: locating a quote needs the article body,
// and the listing that carries annotations deliberately omits it. That costs
// nothing while the parent stays archived, and AnchorExtract fills the position
// in later if the article is ever opened.
func insertHighlights(tx *sql.Tx, documentID, parentID int64, highlights []source.Highlight, options highlightImport, now time.Time) (int, error) {
	imported := 0

	for _, highlight := range highlights {
		quote := strings.TrimSpace(highlight.Quote)
		note := strings.TrimSpace(highlight.Note)

		// A passage-less annotation is still an annotation: a PDF sticky
		// note, or a highlight over a scanned page whose text could not be
		// recovered but which the reader wrote a comment on. Only one with
		// nothing in it at all is skipped.
		if (quote == "" && note == "") || highlight.ExternalID == "" {
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
			// Ranges is a column newer than every highlight already imported
			// before it existed, and re-importing an existing external_ref
			// never reaches the INSERT below that would otherwise set it —
			// so without this, a highlight synced before this column shipped
			// would carry a NULL ranges forever, never getting the one
			// chance anchorHighlights' recovery fallback needs. A listing
			// sync has annotation.Ranges even though it lacks the article
			// body itself, so this can run here rather than waiting for the
			// article to be opened. Guarded to a row that has none yet:
			// once backfilled, there is nothing left to update on any later
			// sync.
			if len(highlight.Ranges) > 0 {
				if _, err := tx.Exec(`
					UPDATE elements SET ranges = ?
					WHERE document_id = ? AND external_ref = ? AND ranges IS NULL`,
					string(highlight.Ranges), documentID, highlight.ExternalID,
				); err != nil {
					return imported, fmt.Errorf("store: backfill ranges for highlight %s: %w", highlight.ExternalID, err)
				}
			}

			// Chapter, page, note and colour are refreshed rather than left
			// alone, because re-uploading is how these are corrected: an
			// export redone after fixing a note in KOReader, or a PDF
			// re-read once its outline was added, should land those
			// corrections rather than be recognised as "already imported"
			// and discarded. The passage itself is deliberately not
			// refreshed — that is the reader's to edit here, and the
			// external ref already changes when the source's own text does.
			//
			// Guarded on the highlight actually carrying some of it, so a
			// wallabag sync — which carries none — does not issue a pointless
			// UPDATE per highlight per run, bumping updated_at on the whole
			// library every half hour.
			if hasBookMetadata(highlight) {
				if err := refreshAnnotation(tx, documentID, highlight, now); err != nil {
					return imported, err
				}
			}
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
		//
		// Skipped entirely for a passage-less annotation: matching on an
		// empty quote would make every note-only annotation in a document
		// look like every other one, and the first note imported would
		// swallow the ref of each one after it.
		var existingID int64
		err = sql.ErrNoRows
		if quote != "" {
			err = tx.QueryRow(`
				SELECT id FROM elements
				WHERE document_id = ? AND parent_id = ? AND quote = ?
				LIMIT 1`,
				documentID, parentID, quote,
			).Scan(&existingID)
		}
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

		var ranges any
		if len(highlight.Ranges) > 0 {
			ranges = string(highlight.Ranges)
		}

		// A parked annotation is suspended rather than merely undated,
		// because suspension already means exactly this everywhere else in
		// the application — present in the library, out of the queue, keeping
		// everything it knows — and triage's "keep this" is then an ordinary
		// unsuspend rather than a second concept.
		state, dueOn := string(ir.StateNew), any(
			// Seeded per highlight (documentID plus its own external ref),
			// not per document — a document's highlights are inserted in one
			// loop with a stable documentID, so seeding on documentID alone
			// used to compute the exact same offset for every highlight in a
			// single import and land them all on one day, the very pile-up
			// this exists to prevent. floorDays keeps a fresh import from
			// showing up too soon; spreadDays is what actually distributes a
			// batch across the following weeks — see ir.FuzzedAnnotationDelay.
			now.AddDate(0, 0, ir.FuzzedAnnotationDelay(
				highlightSeed(documentID, highlight.ExternalID),
				options.floorDays, options.spreadDays,
			)).Format(dateFormat))
		if options.suspended {
			state, dueOn = string(ir.StateSuspended), nil
		}

		var triagedAt any
		if options.triaged {
			triagedAt = formatTime(now)
		}

		_, err = tx.Exec(`
			INSERT INTO elements
			    (document_id, parent_id, kind, title, content_html, quote,
			     priority, state, due_on, interval_days, afactor, reps,
			     read_block, origin, external_ref, ranges, created_at, updated_at,
			     note, chapter, page, color, ordinal, triaged_at)
			VALUES (?, ?, 'topic', ?, ?, ?, ?, ?, ?, 0, 2.0, 0, 0, 'import', ?, ?, ?, ?,
			        ?, ?, ?, ?, ?, ?)`,
			documentID, parentID,
			annotationTitle(highlight),
			annotationHTML(quote, note),
			quote,
			importedPriority,
			state, dueOn,
			highlight.ExternalID, ranges,
			formatTime(now), formatTime(now),
			note, highlight.Chapter, highlight.Page, highlight.Color,
			highlight.Ordinal, triagedAt,
		)
		if err != nil {
			return imported, fmt.Errorf("store: import highlight %s: %w", highlight.ExternalID, err)
		}
		imported++
	}

	return imported, nil
}

// annotationTitle names an imported annotation for a list.
//
// A book annotation's chapter is a far better label than the first eighty
// characters of the passage, which is already shown directly underneath it —
// the same objection that made the reader's heading the article's title
// rather than the extract's. The passage is the fallback, and a note-only
// annotation falls back to the note.
func annotationTitle(highlight source.Highlight) string {
	if chapter := strings.TrimSpace(highlight.Chapter); chapter != "" {
		return SummariseQuote(chapter)
	}
	if quote := strings.TrimSpace(highlight.Quote); quote != "" {
		return SummariseQuote(quote)
	}
	return SummariseQuote(highlight.Note)
}

// annotationHTML renders a passage and the reader's note on it as the
// element's body.
//
// The note is marked up as a distinct block rather than run together with the
// passage: they are different kinds of thing — one the author's words, one the
// reader's — and a cloze deletion taken over the pair should be able to tell
// them apart.
func annotationHTML(quote, note string) string {
	var body strings.Builder
	if quote != "" {
		body.WriteString("<p>" + html.EscapeString(quote) + "</p>")
	}
	if note != "" {
		body.WriteString(`<p class="annotation-note">` + html.EscapeString(note) + "</p>")
	}
	return body.String()
}

// hasBookMetadata reports whether a highlight carries anything only a book
// import produces — the signal that re-importing it is worth a write.
func hasBookMetadata(highlight source.Highlight) bool {
	return highlight.Chapter != "" || highlight.Page != "" ||
		highlight.Color != "" || highlight.Ordinal > 0 ||
		strings.TrimSpace(highlight.Note) != ""
}

// refreshAnnotation updates an already-imported annotation's book metadata.
//
// Deliberately narrow about what it touches.
//
// Scoped to origin = 'import': a manual extract that adopted this ref carries
// a passage the reader chose by hand, and a re-import has no business
// rewriting it.
//
// Scoped to start_block IS NULL — an extract that has never been located in
// an article — because content_html for an anchored one is the article's own
// markup, links and all, recovered by AnchorExtract. Replacing that with an
// escaped plain-text paragraph would quietly downgrade every wallabag
// highlight the reader has ever opened.
//
// The stored title is left alone. Nothing here can tell a title increader
// generated from one a reader will eventually be able to edit, and the
// contents page groups on the chapter column rather than on titles, so
// refreshing it would buy nothing and cost that distinction.
func refreshAnnotation(tx *sql.Tx, documentID int64, highlight source.Highlight, now time.Time) error {
	quote := strings.TrimSpace(highlight.Quote)
	note := strings.TrimSpace(highlight.Note)

	_, err := tx.Exec(`
		UPDATE elements SET
		    note = ?, chapter = ?, page = ?, color = ?,
		    ordinal = CASE WHEN ? > 0 THEN ? ELSE ordinal END,
		    content_html = CASE WHEN start_block IS NULL THEN ? ELSE content_html END,
		    updated_at = ?
		WHERE document_id = ? AND external_ref = ? AND origin = 'import'`,
		note, highlight.Chapter, highlight.Page, highlight.Color,
		highlight.Ordinal, highlight.Ordinal,
		annotationHTML(quote, note),
		formatTime(now),
		documentID, highlight.ExternalID,
	)
	if err != nil {
		return fmt.Errorf("store: refresh annotation %s: %w", highlight.ExternalID, err)
	}
	return nil
}

// highlightSeed and extractSeed turn identifying data into the deterministic
// int64 seed ir.FuzzedAnnotationDelay and ir.FuzzedFirstDueDays need.
//
// Neither a freshly imported highlight nor a freshly made extract has an id
// yet at the point its first due date is computed — the id is assigned by the
// very INSERT the date is being computed for — so the seed has to come from
// data that already exists. Using the same identity each row is deduplicated
// on elsewhere in this file (document + external ref for a highlight, parent +
// quote for an extract) also means the fuzz is stable across a later re-import
// or resync: revisiting the same annotation recomputes the same seed and
// therefore the same offset, rather than reshuffling a date already set.
//
// fnv rather than the multiplicative-constant scramble used elsewhere in this
// file (the queue's tie-break, the old per-document spread this replaces):
// those scramble an int64 that is already effectively random relative to
// insertion order; this needs to hash a string.
func highlightSeed(documentID int64, externalID string) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d:%s", documentID, externalID)
	return int64(h.Sum64())
}

func extractSeed(parentID int64, quote string) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d:%s", parentID, quote)
	return int64(h.Sum64())
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

// UpdateAnnotation saves manual corrections to an extract's own passage, note
// and chapter — the editor a malformed PDF extraction (OCR noise, a
// mis-split sentence) or an uncorrected KOReader export often needs, and how
// a document with no outline of its own gets one by hand.
//
// content_html is rebuilt from quote and note the same way an import itself
// builds it (see annotationHTML), so an edited passage renders exactly as
// one freshly imported would — except when the element is anchored into an
// article's own body (start_block set), where content_html is the article's
// own markup rather than an escaped paragraph, and rewriting it here would
// quietly downgrade it. The same guard refreshAnnotation applies on
// re-import; editing a book annotation's passage never encounters it, since
// those are never anchored in the first place.
//
// Scoped to parent_id IS NOT NULL: a document's root topic has no passage or
// chapter of its own to edit.
func (s *Store) UpdateAnnotation(id int64, quote, note, chapter string, now time.Time) error {
	result, err := s.db.Exec(`
		UPDATE elements SET
		    quote = ?, note = ?, chapter = ?,
		    content_html = CASE WHEN start_block IS NULL THEN ? ELSE content_html END,
		    updated_at = ?
		WHERE id = ? AND parent_id IS NOT NULL`,
		quote, note, chapter, annotationHTML(quote, note), formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: update annotation %d: %w", id, err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("store: element %d: %w", id, ErrNotFound)
	}
	return nil
}

// SetAnnotationChapter overrides one annotation's chapter without touching
// its passage or note — the mass chapter edit's single-row primitive: a
// document with no outline needs its annotations grouped some other way,
// commonly a highlight colour standing in for a chapter heading, and this is
// what a bulk "set chapter" over a checked selection calls once per row.
//
// Scoped to parent_id IS NOT NULL, same as UpdateAnnotation.
func (s *Store) SetAnnotationChapter(id int64, chapter string, now time.Time) error {
	result, err := s.db.Exec(
		`UPDATE elements SET chapter = ?, updated_at = ? WHERE id = ? AND parent_id IS NOT NULL`,
		chapter, formatTime(now), id,
	)
	if err != nil {
		return fmt.Errorf("store: set chapter of element %d: %w", id, err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("store: element %d: %w", id, ErrNotFound)
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

// CountDue returns how many of one queue's elements are due on or before day.
//
// Takes a QueueKind for the same reason Queue does: the number under a queue's
// heading has to count that queue, not both of them.
func (s *Store) CountDue(day time.Time, kind QueueKind) (int, error) {
	predicate, err := kind.predicate()
	if err != nil {
		return 0, err
	}

	var count int
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM elements e
		WHERE `+predicate+`
		  AND e.state NOT IN ('done', 'dismissed', 'suspended')
		  AND (e.due_on IS NULL OR e.due_on <= ?)`,
		day.Format(dateFormat),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count due elements: %w", err)
	}
	return count, nil
}

// ExtractsPageLimit is how many rows the extracts browse page renders at once.
//
// A real cap rather than a token one: this page has no paging, so it is the
// whole of what a reader can reach in one view. It stays finite because a
// library import runs to thousands of passages and the filters above the list
// are the intended way through them — but the page reports the full match count
// alongside, so a reader is never left thinking a truncated list is all there is.
const ExtractsPageLimit = 200

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

// extractFilterWhere is the selection ExtractFilter describes, shared verbatim
// by the listing and by CountMatchingExtracts so the two cannot disagree about
// what matches — the same reason elementColumns is a single constant. A count
// that drifted from its list is exactly how a page ends up claiming to show
// "200 of 1431" when neither number describes the same set.
//
// Expects the elements table aliased e and documents aliased d, and the
// arguments extractFilterArgs returns, in that order.
const extractFilterWhere = `
	WHERE e.parent_id IS NOT NULL
	  AND e.state NOT IN ('dismissed')
	  AND (? = '' OR e.origin = ?)
	  AND (? = 0 OR e.kind = 'item')
	  AND (? = 0 OR e.missing_upstream = 1)
	  AND (? = '' OR e.quote LIKE ? OR d.title LIKE ?)`

// extractFilterArgs returns the bound parameters extractFilterWhere expects.
func extractFilterArgs(filter ExtractFilter) []any {
	pattern := "%" + filter.Query + "%"
	return []any{
		filter.Origin, filter.Origin,
		filter.WithClozes,
		filter.MissingOnly,
		filter.Query, pattern, pattern,
	}
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
// The extract queue answers "which passages are due now", in priority order
// and capped at a day's worth; this answers "what have I harvested", over
// everything regardless of when it is next due. Different question, so its own
// ordering — newest first by default, because the thing you just pulled out is
// the thing you most likely want, but see ExtractFilter.Sort for the rest.
//
// ExtractsPageLimit rows unless the filter says otherwise. The page pairs this
// with CountMatchingExtracts so that a truncated list can say so — a cap this
// page cannot see past is fine, one it hides is not.
func (s *Store) Extracts(filter ExtractFilter) ([]ExtractRow, error) {
	if filter.Limit <= 0 {
		filter.Limit = ExtractsPageLimit
	}

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
		SELECT `+elementColumns+`, COALESCE(NULLIF(d.display_title, ''), d.title), d.url,
		       (SELECT COUNT(*) FROM cloze_ranges c WHERE c.element_id = e.id)
		FROM elements e
		JOIN documents d ON d.id = e.document_id`+extractFilterWhere+`
		ORDER BY `+orderBy+`
		LIMIT ?`,
		append(extractFilterArgs(filter), filter.Limit)...,
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

// CountMatchingExtracts returns how many extracts a filter matches in total,
// ignoring its Limit — what the browse page compares its rendered row count
// against to know whether it has truncated, and by how much.
//
// Shares extractFilterWhere with the listing itself rather than restating the
// predicate, so the two can never come to describe different sets.
func (s *Store) CountMatchingExtracts(filter ExtractFilter) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*)
		FROM elements e
		JOIN documents d ON d.id = e.document_id`+extractFilterWhere,
		extractFilterArgs(filter)...,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count matching extracts: %w", err)
	}
	return count, nil
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
