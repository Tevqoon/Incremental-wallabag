// Package scanworker is the background reader that turns uploaded page
// photos into passages: it claims pending rows from the store's page_scans
// table, sends each to a vision model, stores the answer, and re-assembles
// the owning book's passages from every scan finished so far.
//
// Shaped after internal/syncer — a Wake-able Run loop over a ticker and a
// context — but with one difference syncer never needed: several photos can
// be in flight to the model at once, since each ReadPage call is slow
// (several seconds) and mostly spent waiting on the network. That is what
// Options.Concurrency buys, and it is also what makes Reassemble's mutex
// necessary; see its own doc comment for why.
package scanworker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Tevqoon/increader/internal/pagescan"
	"github.com/Tevqoon/increader/internal/store"
)

// Reader is the one thing the worker needs from a vision model.
//
// Go note: declared here, at the consumer, rather than referenced as
// *pagescan.Client directly — the usual small-interface-at-the-call-site
// idiom. It lets a test hand the worker a fake that returns canned JSON
// instead of making network calls, without internal/pagescan needing to know
// this package exists. *pagescan.Client satisfies it as-is.
type Reader interface {
	ReadPage(ctx context.Context, image []byte, contentType string) (string, error)
}

// Default option values, applied by New wherever the caller leaves a field
// at its zero value.
const (
	DefaultConcurrency  = 3
	DefaultMaxAttempts  = 3
	DefaultRetryDelay   = 30 * time.Second
	DefaultPollInterval = time.Minute
	DefaultReadTimeout  = 3 * time.Minute

	// maxRetryDelay caps how long a goroutine backs off after a retryable
	// failure, regardless of how many attempts have piled up — see New's
	// doc comment on RetryDelay.
	maxRetryDelay = 5 * time.Minute
)

// Options configures a Worker. Every field defaults when left at its zero
// value; see the Default* constants above.
type Options struct {
	// Concurrency is how many photos are read by the model at once.
	Concurrency int

	// MaxAttempts is how many times a scan is tried (counted by
	// ClaimPageScan) before it is left failed for good rather than retried.
	MaxAttempts int

	// RetryDelay is the base backoff a goroutine waits after a retryable
	// failure before it claims again: min(RetryDelay*attempts, 5m). Growing
	// with the attempt count means a transient outage is retried quickly at
	// first and only backs off hard once it looks sustained.
	RetryDelay time.Duration

	// PollInterval is how often an idle goroutine re-checks for pending work
	// on its own, without being told to via Wake — a safety net against a
	// missed or dropped wake rather than the normal way work is picked up.
	PollInterval time.Duration

	// ReadTimeout bounds a single ReadPage call.
	ReadTimeout time.Duration

	// FloorDays and SpreadDays are passed through to ApplyScanPassages for
	// every newly inserted passage — the same annotation-delay settings the
	// file importer uses for a freshly imported book.
	FloorDays, SpreadDays int
}

// Worker is the background reader above store + pagescan.
type Worker struct {
	db      *store.Store
	reader  Reader
	logger  *slog.Logger
	options Options

	// wake carries requests to look for pending scans now. Buffered with
	// room for one, the same reasoning as syncer's own nudge channel: a
	// burst of uploads collapses into a single look-around rather than
	// queuing one per upload, and a non-blocking send means Wake never
	// blocks its caller (an upload handler, say).
	wake chan struct{}

	// assembleMu serializes Reassemble across every goroutine — see its own
	// doc comment for why a passage's correctness depends on this.
	assembleMu sync.Mutex
}

// New builds a Worker. Zero-valued Options fields take the Default* values
// above.
func New(db *store.Store, reader Reader, logger *slog.Logger, options Options) *Worker {
	if options.Concurrency <= 0 {
		options.Concurrency = DefaultConcurrency
	}
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = DefaultMaxAttempts
	}
	if options.RetryDelay <= 0 {
		options.RetryDelay = DefaultRetryDelay
	}
	if options.PollInterval <= 0 {
		options.PollInterval = DefaultPollInterval
	}
	if options.ReadTimeout <= 0 {
		options.ReadTimeout = DefaultReadTimeout
	}
	return &Worker{
		db:      db,
		reader:  reader,
		logger:  logger,
		options: options,
		wake:    make(chan struct{}, 1),
	}
}

// Wake asks the worker to look for pending scans now. Never blocks.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run processes scans until ctx is cancelled.
//
// On start it resets scans a previous process left "processing" — see
// ResetProcessingScans — since nothing marks a scan processing except a
// claim this run makes, and a fresh start has made none yet. It then runs
// Options.Concurrency goroutines; each drains pending scans until none are
// left (see Drain), then waits for Wake, the poll ticker, or ctx before
// draining again. Returns once every goroutine has stopped.
func (w *Worker) Run(ctx context.Context) {
	if n, err := w.db.ResetProcessingScans(time.Now()); err != nil {
		w.logger.Error("scanworker: could not reset in-flight scans at startup", "error", err)
	} else if n > 0 {
		w.logger.Info("scanworker: reset scans left processing by a previous run", "count", n)
	}
	w.Wake()

	var group sync.WaitGroup
	group.Add(w.options.Concurrency)
	for i := 0; i < w.options.Concurrency; i++ {
		go func() {
			defer group.Done()
			w.runOne(ctx)
		}()
	}
	group.Wait()
}

// runOne is the body of one of Run's goroutines.
func (w *Worker) runOne(ctx context.Context) {
	ticker := time.NewTicker(w.options.PollInterval)
	defer ticker.Stop()

	for {
		if err := w.Drain(ctx); err != nil {
			w.logger.Error("scanworker: drain failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

// Drain processes pending scans until none are left, returning the first
// error reading the queue itself produced — a claim failing is a store
// problem worth surfacing to the caller; a single scan going wrong along the
// way is not, and is logged rather than propagated, so one bad row cannot
// stop the rest of the batch.
//
// Used by each of Run's goroutines, and directly by tests that want one
// deterministic pass on a single goroutine (it honours RetryDelay, so a
// retryable failure still pauses that pass before trying the next scan).
func (w *Worker) Drain(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		scan, image, ok, err := w.db.ClaimPageScan(time.Now())
		if err != nil {
			return fmt.Errorf("scanworker: claim scan: %w", err)
		}
		if !ok {
			return nil
		}

		// Pass the wake on. An upload calls Wake once, and the one-slot
		// channel wakes exactly one idle goroutine — which would then read
		// the whole batch alone while the others slept until the next poll.
		// Each successful claim nudges the next idle goroutine, so a batch
		// fans out to Concurrency readers; a goroutine woken with nothing
		// left to claim simply goes back to waiting.
		w.Wake()

		if delay := w.processClaimedScan(ctx, scan, image); delay > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
		}
	}
}

// processClaimedScan runs one already-claimed scan through the model and
// records the outcome, returning how long the caller should back off before
// claiming again (zero unless this was a retryable failure).
func (w *Worker) processClaimedScan(ctx context.Context, scan store.PageScan, image []byte) time.Duration {
	now := time.Now()

	contentType, ok := pagescan.SniffImageType(image, scan.ContentType)
	if !ok {
		message := fmt.Sprintf("not an image the model can read (declared content type %q)", scan.ContentType)
		if err := w.db.FailPageScan(scan.ID, message, false, now); err != nil {
			w.logger.Error("scanworker: could not fail unreadable scan", "scan", scan.ID, "error", err)
		}
		return 0
	}

	readCtx, cancel := context.WithTimeout(ctx, w.options.ReadTimeout)
	raw, err := w.reader.ReadPage(readCtx, image, contentType)
	cancel()

	if err != nil {
		if ctx.Err() != nil {
			// The run itself is shutting down, not just this one read timing
			// out (readCtx would report the same error either way, which is
			// why this checks the run's own ctx rather than readCtx). Leave
			// the scan "processing" rather than recording a failed attempt:
			// ResetProcessingScans puts it back to pending on the next
			// start, so the read is simply tried again in full rather than
			// burning one of MaxAttempts on a shutdown that was never the
			// scan's fault.
			return 0
		}

		w.logger.Warn("scanworker: reading a page failed",
			"scan", scan.ID, "document", scan.DocumentID, "attempts", scan.Attempts, "error", err)

		retry := scan.Attempts < w.options.MaxAttempts
		if failErr := w.db.FailPageScan(scan.ID, err.Error(), retry, now); failErr != nil {
			w.logger.Error("scanworker: could not record scan failure", "scan", scan.ID, "error", failErr)
		}
		if !retry {
			return 0
		}

		delay := w.options.RetryDelay * time.Duration(scan.Attempts)
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
		return delay
	}

	if err := w.db.CompletePageScan(scan.ID, raw, now); err != nil {
		w.logger.Error("scanworker: could not complete scan", "scan", scan.ID, "error", err)
		return 0
	}

	result, err := w.Reassemble(scan.DocumentID, scan.ID)
	if err != nil {
		w.logger.Error("scanworker: could not reassemble after a completed scan",
			"scan", scan.ID, "document", scan.DocumentID, "error", err)
		return 0
	}
	w.logger.Info("scanworker: assembled passages",
		"document", scan.DocumentID, "scan", scan.ID,
		"inserted", result.Inserted, "updated", result.Updated, "removed", result.Removed)
	return 0
}

// Reassemble rebuilds one book's passages from all of its finished scans and
// writes them, with fromScan as the scan that "just changed" — see
// ApplyScanPassages for what that means. Used after every scan this worker
// completes, and directly by the web layer after a reader hand-corrects a
// page label the model could not read.
//
// Go note: holds a worker-wide mutex for the whole read-assemble-write. A
// passage can be split across two photos — one bracketed across a page
// turn — that finish at nearly the same instant on two different
// goroutines. Each goroutine calls CompletePageScan for its own scan
// *before* calling in here, so whichever of the two takes this lock second
// is guaranteed to see both scans as done in PageScans and joins the two
// halves into one passage. Without the lock, both goroutines could load the
// book's scans before the other had written its result, and each would
// insert its own half as a lonely, sentence-trimmed passage that the other
// never gets a chance to join.
func (w *Worker) Reassemble(documentID, fromScan int64) (store.ScanApplyResult, error) {
	w.assembleMu.Lock()
	defer w.assembleMu.Unlock()

	scans, err := w.db.PageScans(documentID)
	if err != nil {
		return store.ScanApplyResult{}, fmt.Errorf("scanworker: list scans of document %d: %w", documentID, err)
	}

	var parsed []pagescan.Scan
	for _, scan := range scans {
		if scan.Status != store.ScanDone {
			continue
		}
		result, err := pagescan.ParseResult(scan.Result)
		if err != nil {
			// Never fail the whole book over one unparseable stored result —
			// log it and assemble from every scan that does parse.
			w.logger.Warn("scanworker: stored scan result would not parse, skipping it",
				"scan", scan.ID, "document", documentID, "error", err)
			continue
		}
		parsed = append(parsed, pagescan.Scan{ID: scan.ID, Result: result})
	}

	existing, err := w.db.ScanElementChapters(documentID)
	if err != nil {
		return store.ScanApplyResult{}, fmt.Errorf("scanworker: read chapters of document %d: %w", documentID, err)
	}

	passages := pagescan.Assemble(parsed, existing)
	converted := make([]store.ScanPassage, len(passages))
	for i, p := range passages {
		converted[i] = store.ScanPassage{
			Ref:          p.Ref,
			AbsorbedRefs: p.AbsorbedRefs,
			Page:         p.Page,
			Ordinal:      p.Ordinal,
			Text:         p.Text,
			NotePending:  p.NotePending,
			Chapter:      p.Chapter,
			ScanIDs:      p.ScanIDs,
		}
	}

	options := store.ScanApplyOptions{FloorDays: w.options.FloorDays, SpreadDays: w.options.SpreadDays}
	result, err := w.db.ApplyScanPassages(documentID, fromScan, converted, options, time.Now())
	if err != nil {
		return store.ScanApplyResult{}, fmt.Errorf("scanworker: apply passages of document %d: %w", documentID, err)
	}
	return result, nil
}
