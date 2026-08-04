package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Activity kinds recorded in activity_log.
const (
	ActivityReview  = "review"
	ActivityExtract = "extract"
)

// DayCount is one day's activity tally, for the dashboard's heatmap and
// weekly charts. Zero-filled for days with no rows, so a caller can lay out
// a fixed grid without checking for gaps.
type DayCount struct {
	Date     time.Time
	Reviews  int
	Extracts int
}

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
		SELECT occurred_on,
		    SUM(CASE WHEN kind = ? THEN 1 ELSE 0 END),
		    SUM(CASE WHEN kind = ? THEN 1 ELSE 0 END)
		FROM activity_log
		WHERE occurred_on BETWEEN ? AND ?
		GROUP BY occurred_on`,
		ActivityReview, ActivityExtract,
		from.Format(dateFormat), to.Format(dateFormat),
	)
	if err != nil {
		return nil, fmt.Errorf("store: activity heatmap: %w", err)
	}
	defer rows.Close()

	type counts struct{ reviews, extracts int }
	byDay := map[string]counts{}
	for rows.Next() {
		var day string
		var c counts
		if err := rows.Scan(&day, &c.reviews, &c.extracts); err != nil {
			return nil, fmt.Errorf("store: activity heatmap: %w", err)
		}
		byDay[day] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: activity heatmap: %w", err)
	}

	var out []DayCount
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		c := byDay[day.Format(dateFormat)]
		out = append(out, DayCount{Date: day, Reviews: c.reviews, Extracts: c.extracts})
	}
	return out, nil
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
		entries = append(entries, row)
	}
	return entries, rows.Err()
}
