package store

import (
	"database/sql"
	"fmt"
)

// Tag is a label attached to documents.
type Tag struct {
	ID         int64
	Source     string
	ExternalID string
	Label      string
	Slug       string
	Documents  int
}

// setDocumentTags replaces a document's tag set with labels.
//
// Wholesale replacement rather than a diff, because a provider listing is
// authoritative: a tag removed upstream is absent from the payload, and merging
// would leave it attached here forever with nothing to say it was ever removed.
func setDocumentTags(tx *sql.Tx, sourceName string, documentID int64, labels []string) error {
	if _, err := tx.Exec(`DELETE FROM document_tags WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("store: clear tags of document %d: %w", documentID, err)
	}

	for _, label := range labels {
		if label == "" {
			continue
		}

		tagID, err := upsertTag(tx, sourceName, label)
		if err != nil {
			return err
		}
		_, err = tx.Exec(
			`INSERT OR IGNORE INTO document_tags (document_id, tag_id) VALUES (?, ?)`,
			documentID, tagID,
		)
		if err != nil {
			return fmt.Errorf("store: attach tag %q to document %d: %w", label, documentID, err)
		}
	}
	return nil
}

// upsertTag returns the id for a label, creating the tag if it is new.
func upsertTag(tx *sql.Tx, sourceName, label string) (int64, error) {
	var id int64
	err := tx.QueryRow(
		`SELECT id FROM tags WHERE source = ? AND label = ?`, sourceName, label,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("store: look up tag %q: %w", label, err)
	}

	outcome, err := tx.Exec(
		`INSERT INTO tags (source, label, slug) VALUES (?, ?, '')`, sourceName, label,
	)
	if err != nil {
		return 0, fmt.Errorf("store: create tag %q: %w", label, err)
	}
	return outcome.LastInsertId()
}

// TagsOf returns a document's labels, alphabetically.
func (s *Store) TagsOf(documentID int64) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT t.label
		FROM tags t
		JOIN document_tags dt ON dt.tag_id = t.id
		WHERE dt.document_id = ?
		ORDER BY t.label`, documentID)
	if err != nil {
		return nil, fmt.Errorf("store: read tags of document %d: %w", documentID, err)
	}
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("store: scan tag: %w", err)
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

// AllTags lists every tag with how many documents carry it, busiest first.
func (s *Store) AllTags() ([]Tag, error) {
	rows, err := s.db.Query(`
		SELECT t.id, t.source, t.external_id, t.label, t.slug,
		       COUNT(dt.document_id)
		FROM tags t
		LEFT JOIN document_tags dt ON dt.tag_id = t.id
		GROUP BY t.id
		ORDER BY COUNT(dt.document_id) DESC, t.label`)
	if err != nil {
		return nil, fmt.Errorf("store: list tags: %w", err)
	}
	defer rows.Close()

	var tags []Tag
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.ID, &tag.Source, &tag.ExternalID,
			&tag.Label, &tag.Slug, &tag.Documents); err != nil {
			return nil, fmt.Errorf("store: scan tag row: %w", err)
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

// AttachTag adds a label to a document locally and queues the write upstream.
//
// Both happen in one transaction: a tag shown here but never sent, or sent but
// not shown, are equally confusing and the transaction makes both impossible.
func (s *Store) AttachTag(documentID int64, sourceName, externalID, label string) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		tagID, err := upsertTag(tx, sourceName, label)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO document_tags (document_id, tag_id) VALUES (?, ?)`,
			documentID, tagID,
		); err != nil {
			return fmt.Errorf("store: attach tag %q: %w", label, err)
		}
		return enqueueWrite(tx, sourceName, externalID, OpTagAdd, label)
	})
}

// DetachTag removes a label from a document locally and queues the removal.
func (s *Store) DetachTag(documentID int64, sourceName, externalID, label string) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			DELETE FROM document_tags
			WHERE document_id = ?
			  AND tag_id IN (SELECT id FROM tags WHERE source = ? AND label = ?)`,
			documentID, sourceName, label,
		)
		if err != nil {
			return fmt.Errorf("store: detach tag %q: %w", label, err)
		}
		return enqueueWrite(tx, sourceName, externalID, OpTagRemove, label)
	})
}
