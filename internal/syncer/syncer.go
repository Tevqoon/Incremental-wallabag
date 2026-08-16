// Package syncer pulls documents from sources into the store.
//
// It is the only place that knows both halves: it depends on the source
// interface and on storage, but on no specific provider. Adding wallabag,
// KOReader or anything else changes main's wiring, not this package.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/store"
)

// writeBatch caps how many queued writes one sync publishes, so a large backlog
// is worked through over several passes instead of stalling a single one.
const writeBatch = 200

// reconcileInterval is how often the scheduled loop pays for a full listing
// to check for documents deleted upstream, rather than every tick. A
// deletion is rare enough, and a full listing costly enough against a
// library of any size, that checking on every 30-minute sync would trade
// real cost for no real benefit over checking once a day.
const reconcileInterval = 24 * time.Hour

// Syncer imports from a fixed set of sources.
type Syncer struct {
	store   *store.Store
	sources []source.Source
	logger  *slog.Logger

	// annotationFloorDays and annotationSpreadDays are how far ahead imported
	// highlights are scheduled — see store.UpsertDocuments.
	annotationFloorDays  int
	annotationSpreadDays int

	// nudge carries requests to publish the outbox early. Buffered with room
	// for one so a burst of edits collapses into a single drain rather than
	// queueing a drain per keystroke, and non-blocking sends mean a handler
	// never waits on it.
	nudge chan struct{}

	// lastReconcile is when Reconcile last ran, read and written only from
	// within Run's own goroutine — nothing else touches it, so it needs no
	// lock. It gates how often the scheduled loop pays for a full listing;
	// the manual "sync now" path reconciles unconditionally instead.
	lastReconcile time.Time

	// drainMu serializes drainWrites across providers and callers. Run's own
	// loop only ever drains from one goroutine at a time, but a request
	// handler's "sync now" runs on its own goroutine and can overlap a
	// scheduled tick or nudge. Without this, two overlapping drains can both
	// read the same pending row before either completes it, publishing the
	// same change upstream twice.
	drainMu sync.Mutex
}

// New builds a syncer over the given sources.
func New(db *store.Store, logger *slog.Logger, sources ...source.Source) *Syncer {
	return &Syncer{
		store:   db,
		sources: sources,
		logger:  logger,
		nudge:   make(chan struct{}, 1),
	}
}

// WithAnnotationDelay sets how far ahead imported highlights become due:
// floorDays at the soonest, spread up to spreadDays further out from there —
// see store.UpsertDocuments.
//
// Go note: a small option method rather than another positional argument to
// New. Callers that do not care are unaffected, and the one that does reads as
// a sentence at the call site.
func (s *Syncer) WithAnnotationDelay(floorDays, spreadDays int) *Syncer {
	s.annotationFloorDays = floorDays
	s.annotationSpreadDays = spreadDays
	return s
}

// Publish asks the running sync loop to drain queued writes now.
//
// Safe to call from a request handler: it never blocks, and dropping the signal
// when one is already pending is correct — the pending drain will pick up
// whatever was just queued.
func (s *Syncer) Publish() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Result summarises one source's sync.
type Result struct {
	Source     string
	Fetched    int
	Created    int
	Updated    int
	Suspended  int
	Highlights int
	Published  int
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

	published := s.drainWrites(ctx, provider)

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

	imported, err := s.store.UpsertDocuments(name, documents, s.annotationFloorDays, s.annotationSpreadDays, time.Now())
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
		Source:     name,
		Fetched:    len(documents),
		Created:    imported.Created,
		Updated:    imported.Updated,
		Suspended:  imported.Suspended,
		Highlights: imported.Highlights,
		Published:  published,
	}
	s.logger.Info("sync finished",
		"source", name,
		"fetched", result.Fetched,
		"created", result.Created,
		"updated", result.Updated,
		"archived", result.Suspended,
		"highlights", result.Highlights,
		"published", result.Published)
	return result, nil
}

// Reconcile checks each source's full current listing against what is stored
// locally, flagging any document no longer found upstream.
//
// This is deliberately separate from the ordinary incremental Sync. Sync asks
// a provider for what changed since a watermark, which by construction can
// never report a deletion: nothing "changed" about a document that stopped
// existing, it simply stopped appearing. Only a full listing — since the
// zero time, meaning everything — can reveal an absence, which is also why
// this is not folded into every scheduled sync; see reconcileInterval.
//
// One failing source does not abort the others, matching SyncAll.
func (s *Syncer) Reconcile(ctx context.Context) error {
	var firstErr error

	for _, provider := range s.sources {
		documents, err := provider.Fetch(ctx, time.Time{})
		if err != nil {
			s.logger.Error("reconcile: fetch failed", "source", provider.Name(), "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		if _, err := s.store.UpsertDocuments(provider.Name(), documents, s.annotationFloorDays, s.annotationSpreadDays, time.Now()); err != nil {
			s.logger.Error("reconcile: upsert failed", "source", provider.Name(), "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		present := make([]string, len(documents))
		var presentHighlights, locationlessHighlights []string
		for i, document := range documents {
			present[i] = document.ExternalID
			for _, highlight := range document.Highlights {
				presentHighlights = append(presentHighlights, highlight.ExternalID)
				if !highlight.HasLocation {
					locationlessHighlights = append(locationlessHighlights, highlight.ExternalID)
				}
			}
		}

		marked, cleared, err := s.reconcileMissingDocuments(ctx, provider, present)
		if err != nil {
			s.logger.Error("reconcile: marking missing documents failed", "source", provider.Name(), "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if marked > 0 || cleared > 0 {
			s.logger.Info("reconciled library against upstream",
				"source", provider.Name(), "newly_missing", marked, "restored", cleared)
		}

		highlightsMarked, highlightsCleared, err := s.reconcileMissingHighlights(ctx, provider, presentHighlights)
		if err != nil {
			s.logger.Error("reconcile: marking missing highlights failed", "source", provider.Name(), "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if highlightsMarked > 0 || highlightsCleared > 0 {
			s.logger.Info("reconciled highlights against upstream",
				"source", provider.Name(), "newly_missing", highlightsMarked, "restored", highlightsCleared)
		}

		// Only a Writer can ever drain what this queues; a read-only source
		// would just accumulate writes nothing ever sends.
		if _, ok := provider.(source.Writer); ok {
			backfilled, err := s.store.BackfillHighlightPushes(provider.Name())
			if err != nil {
				s.logger.Error("reconcile: backfilling highlight pushes failed", "source", provider.Name(), "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if backfilled > 0 {
				s.logger.Info("queued highlight pushes for extracts that had missed theirs",
					"source", provider.Name(), "count", backfilled)
				// Drained here rather than left for the next nudge or tick:
				// Reconcile already runs rarely enough (once a day, or on a
				// manual sync) that whoever triggered it should see the
				// backfilled extracts actually reach wallabag, not just get
				// queued.
				s.drainWrites(ctx, provider)
			}

			relocated, err := s.store.QueueLocationUpdates(provider.Name(), locationlessHighlights)
			if err != nil {
				s.logger.Error("reconcile: queuing highlight location updates failed", "source", provider.Name(), "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if relocated > 0 {
				s.logger.Info("queued location updates for highlights the provider could not draw in place",
					"source", provider.Name(), "count", relocated)
				s.drainWrites(ctx, provider)
			}
		}
	}
	return firstErr
}

// reconcileMissingDocuments flags documents absent from present as missing
// upstream, but only after a second, independent listing agrees they are
// gone — see the comment on Reconcile for why present alone is not proof of
// a deletion.
//
// MissingCandidates asks "would anything be flagged missing right now?"
// against present alone. When the answer is no — the overwhelmingly common
// case, since actual deletions are rare — this returns without provider.Fetch
// ever being called a second time, which is what keeps the fix free for every
// ordinary run. Only when there is at least one candidate does the cost of a
// second full listing get paid, and only that once.
//
// Why a second listing is trustworthy where the first alone is not: present
// alone cannot distinguish a genuine deletion from a document that was merely
// shifted past mid-walk by AllEntries' own ascending pagination (see
// entries.go) — ascending order protects a moved record's own slot from being
// skipped, but not its neighbours', so when an update reappends the moved
// record at the end, everything that was going to occupy its old slot shifts
// down by one and is silently skipped for that walk. A burst of writes (a
// backfill importer, or the outbox draining annotation location updates) is
// enough to shift several records past the page boundary in a single sync.
//
// The second Fetch is an independent sample of the same upstream state
// seconds later, not a retry of the first: it walks the same pages again from
// page 1, in the same ascending order, and whatever slid past the boundary
// during the first walk has by then either settled into a slot this walk
// will visit, or — far less likely — is being shifted again by a second
// burst of writes landing in the same few seconds. A document absent from two
// independent listings taken seconds apart is genuinely gone; one absent from
// a single listing is far more likely to have merely been shifted past.
//
// This is worth the extra request specifically because the cost of getting
// it wrong is a *destructive* suggestion in the library UI: a delete button
// sits right next to "missing upstream", and flapping that flag risks a
// reader deleting a document that still exists at the provider.
func (s *Syncer) reconcileMissingDocuments(ctx context.Context, provider source.Source, present []string) (marked, cleared int, err error) {
	name := provider.Name()

	candidates, err := s.store.MissingCandidates(name, present)
	if err != nil {
		return 0, 0, fmt.Errorf("missing candidates: %w", err)
	}

	// No candidates means nothing can newly go missing, so the second listing
	// is skipped and this stays a single-request operation in the common case.
	// ReconcileMissing is still called, because it does two jobs: it flags what
	// has gone, and it clears the flag from anything that has come back. Only
	// the first depends on a candidate existing. Returning early here instead
	// would leave a document that was wrongly flagged once — which is exactly
	// what the race below produces — carrying that flag until some unrelated
	// candidate happened to trigger this path again.
	if len(candidates) == 0 {
		return s.store.ReconcileMissing(name, present)
	}

	second, err := provider.Fetch(ctx, time.Time{})
	if err != nil {
		return 0, 0, fmt.Errorf("second listing: %w", err)
	}

	candidateSet := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		candidateSet[id] = true
	}
	rescued := map[string]bool{}
	union := append([]string{}, present...)
	for _, document := range second {
		union = append(union, document.ExternalID)
		if candidateSet[document.ExternalID] {
			rescued[document.ExternalID] = true
		}
	}
	if len(rescued) > 0 {
		s.logger.Info("second listing rescued documents from a false missing flag",
			"source", name, "candidates", len(candidates), "rescued", len(rescued))
	}

	return s.store.ReconcileMissing(name, union)
}

// reconcileMissingHighlights is reconcileMissingDocuments one level down: the
// same false-positive risk applies to an individual highlight's external_ref
// exactly as it does to its parent document's external_id, since both are
// shifted past a page boundary by the very same mid-walk update. See that
// function's comment for the full argument; nothing here differs beyond
// operating on highlight refs gathered from a document listing rather than
// document ids directly.
func (s *Syncer) reconcileMissingHighlights(ctx context.Context, provider source.Source, present []string) (marked, cleared int, err error) {
	name := provider.Name()

	candidates, err := s.store.MissingHighlightCandidates(name, present)
	if err != nil {
		return 0, 0, fmt.Errorf("missing highlight candidates: %w", err)
	}
	// Still called with no candidates, for the same reason as its document
	// counterpart above: this clears a stale flag as well as setting a new one,
	// and only the setting half needs a candidate.
	if len(candidates) == 0 {
		return s.store.ReconcileMissingHighlights(name, present)
	}

	second, err := provider.Fetch(ctx, time.Time{})
	if err != nil {
		return 0, 0, fmt.Errorf("second listing: %w", err)
	}

	candidateSet := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		candidateSet[id] = true
	}
	rescued := map[string]bool{}
	union := append([]string{}, present...)
	for _, document := range second {
		for _, highlight := range document.Highlights {
			union = append(union, highlight.ExternalID)
			if candidateSet[highlight.ExternalID] {
				rescued[highlight.ExternalID] = true
			}
		}
	}
	if len(rescued) > 0 {
		s.logger.Info("second listing rescued highlights from a false missing flag",
			"source", name, "candidates", len(candidates), "rescued", len(rescued))
	}

	return s.store.ReconcileMissingHighlights(name, union)
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
	// Reconciling right after the initial sync, rather than waiting a full
	// reconcileInterval, means a fresh start also gets a deletion check —
	// there is no earlier state for it to have missed anyway.
	if err := s.Reconcile(ctx); err != nil {
		s.logger.Error("initial reconcile failed", "error", err)
	}
	s.lastReconcile = time.Now()

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
			if time.Since(s.lastReconcile) > reconcileInterval {
				if err := s.Reconcile(ctx); err != nil {
					s.logger.Error("scheduled reconcile failed", "error", err)
				}
				s.lastReconcile = time.Now()
			}

		case <-s.nudge:
			// A change was made that needs sending upstream. Publish only —
			// a full fetch here would make every star and tag edit pull the
			// whole library.
			for _, provider := range s.sources {
				if published := s.drainWrites(ctx, provider); published > 0 {
					s.logger.Info("published queued changes",
						"source", provider.Name(), "count", published)
				}
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

// drainWrites publishes queued changes to a provider before fetching from it.
//
// Before, rather than after, so that a state increader has already decided —
// an article marked Done — is upstream by the time the listing comes back. The
// other order would fetch the stale value and then immediately overwrite it
// locally, undoing the reader's action until the following sync.
//
// A provider that cannot write has nothing queued and nothing to do.
//
// The read-act-complete sequence below (PendingWrites, then a provider call
// per row, then CompleteWrite) is not itself atomic, so two overlapping
// calls — the scheduled loop and a manual "sync now" request, say — could
// otherwise both claim the same pending row and publish it upstream twice.
// drainMu makes the whole sequence run one call at a time.
func (s *Syncer) drainWrites(ctx context.Context, provider source.Source) int {
	writer, ok := provider.(source.Writer)
	if !ok {
		return 0
	}

	s.drainMu.Lock()
	defer s.drainMu.Unlock()

	writes, err := s.store.PendingWrites(provider.Name(), writeBatch)
	if err != nil {
		s.logger.Error("could not read pending writes", "source", provider.Name(), "error", err)
		return 0
	}

	published := 0
	for _, write := range writes {
		ref, err := s.applyWrite(ctx, writer, write)

		switch {
		case err == nil:
			// OpHighlightCreate and OpHighlightUpdateLocation are the two
			// operations that return a ref: the provider's id for something
			// that did not exist (or exists anew, replacing what did) until
			// this write. Recording it is
			// what lets a later delete of the same element find it upstream.
			if ref != "" && write.ElementID.Valid {
				if err := s.store.SetExternalRef(write.ElementID.Int64, ref); err != nil {
					s.logger.Error("could not record new highlight's id",
						"element", write.ElementID.Int64, "error", err)
				}
			}
			if err := s.store.CompleteWrite(write.ID); err != nil {
				s.logger.Error("could not clear a published write", "id", write.ID, "error", err)
			}
			published++

		case errors.Is(err, source.ErrGone):
			// The entry is gone upstream, so this can never succeed. Dropping
			// it beats retrying until the attempt cap and leaving a permanent
			// error sitting in the queue.
			s.logger.Warn("dropping a write for an entry that no longer exists",
				"source", write.Source, "entry", write.ExternalID, "operation", write.Operation)
			if err := s.store.CompleteWrite(write.ID); err != nil {
				s.logger.Error("could not drop a write", "id", write.ID, "error", err)
			}

		default:
			s.logger.Warn("write failed, will retry",
				"source", write.Source, "entry", write.ExternalID,
				"operation", write.Operation, "attempts", write.Attempts+1, "error", err)
			if err := s.store.FailWrite(write.ID, err); err != nil {
				s.logger.Error("could not record a write failure", "id", write.ID, "error", err)
			}
			// Keep going: one unreachable entry should not block the rest.
		}
	}

	return published
}

// applyWrite dispatches one queued change to the provider. The returned
// string is only ever non-empty for OpHighlightCreate, which is the one
// operation whose result the caller needs back.
func (s *Syncer) applyWrite(ctx context.Context, writer source.Writer, write store.PendingWrite) (string, error) {
	switch write.Operation {
	case store.OpArchive:
		return "", writer.SetArchived(ctx, write.ExternalID, store.PayloadBool(write.Payload))
	case store.OpStar:
		return "", writer.SetStarred(ctx, write.ExternalID, store.PayloadBool(write.Payload))
	case store.OpTagAdd:
		return "", writer.AddTags(ctx, write.ExternalID, []string{write.Payload})
	case store.OpTagRemove:
		return "", writer.RemoveTag(ctx, write.ExternalID, write.Payload)
	case store.OpHighlightDelete:
		return "", writer.DeleteHighlight(ctx, write.ExternalID)
	case store.OpEntryDelete:
		return "", writer.DeleteEntry(ctx, write.ExternalID)
	case store.OpHighlightCreate:
		return writer.CreateHighlight(ctx, write.ExternalID, write.Payload)
	case store.OpHighlightUpdateLocation:
		if !write.ElementID.Valid {
			// Nothing to resolve the document from; drop it rather than
			// fail forever on a row that can never be completed.
			return "", nil
		}
		documentExternalID, err := s.store.DocumentExternalID(write.ElementID.Int64)
		if err != nil {
			return "", err
		}
		return writer.UpdateHighlightLocation(ctx, write.ExternalID, documentExternalID, write.Payload)
	default:
		// Unknown operations are dropped rather than retried: the row was
		// written by a version of increader that understood it, and no future
		// attempt by this one will do better.
		return "", nil
	}
}
