package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Operations increader can push back to a provider.
const (
	OpArchive   = "archive"
	OpStar      = "star"
	OpTagAdd    = "tag_add"
	OpTagRemove = "tag_remove"
)

// maxWriteAttempts caps retries so a write that can never succeed stops
// consuming a request on every sync. It is kept rather than deleted, so the
// failure stays visible instead of vanishing.
const maxWriteAttempts = 8

// PendingWrite is one queued change to a provider.
type PendingWrite struct {
	ID         int64
	Source     string
	ExternalID string
	Operation  string
	Payload    string
	Attempts   int
	LastError  string
	CreatedAt  time.Time
}

// inTransaction runs fn in a transaction, committing on success.
//
// The pattern exists because every write-back pairs a local change with an
// outbox row, and those two must land together — the entire reason the outbox
// is worth having rather than calling the API directly from a handler.
func (s *Store) inTransaction(fn func(*sql.Tx) error) error {
	transaction, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	defer transaction.Rollback()

	if err := fn(transaction); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}
	return nil
}

// enqueueWrite records an intent to change the provider.
//
// Takes a transaction rather than opening its own, so callers can commit it
// alongside the local change it corresponds to.
func enqueueWrite(tx *sql.Tx, sourceName, externalID, operation, payload string) error {
	if externalID == "" {
		// A document with no provider identity — none exist today, but a
		// locally created one would — has nothing to write back to.
		return nil
	}

	// Supersede any queued write of the same operation on the same entry. Only
	// the final state matters: archiving, unarchiving and archiving again
	// should send one request, not three, and replaying the intermediate ones
	// would briefly put wallabag into states the reader never asked for.
	_, err := tx.Exec(`
		DELETE FROM pending_writes
		WHERE source = ? AND external_id = ? AND operation = ?
		  AND (? IN (?, ?) OR payload = ?)`,
		sourceName, externalID, operation,
		operation, OpArchive, OpStar, payload,
	)
	if err != nil {
		return fmt.Errorf("store: supersede queued write: %w", err)
	}

	_, err = tx.Exec(`
		INSERT INTO pending_writes (source, external_id, operation, payload, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		sourceName, externalID, operation, payload, formatTime(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("store: queue %s write: %w", operation, err)
	}
	return nil
}

// EnqueueWrite queues a provider change on its own, for callers that have no
// other local change to pair it with.
func (s *Store) EnqueueWrite(sourceName, externalID, operation, payload string) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		return enqueueWrite(tx, sourceName, externalID, operation, payload)
	})
}

// PendingWrites returns queued writes for a source, oldest first.
//
// Order matters: two writes to the same entry must reach the provider in the
// order they were made, or the older one wins.
func (s *Store) PendingWrites(sourceName string, limit int) ([]PendingWrite, error) {
	rows, err := s.db.Query(`
		SELECT id, source, external_id, operation, payload, attempts, last_error, created_at
		FROM pending_writes
		WHERE source = ? AND attempts < ?
		ORDER BY id
		LIMIT ?`,
		sourceName, maxWriteAttempts, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: read pending writes: %w", err)
	}
	defer rows.Close()

	var writes []PendingWrite
	for rows.Next() {
		var (
			write     PendingWrite
			createdAt sql.NullString
		)
		if err := rows.Scan(&write.ID, &write.Source, &write.ExternalID,
			&write.Operation, &write.Payload, &write.Attempts,
			&write.LastError, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan pending write: %w", err)
		}
		write.CreatedAt = parseTime(createdAt)
		writes = append(writes, write)
	}
	return writes, rows.Err()
}

// CompleteWrite removes a write that reached the provider.
func (s *Store) CompleteWrite(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM pending_writes WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: complete write %d: %w", id, err)
	}
	return nil
}

// FailWrite records an attempt that did not reach the provider.
func (s *Store) FailWrite(id int64, cause error) error {
	_, err := s.db.Exec(
		`UPDATE pending_writes SET attempts = attempts + 1, last_error = ? WHERE id = ?`,
		cause.Error(), id,
	)
	if err != nil {
		return fmt.Errorf("store: record failure of write %d: %w", id, err)
	}
	return nil
}

// CountPendingWrites reports how many writes are queued and how many have
// exhausted their retries, so the state is visible rather than only in a log.
func (s *Store) CountPendingWrites(sourceName string) (queued, abandoned int, err error) {
	err = s.db.QueryRow(`
		SELECT
		    SUM(CASE WHEN attempts <  ? THEN 1 ELSE 0 END),
		    SUM(CASE WHEN attempts >= ? THEN 1 ELSE 0 END)
		FROM pending_writes WHERE source = ?`,
		maxWriteAttempts, maxWriteAttempts, sourceName,
	).Scan(&queued, &abandoned)
	if err != nil {
		return 0, 0, fmt.Errorf("store: count pending writes: %w", err)
	}
	return queued, abandoned, nil
}

// SetArchived records an article's read state locally and queues it upstream.
//
// The local column is updated in the same breath so the next sync sees no
// change and the M6 archive transition does not fire on increader's own write —
// which would otherwise suspend an element that was just marked done.
func (s *Store) SetArchived(documentID int64, sourceName, externalID string, archived bool, now time.Time) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`UPDATE documents SET is_archived = ? WHERE id = ?`, archived, documentID,
		); err != nil {
			return fmt.Errorf("store: set archived on document %d: %w", documentID, err)
		}
		return enqueueWrite(tx, sourceName, externalID, OpArchive, boolPayload(archived))
	})
}

// SetStarred records an article's favourite state locally and queues it upstream.
func (s *Store) SetStarred(documentID int64, sourceName, externalID string, starred bool, now time.Time) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`UPDATE documents SET is_starred = ? WHERE id = ?`, starred, documentID,
		); err != nil {
			return fmt.Errorf("store: set starred on document %d: %w", documentID, err)
		}
		return enqueueWrite(tx, sourceName, externalID, OpStar, boolPayload(starred))
	})
}

// boolPayload renders a flag for the outbox.
func boolPayload(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// PayloadBool reads a flag back out of the outbox.
func PayloadBool(payload string) bool { return payload == "1" }
