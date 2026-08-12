package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/Tevqoon/increader/internal/store"
)

// RepairResult tallies what Repair actually did.
type RepairResult struct {
	// Repaired counts entries whose local document row was found and
	// brought up to date (refs remapped, content cleared, anchors cleared).
	Repaired int

	// Skipped counts entries with no local row to repair — the ordinary
	// case for a document ActionCreate just made: it will not exist locally
	// until the next sync's UpsertDocuments creates it, which this run has
	// no reason to run itself.
	Skipped int

	Errors []error
}

// Repair folds a completed Apply back into the local store, so that a
// wallabag highlight whose id just changed (see Remap) keeps the reading
// schedule already built up on its local row, and so that a document whose
// content was just replaced re-fetches the real thing instead of continuing
// to serve whatever body it cached before this run.
//
// Only ever touches entries Applied.Remaps says had a successful content
// write — see that field's own comment for how its keys carry that fact.
// An entry whose content write failed was never given a slot there at all,
// which is what keeps a failed write from ever reaching this function; Apply
// itself already refused to touch that entry's annotations for the same
// reason.
//
// store.ErrNotFound from DocumentByExternalID is the expected, ordinary
// outcome for a freshly created entry — its local row is only made by the
// next sync, not by this run — and is counted as Skipped, not an error.
//
// Idempotent by construction, which is what makes it safe to re-run after a
// partial failure: RemapExternalRef tolerates both a zero-match retry and a
// unique-index collision as already-done (see its own comment), and
// ClearDocumentContent / ClearExtractAnchors are themselves idempotent
// updates with nothing left to do on a second pass. A failure partway
// through one entry's repair — a remap that collides for a reason worth
// logging, say — does not stop the remaining remaps for that same entry, nor
// does it stop Repair moving on to the next entry: every local write here is
// independent of every other, and there is no partial local state a
// half-finished repair could leave that this same call, run again, would not
// finish cleanly.
func Repair(ctx context.Context, db *store.Store, applied Applied, logger *slog.Logger) (RepairResult, error) {
	var result RepairResult

	for entryID, remaps := range applied.Remaps {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("ingest: repair cancelled: %w", err)
		}

		externalID := strconv.Itoa(entryID)
		document, err := db.DocumentByExternalID("wallabag", externalID)
		if errors.Is(err, store.ErrNotFound) {
			result.Skipped++
			logger.Debug("ingest: no local document yet for entry, skipping repair", "entry_id", entryID)
			continue
		}
		if err != nil {
			result.Errors = append(result.Errors,
				fmt.Errorf("ingest: repair entry %d: look up local document: %w", entryID, err))
			continue
		}

		for _, remap := range remaps {
			if err := db.RemapExternalRef(document.ID, remap.Old, remap.New); err != nil {
				// Logged and kept: one bad remap must not stop the other
				// remaps on the same entry, nor the content/anchor cleanup
				// that follows — the local row for an un-remapped highlight
				// simply keeps its old ref, which is a smaller problem than
				// leaving the rest of the entry unrepaired over it.
				result.Errors = append(result.Errors,
					fmt.Errorf("ingest: repair entry %d: remap %s -> %s: %w",
						entryID, remap.Old, remap.New, err))
			}
		}

		if err := db.ClearDocumentContent(document.ID); err != nil {
			result.Errors = append(result.Errors,
				fmt.Errorf("ingest: repair entry %d: clear content: %w", entryID, err))
			continue
		}
		if _, err := db.ClearExtractAnchors(document.ID); err != nil {
			result.Errors = append(result.Errors,
				fmt.Errorf("ingest: repair entry %d: clear extract anchors: %w", entryID, err))
			continue
		}

		result.Repaired++
	}

	return result, nil
}
