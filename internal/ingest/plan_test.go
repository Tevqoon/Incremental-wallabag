package ingest

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/wallabag"
)

// anchoredRange builds one xpath-range object of the exact shape
// internal/wallabag's ranges.go expects — {start, startOffset, end,
// endOffset}, all strings — for a single top-level <p> element (path
// "/p[1]") holding one text node. This is hand-built rather than produced by
// calling into the wallabag package's own (unexported) computeRanges,
// because plan.go's whole point is to be testable against literals with no
// dependency on any of wallabag's internals; the two packages agreeing on
// the wire shape is exactly what wallabag's own exported QuoteAnchored is
// there to let a caller here rely on without reaching past it.
func anchoredRange(startOffset, endOffset int) []json.RawMessage {
	raw := fmt.Sprintf(`{"start":"/p[1]","startOffset":"%d","end":"/p[1]","endOffset":"%d"}`,
		startOffset, endOffset)
	return []json.RawMessage{json.RawMessage(raw)}
}

// TestBuildPlanClassifiesEachCombinationOfContentAndAnnotationState is the
// classification table itself: no wallabag match at all, an existing entry
// whose content is still a preview, an entry whose content is already full
// but carries a stale annotation, and an entry that is fully up to date —
// four states, four different Actions.
func TestBuildPlanClassifiesEachCombinationOfContentAndAnnotationState(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	fullContent := "<p>The quick brown fox jumps over the lazy dog.</p>"
	quote := "The quick brown fox jumps over the lazy dog."

	tests := []struct {
		name       string
		post       source.Document
		snap       Snapshot
		wantAction Action
	}{
		{
			name:       "no wallabag entry matches this slug at all",
			post:       source.Document{URL: "https://example.substack.com/p/never-seen", ContentHTML: fullContent},
			snap:       Snapshot{},
			wantAction: ActionCreate,
		},
		{
			name: "matched entry's content is still the preview",
			post: source.Document{URL: "https://example.substack.com/p/a-post", ContentHTML: fullContent},
			snap: Snapshot{
				BySlug: map[string][]wallabag.Entry{"a-post": {{ID: 1, URL: "https://example.substack.com/p/a-post"}}},
				Details: map[int]wallabag.Entry{
					1: {ID: 1, Content: "<p>Subscribe to keep reading…</p>"},
				},
			},
			wantAction: ActionUpdate,
		},
		{
			name: "content already full, one annotation not anchored to it",
			post: source.Document{URL: "https://example.substack.com/p/a-post", ContentHTML: fullContent},
			snap: Snapshot{
				BySlug: map[string][]wallabag.Entry{"a-post": {{ID: 1, URL: "https://example.substack.com/p/a-post",
					Annotations: []wallabag.Annotation{{ID: 500, Quote: quote}}}}},
				Details: map[int]wallabag.Entry{
					1: {
						ID: 1, Content: fullContent,
						Annotations: []wallabag.Annotation{{ID: 500, Quote: quote}}, // no ranges: unanchored
					},
				},
			},
			wantAction: ActionAnnotationsOnly,
		},
		{
			name: "content already full, its one annotation is already anchored",
			post: source.Document{URL: "https://example.substack.com/p/a-post", ContentHTML: fullContent},
			snap: Snapshot{
				BySlug: map[string][]wallabag.Entry{"a-post": {{ID: 1, URL: "https://example.substack.com/p/a-post",
					Annotations: []wallabag.Annotation{{ID: 500, Quote: quote}}}}},
				Details: map[int]wallabag.Entry{
					1: {
						ID: 1, Content: fullContent,
						Annotations: []wallabag.Annotation{{ID: 500, Quote: quote, Ranges: anchoredRange(0, len(quote))}},
					},
				},
			},
			wantAction: ActionSkip,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := BuildPlan([]source.Document{test.post}, test.snap, now)
			if len(plan.Items) != 1 {
				t.Fatalf("got %d items, want 1", len(plan.Items))
			}
			if got := plan.Items[0].Action; got != test.wantAction {
				t.Errorf("Action = %q, want %q", got, test.wantAction)
			}
		})
	}
}

// TestBuildPlanRefusesTwoAnnotatedCandidates covers the conflict rule: two
// or more entries under one slug both carry annotations, so BuildPlan must
// plan nothing for the post at all rather than silently pick one and leave
// the other's highlights stranded.
func TestBuildPlanRefusesTwoAnnotatedCandidates(t *testing.T) {
	now := time.Now()
	post := source.Document{URL: "https://example.substack.com/p/a-post", ContentHTML: "<p>Full.</p>"}

	snap := Snapshot{
		BySlug: map[string][]wallabag.Entry{
			"a-post": {
				{ID: 1, URL: "https://example.substack.com/p/a-post",
					Annotations: []wallabag.Annotation{{ID: 500, Quote: "one"}}},
				{ID: 2, GivenURL: "https://open.substack.com/pub/example/p/a-post",
					Annotations: []wallabag.Annotation{{ID: 501, Quote: "two"}}},
			},
		},
	}

	plan := BuildPlan([]source.Document{post}, snap, now)
	if len(plan.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(plan.Items))
	}
	item := plan.Items[0]

	if item.Action != ActionConflict {
		t.Errorf("Action = %q, want %q", item.Action, ActionConflict)
	}
	if item.EntryID != 0 {
		t.Errorf("EntryID = %d, want 0 — a conflict must not silently pick an entry", item.EntryID)
	}
	if len(item.Annotations) != 0 {
		t.Errorf("Annotations = %+v, want none planned for a conflict", item.Annotations)
	}
	if plan.Conflicts != 1 {
		t.Errorf("plan.Conflicts = %d, want 1", plan.Conflicts)
	}
	if len(item.Notes) != 2 {
		t.Errorf("Notes = %v, want one line per annotated candidate (2)", item.Notes)
	}
}

// TestBuildPlanShadowsUnannotatedDuplicate covers the non-conflict duplicate
// case: neither candidate has annotations, so BuildPlan picks the newer one
// and records the other as shadowed rather than refusing outright — refusing
// is reserved for when a reader's own highlights are actually at stake.
func TestBuildPlanShadowsUnannotatedDuplicate(t *testing.T) {
	now := time.Now()
	older := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	post := source.Document{URL: "https://example.substack.com/p/a-post", ContentHTML: "<p>Full.</p>"}
	snap := Snapshot{
		BySlug: map[string][]wallabag.Entry{
			"a-post": {
				{ID: 1, URL: "https://example.substack.com/p/a-post", CreatedAt: wallabag.Time{Time: older}},
				{ID: 2, GivenURL: "https://example.substack.com/p/a-post?utm_source=x", CreatedAt: wallabag.Time{Time: newer}},
			},
		},
	}

	plan := BuildPlan([]source.Document{post}, snap, now)
	item := plan.Items[0]

	if item.Action == ActionConflict {
		t.Fatal("Action = conflict, want a duplicate resolved between two unannotated candidates")
	}
	if item.EntryID != 2 {
		t.Errorf("EntryID = %d, want 2 (the newer of the two)", item.EntryID)
	}
	if len(item.Notes) == 0 {
		t.Error("Notes is empty, want a note about the shadowed duplicate (entry 1)")
	}
}

// TestPlanAnnotationVerdicts is the four-verdict table plus Truncates,
// directly against planAnnotation.
func TestPlanAnnotationVerdicts(t *testing.T) {
	content := "<p>The quick brown fox jumps over the lazy dog. The quick brown fox again.</p>"

	tests := []struct {
		name        string
		annotation  wallabag.Annotation
		wantVerdict Verdict
		wantOccurs  int
	}{
		{
			name:        "ranges resolve to the quote unchanged",
			annotation:  wallabag.Annotation{ID: 1, Quote: "The quick brown fox jumps over the lazy dog.", Ranges: anchoredRange(0, len("The quick brown fox jumps over the lazy dog."))},
			wantVerdict: VerdictAnchored,
			// Occurrences is still computed even when already anchored — it
			// is informational for the report, not part of the anchored
			// decision — and this quote happens to occur once.
			wantOccurs: 1,
		},
		{
			name:        "no ranges, quote occurs exactly once",
			annotation:  wallabag.Annotation{ID: 2, Quote: "jumps over the lazy dog"},
			wantVerdict: VerdictUnique,
			wantOccurs:  1,
		},
		{
			name:        "no ranges, quote occurs more than once",
			annotation:  wallabag.Annotation{ID: 3, Quote: "The quick brown fox"},
			wantVerdict: VerdictAmbiguous,
			wantOccurs:  2,
		},
		{
			name:        "quote does not occur in this content at all",
			annotation:  wallabag.Annotation{ID: 4, Quote: "an entirely different sentence"},
			wantVerdict: VerdictMissing,
			wantOccurs:  0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := planAnnotation(test.annotation, content)
			if got.Verdict != test.wantVerdict {
				t.Errorf("Verdict = %q, want %q", got.Verdict, test.wantVerdict)
			}
			if got.Occurrences != test.wantOccurs {
				t.Errorf("Occurrences = %d, want %d", got.Occurrences, test.wantOccurs)
			}
			if got.AnnotationID != test.annotation.ID {
				t.Errorf("AnnotationID = %d, want %d", got.AnnotationID, test.annotation.ID)
			}
		})
	}
}

// TestPlanAnnotationTruncates pins the 900-byte cutoff separately: a quote
// past it will come back from wallabag shortened on re-anchor, missing the
// local exact-quote adopt path — this is what tells Apply and the report
// that this annotation is not a plain re-anchor.
func TestPlanAnnotationTruncates(t *testing.T) {
	short := "A short quote."
	long := ""
	for len(long) <= truncateLimit {
		long += "word "
	}

	if got := planAnnotation(wallabag.Annotation{Quote: short}, "<p>irrelevant</p>").Truncates; got {
		t.Error("Truncates = true for a short quote, want false")
	}
	if got := planAnnotation(wallabag.Annotation{Quote: long}, "<p>irrelevant</p>").Truncates; !got {
		t.Error("Truncates = false for a quote past the limit, want true")
	}
}

// TestBuildPlanIsIdempotentAfterACompletedApply is the idempotence
// requirement the whole design turns on: a Snapshot that already reflects a
// successful Apply — full content, every annotation anchored to it under its
// new id — must classify as ActionSkip with nothing left to do, so that
// running the importer again over material already handled writes nothing.
func TestBuildPlanIsIdempotentAfterACompletedApply(t *testing.T) {
	now := time.Now()
	content := "<p>The quick brown fox jumps over the lazy dog.</p>"
	quote := "The quick brown fox jumps over the lazy dog."

	post := source.Document{URL: "https://example.substack.com/p/a-post", ContentHTML: content}
	snap := Snapshot{
		BySlug: map[string][]wallabag.Entry{
			"a-post": {{ID: 1, URL: "https://example.substack.com/p/a-post",
				Annotations: []wallabag.Annotation{{ID: 999, Quote: quote, Ranges: anchoredRange(0, len(quote))}}}},
		},
		Details: map[int]wallabag.Entry{
			1: {ID: 1, Content: content,
				Annotations: []wallabag.Annotation{{ID: 999, Quote: quote, Ranges: anchoredRange(0, len(quote))}}},
		},
	}

	plan := BuildPlan([]source.Document{post}, snap, now)
	if len(plan.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(plan.Items))
	}
	if got := plan.Items[0].Action; got != ActionSkip {
		t.Errorf("Action = %q, want %q — a completed apply must classify as skip on the next run", got, ActionSkip)
	}
}
