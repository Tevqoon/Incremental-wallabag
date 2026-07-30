// Package syncer pulls documents from sources into the store.
//
// It is the only place that knows both halves: it depends on the source
// interface and on storage, but on no specific provider. Adding wallabag,
// KOReader or anything else changes main's wiring, not this package.
package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/store"
)

// Syncer imports from a fixed set of sources.
type Syncer struct {
	store   *store.Store
	sources []source.Source
	logger  *slog.Logger
}

// New builds a syncer over the given sources.
func New(db *store.Store, logger *slog.Logger, sources ...source.Source) *Syncer {
	return &Syncer{store: db, sources: sources, logger: logger}
}

// Result summarises one source's sync.
type Result struct {
	Source  string
	Fetched int
	Created int
	Updated int
}

// SyncAll syncs every configured source.
//
// One failing source does not abort the others: a wallabag outage should not
// stop a local KOReader import from running. The first error is returned once
// every source has had its turn.
func (s *Syncer) SyncAll(ctx context.Context) ([]Result, error) {
	var (
		results  []Result
		firstErr error
	)

	for _, provider := range s.sources {
		result, err := s.Sync(ctx, provider)
		if err != nil {
			s.logger.Error("sync failed", "source", provider.Name(), "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		results = append(results, result)
	}
	return results, firstErr
}

// Sync imports everything one source has changed since its watermark.
func (s *Syncer) Sync(ctx context.Context, provider source.Source) (Result, error) {
	name := provider.Name()

	state, err := s.store.SyncState(name)
	if err != nil {
		return Result{}, err
	}

	s.logger.Info("sync starting", "source", name, "since", watermarkForLog(state.Watermark))

	documents, err := provider.Fetch(ctx, state.Watermark)
	if err != nil {
		// Record the failure so it is visible without reading logs, but keep
		// the old watermark: advancing it past documents that were never
		// imported would lose them permanently.
		state.LastRun = time.Now()
		state.LastError = err.Error()
		if saveErr := s.store.SaveSyncState(state); saveErr != nil {
			s.logger.Error("could not record sync failure", "source", name, "error", saveErr)
		}
		return Result{}, fmt.Errorf("sync %s: %w", name, err)
	}

	imported, err := s.store.UpsertDocuments(name, documents, time.Now())
	if err != nil {
		return Result{}, fmt.Errorf("sync %s: %w", name, err)
	}

	// Only advance the watermark, never move it back. An empty batch leaves it
	// where it was, and a provider that reports a stale timestamp cannot cause
	// the next sync to re-download the whole library.
	if imported.Watermark.After(state.Watermark) {
		state.Watermark = imported.Watermark
	}
	state.LastRun = time.Now()
	state.LastError = ""
	if err := s.store.SaveSyncState(state); err != nil {
		return Result{}, err
	}

	result := Result{
		Source:  name,
		Fetched: len(documents),
		Created: imported.Created,
		Updated: imported.Updated,
	}
	s.logger.Info("sync finished",
		"source", name,
		"fetched", result.Fetched,
		"created", result.Created,
		"updated", result.Updated)
	return result, nil
}

// Run syncs on a fixed interval until the context is cancelled.
//
// It syncs once immediately: on a fresh start the queue would otherwise be
// empty until the first tick, which for a 30-minute interval is a long time to
// stare at nothing.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	if _, err := s.SyncAll(ctx); err != nil {
		s.logger.Error("initial sync failed", "error", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("sync loop stopping")
			return
		case <-ticker.C:
			if _, err := s.SyncAll(ctx); err != nil {
				s.logger.Error("scheduled sync failed", "error", err)
			}
		}
	}
}

func watermarkForLog(watermark time.Time) string {
	if watermark.IsZero() {
		return "beginning"
	}
	return watermark.Format(time.RFC3339)
}
