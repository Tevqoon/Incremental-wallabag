package store

import (
	"database/sql"
	"fmt"
	"time"
)

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
		  AND due_on IS NOT NULL
		  AND due_on <= ?`,
		day.Format(dateFormat),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count due elements: %w", err)
	}
	return count, nil
}
