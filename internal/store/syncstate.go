package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SyncState is the incremental-sync bookmark for one source.
type SyncState struct {
	Source string

	// Watermark is the newest provider-side update time already imported. The
	// next sync asks the provider for everything at or after this. It is
	// provider time, never local time: the two clocks disagree, and using the
	// local one silently skips records.
	Watermark time.Time

	LastRun   time.Time
	LastError string
}

// SyncState reads a source's bookmark. A source that has never synced returns
// a zero-valued state, which means "fetch everything".
func (s *Store) SyncState(sourceName string) (SyncState, error) {
	var (
		state     = SyncState{Source: sourceName}
		watermark sql.NullString
		lastRun   sql.NullString
	)

	err := s.db.QueryRow(
		`SELECT watermark, last_run, last_error FROM sync_state WHERE source = ?`,
		sourceName,
	).Scan(&watermark, &lastRun, &state.LastError)

	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("store: read sync state for %s: %w", sourceName, err)
	}

	state.Watermark = parseTime(watermark)
	state.LastRun = parseTime(lastRun)
	return state, nil
}

// SaveSyncState writes a source's bookmark, inserting it on first use.
func (s *Store) SaveSyncState(state SyncState) error {
	_, err := s.db.Exec(`
		INSERT INTO sync_state (source, watermark, last_run, last_error)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (source) DO UPDATE SET
		    watermark  = excluded.watermark,
		    last_run   = excluded.last_run,
		    last_error = excluded.last_error`,
		state.Source, formatTime(state.Watermark),
		formatTime(state.LastRun), state.LastError,
	)
	if err != nil {
		return fmt.Errorf("store: save sync state for %s: %w", state.Source, err)
	}
	return nil
}

// ResetWatermark clears a source's bookmark so the next sync re-reads
// everything.
//
// Needed whenever increader starts caring about a field it did not store
// before: incremental sync only asks for entries changed since the watermark,
// so a library that is already up to date would never see the new field at all.
func (s *Store) ResetWatermark(sourceName string) error {
	_, err := s.db.Exec(`UPDATE sync_state SET watermark = NULL WHERE source = ?`, sourceName)
	if err != nil {
		return fmt.Errorf("store: reset watermark for %s: %w", sourceName, err)
	}
	return nil
}
