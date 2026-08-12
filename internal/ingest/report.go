package ingest

import (
	"fmt"
	"io"
)

// WriteReport renders plan — and, once a run has actually written anything,
// applied — as the operator-facing summary both a dry run and a completed
// `-commit` run print. applied is nil for a dry run: nothing has been sent
// to wallabag yet, so there is nothing to report beyond what the plan
// intends to do.
//
// Never prints a credential or the Substack session cookie, and cannot be
// made to by accident: neither Plan nor Applied carries one anywhere in
// their field trees (see plan.go and apply.go) — those live only in
// substack.Config and wallabag.Config, neither of which this function, or
// anything it is passed, ever sees. There is no redaction logic here because
// there is nothing here to redact.
func WriteReport(w io.Writer, plan Plan, applied *Applied) error {
	var writeErr error
	printf := func(format string, args ...any) {
		if writeErr != nil {
			return
		}
		_, writeErr = fmt.Fprintf(w, format, args...)
	}

	counts := map[Action]int{}
	var (
		grewCount      int
		contentChanges []Item
	)
	for _, item := range plan.Items {
		counts[item.Action]++
		if item.ContentGrew {
			grewCount++
		}
		if item.Action == ActionUpdate {
			contentChanges = append(contentChanges, item)
		}
	}

	printf("Plan: %d post(s) considered\n", len(plan.Items))
	printf("  create:            %d\n", counts[ActionCreate])
	printf("  update content:    %d\n", counts[ActionUpdate])
	printf("  annotations only:  %d\n", counts[ActionAnnotationsOnly])
	printf("  skip (up to date): %d\n", counts[ActionSkip])
	printf("  conflict:          %d\n", counts[ActionConflict])
	printf("\n")

	// This is the operator's actual reason for running the importer, so it
	// gets its own headline number rather than being folded into the plain
	// update count above: replacing a paywall preview with the full article
	// is only worth doing because it puts the article back in front of the
	// reader, even over material already marked done. See
	// Item.ContentGrew's own comment for the growth-ratio rule behind this
	// count, and RequeueDocumentRoot for what actually happens to each one.
	printf("Returning to the reading queue (content grew materially): %d\n", grewCount)
	if len(contentChanges) > 0 {
		printf("Content size, old -> new bytes (an estimate — see the note on wallabag's own\n")
		printf("HTML normalisation below):\n")
		for _, item := range contentChanges {
			note := ""
			if item.ContentGrew {
				note = "  -> grew enough to return to the queue, due today"
			}
			printf("  entry %d (%s): %d -> %d%s\n",
				item.EntryID, item.Post.URL, item.OldBytes, item.NewBytes, note)
		}
	}
	printf("\n")

	if plan.Conflicts > 0 {
		printf("Conflicts — two or more annotated wallabag entries share one Substack post,\n")
		printf("so nothing was planned for it; resolve which entry is current by hand:\n")
		for _, item := range plan.Items {
			if item.Action != ActionConflict {
				continue
			}
			printf("  %s\n", item.Post.URL)
			for _, note := range item.Notes {
				printf("    %s\n", note)
			}
		}
		printf("\n")
	}

	printf("Occurrence counts below are computed against the content this run intends to\n")
	printf("upload, not against what wallabag ends up actually storing — wallabag may\n")
	printf("normalise stored HTML in ways this has only been confirmed correct for on\n")
	printf("simple markup, so treat a count for any entry whose content is changing as an\n")
	printf("estimate. Where a quote matches more than once, re-anchoring always takes the\n")
	printf("FIRST occurrence in document order — it never picks the occurrence nearest the\n")
	printf("highlight's original position, because nothing in this pipeline computes or\n")
	printf("stores what that original position even was.\n\n")

	var (
		needsWork []Item
		missing   []AnnotationPlan
	)
	for _, item := range plan.Items {
		var hasWork bool
		for _, ann := range item.Annotations {
			if ann.Verdict != VerdictAnchored {
				hasWork = true
			}
			if ann.Verdict == VerdictMissing {
				missing = append(missing, ann)
			}
		}
		if hasWork {
			needsWork = append(needsWork, item)
		}
	}

	if len(needsWork) > 0 {
		printf("Entries with annotations needing re-anchoring:\n")
		for _, item := range needsWork {
			printf("  entry %d (%s)\n", item.EntryID, item.Post.URL)
			for _, ann := range item.Annotations {
				if ann.Verdict == VerdictAnchored {
					continue
				}
				truncateNote := ""
				if ann.Truncates {
					truncateNote = " [quote exceeds wallabag's ~900-byte limit and will be truncated on re-anchor, so it will not adopt onto the existing local row by exact match]"
				}
				trimNote := ""
				if ann.TrimmedMatch {
					trimNote = " [matched only after stripping wallabag's own truncation marker from the stored quote — the raw stored text does not occur in this content verbatim]"
				}
				printf("    annotation %d: %s, %d occurrence(s)%s%s\n",
					ann.AnnotationID, ann.Verdict, ann.Occurrences, truncateNote, trimNote)
			}
		}
		printf("\n")
	}

	if len(missing) > 0 {
		printf("Quotes absent from the new content entirely — these cannot be re-anchored\n")
		printf("automatically, and Apply leaves them completely untouched: fix them by hand.\n")
		printf("Their existing ranges still point into content that has since been replaced,\n")
		printf("so wallabag's own reader may draw the highlight in the wrong place (or not at\n")
		printf("all) until then. That wrong position is deliberate, not an oversight: wiping\n")
		printf("the ranges would also erase the only durable copy of a highlight's full text\n")
		printf("for any quote wallabag itself has already truncated (see the ~900-byte limit\n")
		printf("noted above), so this trades a temporarily wrong position for not destroying\n")
		printf("data that cannot be recovered afterward.\n")
		for _, ann := range missing {
			printf("  annotation %d: %q\n", ann.AnnotationID, ann.Quote)
		}
		printf("\n")
	}

	if applied != nil {
		printf("Applied:\n")
		printf("  entries created:          %d\n", applied.Created)
		printf("  entries content-updated:  %d\n", applied.Updated)
		printf("  annotations re-anchored:  %d\n", applied.Reanchored)
		printf("  annotations skipped (missing quote, left untouched): %d\n", applied.Skipped)
		printf("  annotation failures:      %d\n", applied.AnnotationFailures)
		if len(applied.Errors) > 0 {
			printf("  errors:\n")
			for _, err := range applied.Errors {
				printf("    %v\n", err)
			}
		}
	}

	return writeErr
}
