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
	for _, item := range plan.Items {
		counts[item.Action]++
	}

	printf("Plan: %d post(s) considered\n", len(plan.Items))
	printf("  create:            %d\n", counts[ActionCreate])
	printf("  update content:    %d\n", counts[ActionUpdate])
	printf("  annotations only:  %d\n", counts[ActionAnnotationsOnly])
	printf("  skip (up to date): %d\n", counts[ActionSkip])
	printf("  conflict:          %d\n", counts[ActionConflict])
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
				printf("    annotation %d: %s, %d occurrence(s)%s\n",
					ann.AnnotationID, ann.Verdict, ann.Occurrences, truncateNote)
			}
		}
		printf("\n")
	}

	if len(missing) > 0 {
		printf("Quotes absent from the new content entirely — these cannot be re-anchored\n")
		printf("automatically; fix them by hand:\n")
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
