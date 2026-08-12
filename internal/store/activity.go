package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
)

// Activity kinds recorded in activity_log.
const (
	ActivityReview  = "review"
	ActivityExtract = "extract"
)

// DayCount is one day's activity tally, for the calendar grid. Zero-filled
// for days with no rows, so a caller can lay out a fixed grid without
// checking for gaps.
type DayCount struct {
	Date time.Time

	// Articles is how many distinct articles were read that day: documents
	// whose *root* topic had a review logged. Reviews of extracts are counted
	// separately, in ExtractReviews, rather than folded in here — re-reading
	// a passage harvested last month is not the same act as reading an
	// article, and "articles read" is the number the calendar exists to show.
	Articles int

	// Finished is the subset of Articles whose review that day landed on
	// StateDone: read to the end, as opposed to merely picked up again.
	Finished int

	// Reviews counts every grading pass on an article, several on the same
	// article in one sitting included, so Reviews >= Articles always.
	Reviews int

	// ExtractReviews counts grading passes on extracts — the other queue's
	// half of the day's work.
	ExtractReviews int

	// Extracts is how many passages were harvested that day.
	Extracts int

	// Words is the word count of every extract harvested that day, summed.
	// Extracts rather than whole articles: an extract's quote is stored
	// verbatim plain text (see elements.quote), so this is exact and needs no
	// HTML parsing, unlike a root topic's content_html. It reads as "how much
	// did I pull out today" — the incremental-reading counterpart to a
	// reading-time estimate.
	Words int
}

// Rollup is the same tally over a range of days rather than one day.
//
// Articles and Finished are distinct *across the whole range*, not the sum of
// each day's own distinct count: an article read on Monday and again on
// Thursday is one article this week, not two. That is the difference between
// this and adding up DayCounts, and the reason a caller wanting a weekly or
// monthly figure should ask for it here rather than summing.
type Rollup struct {
	Articles       int
	Finished       int
	Reviews        int
	ExtractReviews int
	Extracts       int
	Words          int

	// ActiveDays is how many days in the range had anything logged at all —
	// the denominator for "you read on 12 of 30 days".
	ActiveDays int
}

// MonthCount is one calendar month's Rollup, labelled by the month's first
// day. Zero-filled across the requested range, like ActivityHeatmap.
type MonthCount struct {
	Month time.Time
	Rollup
}

// activityTallies is the aggregate column list shared by the per-day,
// per-month and whole-range queries, so all three count the same things the
// same way — the one place "what counts as an article read" is decided.
//
// The sums are COALESCEd because ActivityBetween has no GROUP BY: over a
// range with nothing logged, SQLite still returns one row, and SUM of no
// rows is NULL rather than 0.
const activityTallies = `
	COUNT(DISTINCT CASE WHEN a.kind = '` + ActivityReview + `' AND e.parent_id IS NULL
	                    THEN e.document_id END),
	COUNT(DISTINCT CASE WHEN a.kind = '` + ActivityReview + `' AND e.parent_id IS NULL
	                     AND a.grade = '` + string(ir.StateDone) + `' THEN e.document_id END),
	COALESCE(SUM(CASE WHEN a.kind = '` + ActivityReview + `' AND e.parent_id IS NULL THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN a.kind = '` + ActivityReview + `' AND e.parent_id IS NOT NULL THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN a.kind = '` + ActivityExtract + `' THEN 1 ELSE 0 END), 0)`

// logActivity records one activity_log row. It takes a dbtx rather than the
// Store itself so it can run inside an existing transaction — the write path
// for both a graded review (SaveScheduleReviewed) and a manually harvested
// extract (CreateExtract) already has one open, and the schedule/extract
// change and its log entry should land together or not at all.
func logActivity(tx dbtx, kind string, elementID int64, grade string, now time.Time) error {
	var gradeValue any
	if grade != "" {
		gradeValue = grade
	}
	_, err := tx.Exec(`
		INSERT INTO activity_log (kind, element_id, grade, occurred_on, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		kind, elementID, gradeValue, now.Format(dateFormat), formatTime(now),
	)
	if err != nil {
		return fmt.Errorf("store: log %s activity for element %d: %w", kind, elementID, err)
	}
	return nil
}

// CurrentStreak counts consecutive days, ending today, on which at least one
// review was graded.
//
// If today has no review yet, the streak is counted from yesterday instead —
// otherwise it would read as broken every morning before the reader has had
// a chance to open the queue, rather than only once a day is actually missed.
func (s *Store) CurrentStreak(today time.Time) (int, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT occurred_on FROM activity_log
		WHERE kind = ? ORDER BY occurred_on DESC`,
		ActivityReview,
	)
	if err != nil {
		return 0, fmt.Errorf("store: current streak: %w", err)
	}
	defer rows.Close()

	days := map[string]bool{}
	for rows.Next() {
		var day string
		if err := rows.Scan(&day); err != nil {
			return 0, fmt.Errorf("store: current streak: %w", err)
		}
		days[day] = true
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: current streak: %w", err)
	}

	cursor := today
	if !days[cursor.Format(dateFormat)] {
		cursor = cursor.AddDate(0, 0, -1)
	}

	streak := 0
	for days[cursor.Format(dateFormat)] {
		streak++
		cursor = cursor.AddDate(0, 0, -1)
	}
	return streak, nil
}

// ActivityHeatmap returns one DayCount per day in [from, to], inclusive of
// both ends, in ascending date order — even days with no activity, so the
// caller can lay out a fixed grid rather than reasoning about gaps.
func (s *Store) ActivityHeatmap(from, to time.Time) ([]DayCount, error) {
	rows, err := s.db.Query(`
		SELECT a.occurred_on, `+activityTallies+`
		FROM activity_log a
		JOIN elements e ON e.id = a.element_id
		WHERE a.occurred_on BETWEEN ? AND ?
		GROUP BY a.occurred_on`,
		from.Format(dateFormat), to.Format(dateFormat),
	)
	if err != nil {
		return nil, fmt.Errorf("store: activity heatmap: %w", err)
	}
	defer rows.Close()

	byDay := map[string]DayCount{}
	for rows.Next() {
		var day string
		var c DayCount
		if err := rows.Scan(&day, &c.Articles, &c.Finished,
			&c.Reviews, &c.ExtractReviews, &c.Extracts); err != nil {
			return nil, fmt.Errorf("store: activity heatmap: %w", err)
		}
		byDay[day] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: activity heatmap: %w", err)
	}

	words, err := s.wordsPerDay(from, to)
	if err != nil {
		return nil, err
	}

	var out []DayCount
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		key := day.Format(dateFormat)
		count := byDay[key]
		count.Date = day
		count.Words = words[key]
		out = append(out, count)
	}
	return out, nil
}

// ActivityBetween rolls the whole of [from, to] into a single tally, with
// Articles counted distinct across the range rather than per day — see Rollup.
func (s *Store) ActivityBetween(from, to time.Time) (Rollup, error) {
	var rollup Rollup
	err := s.db.QueryRow(`
		SELECT `+activityTallies+`, COUNT(DISTINCT a.occurred_on)
		FROM activity_log a
		JOIN elements e ON e.id = a.element_id
		WHERE a.occurred_on BETWEEN ? AND ?`,
		from.Format(dateFormat), to.Format(dateFormat),
	).Scan(&rollup.Articles, &rollup.Finished, &rollup.Reviews,
		&rollup.ExtractReviews, &rollup.Extracts, &rollup.ActiveDays)
	if err != nil {
		return Rollup{}, fmt.Errorf("store: activity between %s and %s: %w",
			from.Format(dateFormat), to.Format(dateFormat), err)
	}

	words, err := s.wordsPerDay(from, to)
	if err != nil {
		return Rollup{}, err
	}
	for _, count := range words {
		rollup.Words += count
	}
	return rollup, nil
}

// ActivityByMonth rolls activity into calendar months, oldest first, one
// entry per month touched by [from, to] whether or not anything was logged in
// it — the calendar's twelve-month strip needs a fixed set of columns for the
// same reason its day grid needs a fixed set of cells.
func (s *Store) ActivityByMonth(from, to time.Time) ([]MonthCount, error) {
	rows, err := s.db.Query(`
		SELECT substr(a.occurred_on, 1, 7), `+activityTallies+`, COUNT(DISTINCT a.occurred_on)
		FROM activity_log a
		JOIN elements e ON e.id = a.element_id
		WHERE a.occurred_on BETWEEN ? AND ?
		GROUP BY substr(a.occurred_on, 1, 7)`,
		from.Format(dateFormat), to.Format(dateFormat),
	)
	if err != nil {
		return nil, fmt.Errorf("store: activity by month: %w", err)
	}
	defer rows.Close()

	byMonth := map[string]Rollup{}
	for rows.Next() {
		var month string
		var rollup Rollup
		if err := rows.Scan(&month, &rollup.Articles, &rollup.Finished, &rollup.Reviews,
			&rollup.ExtractReviews, &rollup.Extracts, &rollup.ActiveDays); err != nil {
			return nil, fmt.Errorf("store: activity by month: %w", err)
		}
		byMonth[month] = rollup
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: activity by month: %w", err)
	}

	words, err := s.wordsPerDay(from, to)
	if err != nil {
		return nil, err
	}
	wordsByMonth := map[string]int{}
	for day, count := range words {
		wordsByMonth[day[:len(monthFormat)]] += count
	}

	var out []MonthCount
	first := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, from.Location())
	last := time.Date(to.Year(), to.Month(), 1, 0, 0, 0, 0, to.Location())
	for month := first; !month.After(last); month = month.AddDate(0, 1, 0) {
		key := month.Format(monthFormat)
		rollup := byMonth[key]
		rollup.Words = wordsByMonth[key]
		out = append(out, MonthCount{Month: month, Rollup: rollup})
	}
	return out, nil
}

// ArticleRead is one article read on one day: the pair a caller needs to
// count distinct articles over any window it likes.
type ArticleRead struct {
	Day        time.Time
	DocumentID int64
}

// ArticlesReadBetween returns one row per (day, article) pair in [from, to],
// oldest first — every document whose root topic was reviewed that day, once
// per day it was read.
//
// The rollups above answer "how many articles in this range" for one range;
// this is for a caller bucketing the same range several ways at once (the
// dashboard's twelve weekly bars, plus its this-week and last-week figures)
// and wanting each bucket's count to be a true distinct count rather than a
// sum of daily ones. Deduplication is left to the caller, which is doing it
// per bucket anyway.
func (s *Store) ArticlesReadBetween(from, to time.Time) ([]ArticleRead, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT a.occurred_on, e.document_id
		FROM activity_log a
		JOIN elements e ON e.id = a.element_id
		WHERE a.kind = ? AND e.parent_id IS NULL
		  AND a.occurred_on BETWEEN ? AND ?
		ORDER BY a.occurred_on`,
		ActivityReview, from.Format(dateFormat), to.Format(dateFormat),
	)
	if err != nil {
		return nil, fmt.Errorf("store: articles read: %w", err)
	}
	defer rows.Close()

	var reads []ArticleRead
	for rows.Next() {
		var (
			day  string
			read ArticleRead
		)
		if err := rows.Scan(&day, &read.DocumentID); err != nil {
			return nil, fmt.Errorf("store: articles read: %w", err)
		}
		parsed, err := time.ParseInLocation(dateFormat, day, time.Local)
		if err != nil {
			return nil, fmt.Errorf("store: articles read: bad date %q: %w", day, err)
		}
		read.Day = parsed
		reads = append(reads, read)
	}
	return reads, rows.Err()
}

// monthFormat is a bare year-and-month, the prefix of dateFormat that
// substr(occurred_on, 1, 7) picks out in the queries above.
const monthFormat = "2006-01"

// wordsPerDay sums the word count of every extract logged in [from, to],
// keyed by occurred_on. A quote's word count is computed in Go rather than
// SQL — SQLite has no word-splitting built in, and the quote is already
// plain text (see elements.quote), so len(strings.Fields(...)) is exact.
func (s *Store) wordsPerDay(from, to time.Time) (map[string]int, error) {
	rows, err := s.db.Query(`
		SELECT a.occurred_on, e.quote
		FROM activity_log a
		JOIN elements e ON e.id = a.element_id
		WHERE a.kind = ? AND a.occurred_on BETWEEN ? AND ?`,
		ActivityExtract, from.Format(dateFormat), to.Format(dateFormat),
	)
	if err != nil {
		return nil, fmt.Errorf("store: words per day: %w", err)
	}
	defer rows.Close()

	words := map[string]int{}
	for rows.Next() {
		var day, quote string
		if err := rows.Scan(&day, &quote); err != nil {
			return nil, fmt.Errorf("store: words per day: %w", err)
		}
		words[day] += wordCount(quote)
	}
	return words, rows.Err()
}

// wordCount is the plain word count of a passage of text — split on
// whitespace, the same way a word processor's count does.
func wordCount(text string) int {
	return len(strings.Fields(text))
}

// dbtx is satisfied by both *sql.DB and *sql.Tx, so the same helper can write
// standalone or as part of a larger transaction. Declared here, alongside its
// first use, rather than in store.go — nothing else needs it yet.
type dbtx interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// ActivityEntry is one activity_log row together with the element and
// document it happened on — what the calendar's day view shows.
type ActivityEntry struct {
	Element
	DocumentTitle string
	DocumentURL   string

	// Kind and Grade are the activity_log row's own columns, not the
	// element's current schedule: Grade is the state a review actually
	// landed on at the time, which for an element read again since need not
	// still match what the element's live schedule says now.
	Kind  string
	Grade string

	// OccurredAt is when the activity happened, not when the element was
	// created — activity_log's own timestamp, for ordering entries within
	// the day chronologically.
	OccurredAt time.Time

	// Words is the extract's word count for a Kind == ActivityExtract row,
	// and 0 for a review — a root topic has no quote of its own to count.
	Words int
}

// ActivityOn lists everything that happened on one day — every review graded
// and every extract manually harvested, oldest first — the calendar's day
// view. An element hard-deleted since cannot appear: activity_log cascades
// off elements(id), so its own row went with it.
func (s *Store) ActivityOn(day time.Time) ([]ActivityEntry, error) {
	rows, err := s.db.Query(`
		SELECT `+elementColumns+`, COALESCE(NULLIF(d.display_title, ''), d.title), d.url,
		       a.kind, COALESCE(a.grade, ''), a.created_at
		FROM activity_log a
		JOIN elements e ON e.id = a.element_id
		JOIN documents d ON d.id = e.document_id
		WHERE a.occurred_on = ?
		ORDER BY a.created_at`,
		day.Format(dateFormat),
	)
	if err != nil {
		return nil, fmt.Errorf("store: activity on %s: %w", day.Format(dateFormat), err)
	}
	defer rows.Close()

	var entries []ActivityEntry
	for rows.Next() {
		var (
			row        ActivityEntry
			nullable   nullableElement
			occurredAt sql.NullString
		)
		targets := append(scanTargets(&row.Element, &nullable),
			&row.DocumentTitle, &row.DocumentURL, &row.Kind, &row.Grade, &occurredAt)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("store: scan activity row: %w", err)
		}
		nullable.apply(&row.Element)
		row.OccurredAt = parseTime(occurredAt)
		if row.Kind == ActivityExtract {
			row.Words = wordCount(row.Quote)
		}
		entries = append(entries, row)
	}
	return entries, rows.Err()
}
