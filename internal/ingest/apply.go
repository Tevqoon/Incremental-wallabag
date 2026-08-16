package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/Tevqoon/increader/internal/wallabag"
)

// Remap is one annotation's old wallabag id and the new one that replaced
// it — UpdateHighlightLocation's create-then-delete result, carried forward
// so Repair can apply the same change to the matching local elements row.
type Remap struct{ Old, New string }

// Applied is what actually happened when a Plan was sent to wallabag.
type Applied struct {
	Created, Updated, Reanchored, AnnotationFailures int

	// Skipped counts annotations Apply deliberately left untouched because
	// their Verdict was VerdictMissing — see applyExisting's own comment on
	// why leaving them alone, ranges and all, is correct rather than merely
	// convenient. Surfaced here, next to Reanchored, so the two numbers are
	// visible side by side in the same summary: this field is what caught
	// the 2026-08-12 production bug in the first place, where a run
	// re-anchored 34 annotations against a plan that had classified only 32
	// as VerdictUnique. A report that prints Reanchored without also
	// printing how many were intentionally skipped gives the operator no
	// way to notice that kind of mismatch for free; this field is that
	// check, paid for once here rather than reconstructed by hand every time
	// someone suspects Apply of doing more than the plan said.
	Skipped int

	// Remaps records, per wallabag entry id, every annotation re-anchor that
	// succeeded on it. A map entry's mere presence — even with a nil or
	// empty slice — is itself significant: it is Apply's record that this
	// entry's *content* write (create or PATCH) succeeded, since an entry is
	// only ever given a slot in this map from the point its content write is
	// known good. That is what lets Repair, which only receives this struct
	// and not the Plan that produced it, tell "this entry is safe to run
	// local cleanup on" apart from "this entry's write failed, do not touch
	// its local row" using nothing but the struct the brief specifies —
	// without it, Repair would need either the Plan itself or a second field
	// duplicating what content-success already implies.
	Remaps map[int][]Remap

	// Grew records, per wallabag entry id whose content write succeeded,
	// whether BuildPlan flagged that entry's content as having grown
	// materially (Item.ContentGrew) — the signal Repair needs to decide
	// whether to call RequeueDocumentRoot. Carried here rather than having
	// Repair re-derive it from the Plan for the same reason Remaps'
	// presence stands in for content-success: Repair receives only this
	// struct, not the Plan that produced it. An entry id absent from this
	// map reads as false via Go's own zero value for a missing key, which
	// is exactly right for an entry Apply never touched (create; a failed
	// content write) or one BuildPlan never flagged as grown.
	Grew map[int]bool

	// Errors accumulates every per-item and per-annotation failure that did
	// not abort the run — Apply itself only returns a non-nil error for
	// something that stops the whole batch (a cancelled context), never for
	// one bad entry among many.
	Errors []error
}

// Apply sends a Plan to wallabag and returns what happened.
//
// There is no cross-system transaction covering any of this — wallabag and
// increader's local store are two different systems with no shared commit
// point — so the ordering within each item is what makes a partial failure
// safe rather than merely likely to be safe:
//
//  1. Content: PATCH an existing entry, or create a new one. On failure this
//     item is abandoned entirely — its annotations are never touched. A
//     content write that failed has left the entry exactly as it was before
//     this run; touching annotations on top of that would be building on
//     content this run does not actually control.
//
//  2. Each annotation whose Verdict is VerdictUnique or VerdictAmbiguous,
//     re-anchored via UpdateHighlightLocation. VerdictAnchored is skipped as
//     pointless churn (see its own doc comment). VerdictMissing is skipped
//     too, deliberately, and for a different reason:
//
//     A stale range after a content swap points into markup that no longer
//     exists, so it is worthless either way — that is the argument for
//     replacing it with an honestly empty one by re-anchoring anyway. But
//     the ranges are the only durable record of a highlight's full text.
//     wallabag truncates a stored quote past ~900 bytes (see truncateQuote
//     in internal/wallabag/write.go), and recoverQuote / ResolveRange exist
//     precisely to rebuild the full passage from the ranges when the quote
//     alone is not enough. Re-anchoring a VerdictMissing annotation calls
//     CreateHighlight with a quote that, by definition of this verdict,
//     cannot be found in the new content — it comes back with ranges empty,
//     silently wiping the one remaining copy of the highlight's full text
//     for any annotation whose quote was also truncated. Confirmed live on
//     2026-08-12: a backfill run classified 32 annotations VerdictUnique but
//     re-anchored 34 — the extra two were VerdictMissing, and both came back
//     with empty ranges (ids 92934 -> 99936 and 87497 -> 99959). Their quotes
//     happened to be under the truncation limit, so nothing was actually
//     lost that run, but the same bug against a missing-and-truncated
//     annotation would be unrecoverable. report.go already tells the
//     operator these must be "fixed by hand" — Apply re-anchoring them
//     anyway would make that claim false. A single annotation failing to
//     re-anchor logs and moves on to the next one — it never rolls the
//     content write back, and it never aborts the rest of this entry's
//     annotations either. The alternative, making one annotation's failure
//     block the others, would turn "8 of 9 annotations re-anchored, 1
//     network blip" into "0 of 9", for no benefit to anyone.
//
//  3. Tags — only for an existing entry (ActionUpdate / ActionAnnotationsOnly).
//     A brand-new entry's tags already went out with its NewEntry.Tags on
//     create (see entryForm), but EntryUpdate carries no Tags field at all
//     (see EntryUpdate.form's own comment on why full-omission-on-PATCH is
//     the safe default), so an existing entry's tags need this separate call
//     regardless of whether its content or its annotations changed.
//
// Items with ActionSkip or ActionConflict are not sent anywhere: skip
// because there is nothing to do, conflict because BuildPlan already refused
// to decide which entry this post belongs to.
//
// Accepted rather than "fixed": CreateHighlight (internal/wallabag/write.go)
// re-fetches the entry's whole body once per annotation it creates, so an
// entry with eight stale annotations does eight full-body fetches over the
// course of step 2. Wasteful in the abstract, and entirely fine for a
// command an operator runs about once a month — forking a second,
// range-aware annotation-create path to avoid it would be exactly the
// "computeRangesAt" duplication UpdateHighlightLocation's own comment warns
// against, for a cost that does not matter at this scale.
func Apply(ctx context.Context, client *wallabag.Client, src *wallabag.Source, plan Plan, logger *slog.Logger) (Applied, error) {
	applied := Applied{Remaps: make(map[int][]Remap), Grew: make(map[int]bool)}

	for _, item := range plan.Items {
		if err := ctx.Err(); err != nil {
			return applied, fmt.Errorf("ingest: apply cancelled: %w", err)
		}

		switch item.Action {
		case ActionSkip, ActionConflict:
			continue
		case ActionCreate:
			applyCreate(ctx, client, item, &applied, logger)
		case ActionUpdate, ActionAnnotationsOnly:
			applyExisting(ctx, client, src, item, &applied, logger)
		}
	}

	return applied, nil
}

// applyCreate makes a brand new wallabag entry for a post with no existing
// match. There are no annotations to re-anchor — nothing existed upstream
// for this post before this call — so this is the one branch of Apply that
// only ever does step 1.
func applyCreate(ctx context.Context, client *wallabag.Client, item Item, applied *Applied, logger *slog.Logger) {
	entry, err := client.CreateEntry(ctx, wallabag.NewEntry{
		URL:         item.Post.URL,
		Title:       item.Post.Title,
		Content:     forWallabag(item.Post.ContentHTML),
		Language:    item.Post.Language,
		Authors:     item.Post.Author,
		PublishedAt: item.Post.PublishedAt,
		Tags:        item.Post.Tags,
	})
	if err != nil {
		logger.Error("ingest: create entry failed", "url", item.Post.URL, "error", err)
		applied.Errors = append(applied.Errors, fmt.Errorf("ingest: create entry for %s: %w", item.Post.URL, err))
		return
	}

	applied.Created++
	// See Applied.Remaps' own comment: a nil slice under this key still
	// marks the content write as having succeeded, which is what tells
	// Repair this entry is safe to clean up locally even though there is
	// nothing to remap on a post that never had annotations to begin with.
	applied.Remaps[entry.ID] = nil
	logger.Info("ingest: created entry", "url", item.Post.URL, "entry_id", entry.ID)
}

// applyExisting handles ActionUpdate and ActionAnnotationsOnly, which share
// everything past the content step: an ActionAnnotationsOnly item simply has
// no content PATCH to make, its content having already been confirmed full
// by BuildPlan.
func applyExisting(ctx context.Context, client *wallabag.Client, src *wallabag.Source, item Item, applied *Applied, logger *slog.Logger) {
	if item.Action == ActionUpdate {
		_, err := client.UpdateEntry(ctx, item.EntryID, wallabag.EntryUpdate{
			Title:   item.Post.Title,
			Content: forWallabag(item.Post.ContentHTML),
			// Authors is re-sent on every write that touches content,
			// deliberately: a content-only PATCH against the live API
			// preserved the title but blanked published_by (see
			// EntryUpdate.form's own comment on this finding). Omitting it
			// here on the theory that "only content changed" would
			// reproduce exactly that loss.
			Authors:     item.Post.Author,
			Language:    item.Post.Language,
			PublishedAt: item.Post.PublishedAt,
		})
		if err != nil {
			logger.Error("ingest: content update failed; leaving this entry's annotations untouched",
				"entry_id", item.EntryID, "error", err)
			applied.Errors = append(applied.Errors,
				fmt.Errorf("ingest: update entry %d: %w", item.EntryID, err))
			return // Step 1 failed: never touch this entry's annotations.
		}
		applied.Updated++
	}

	// From here on, this entry's content is known good — either this call
	// just PATCHed it, or BuildPlan already found it full. Reserve its slot
	// in Remaps now, even before any annotation is actually re-anchored, so
	// Repair still runs local cleanup on an entry whose content changed but
	// which happens to have zero stale annotations.
	if _, exists := applied.Remaps[item.EntryID]; !exists {
		applied.Remaps[item.EntryID] = nil
	}
	// Recorded regardless of whether it is true: for ActionAnnotationsOnly,
	// content was already found full by BuildPlan, and content that has not
	// changed cannot itself have grown (see planOne's ContentGrew
	// computation), so this is always false on that path. Set unconditionally
	// rather than only when true so the map's own presence for this entry
	// stays consistent with Remaps' — both exist here the moment content is
	// known good, never before.
	applied.Grew[item.EntryID] = item.ContentGrew

	entryID := strconv.Itoa(item.EntryID)
	for _, ann := range item.Annotations {
		if ann.Verdict == VerdictAnchored {
			continue
		}
		if ann.Verdict == VerdictMissing {
			// See Apply's own doc comment for why: re-anchoring a quote that
			// is not in the new content at all can only come back with empty
			// ranges, destroying the one durable copy of a highlight whose
			// quote is also truncated. report.go promises these are left
			// alone for the operator to fix by hand — this is what keeps
			// that promise true.
			applied.Skipped++
			continue
		}

		oldID := strconv.Itoa(ann.AnnotationID)
		// wallabag.TrimTruncationMarker(ann.Quote), not ann.Quote itself: the
		// stored quote may still carry truncateQuote's own trailing "…" (or,
		// on data written before that function's UTF-8 fix, dangling
		// debris), and sending that upstream verbatim would hand wallabag a
		// literal ellipsis character that is not actually part of the
		// article — CreateHighlight's own quote-location lookup (see
		// computeRanges in internal/wallabag/ranges.go) would then fail to
		// find it in the entry's content for exactly the reason this whole
		// change exists to fix. The trimmed quote is also comfortably under
		// maxHighlightQuoteLength (900 bytes) even when the original,
		// untrimmed local extract was not — it was already truncated to
		// roughly that size once, upstream, before increader ever saw it
		// stored — so this re-anchor will not trigger truncateQuote a
		// second time.
		//
		// That does mean the quote wallabag ends up storing after this call
		// differs from increader's own local, untruncated copy of the same
		// highlight — which is exactly why RemapExternalRef (see repair.go)
		// is load-bearing here and not merely tidy: insertHighlights'
		// adopt-by-exact-quote path (elements.go) matches local rows against
		// wallabag's stored text byte-for-byte, and a re-anchored annotation
		// re-anchored through this path will never satisfy that match on its
		// own.
		newID, err := src.UpdateHighlightLocation(ctx, oldID, entryID, wallabag.TrimTruncationMarker(ann.Quote))
		if err != nil {
			logger.Error("ingest: re-anchor annotation failed",
				"entry_id", item.EntryID, "annotation_id", ann.AnnotationID, "error", err)
			applied.AnnotationFailures++
			applied.Errors = append(applied.Errors,
				fmt.Errorf("ingest: re-anchor annotation %d on entry %d: %w",
					ann.AnnotationID, item.EntryID, err))
			continue // Never roll the content write back over one failed annotation.
		}

		applied.Reanchored++
		applied.Remaps[item.EntryID] = append(applied.Remaps[item.EntryID], Remap{Old: oldID, New: newID})
	}

	if len(item.Post.Tags) > 0 {
		if err := src.AddTags(ctx, entryID, item.Post.Tags); err != nil {
			logger.Error("ingest: add tags failed", "entry_id", item.EntryID, "error", err)
			applied.Errors = append(applied.Errors,
				fmt.Errorf("ingest: add tags to entry %d: %w", item.EntryID, err))
		}
	}
}
