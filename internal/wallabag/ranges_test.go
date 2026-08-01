package wallabag

import (
	"reflect"
	"testing"
)

// TestComputeRangesSimpleParagraph is the base case: a quote entirely inside
// one paragraph, no nesting to complicate the path.
func TestComputeRangesSimpleParagraph(t *testing.T) {
	html := `<p>Before text. </p><p>The quick brown fox jumps over the lazy dog.</p><p>After text.</p>`

	got := computeRanges(html, "quick brown fox")
	want := []serializedRange{{Start: "/p[2]", StartOffset: "4", End: "/p[2]", EndOffset: "19"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("computeRanges = %+v, want %+v", got, want)
	}
}

// TestComputeRangesWrappedInDiv mirrors the real, empirically-confirmed shape
// of a native wallabag annotation seen in this account: /div[1]/p[10]. Some
// source articles' readability-parsed content wraps everything in one root
// div, and paragraphs are one level inside it rather than direct children of
// <body>.
func TestComputeRangesWrappedInDiv(t *testing.T) {
	htmlDoc := "<div>" +
		"<p>Paragraph one.</p>" +
		"<p>Paragraph two.</p>" +
		"<p>The target passage is right here.</p>" +
		"</div>"

	got := computeRanges(htmlDoc, "target passage")
	want := []serializedRange{{Start: "/div[1]/p[3]", StartOffset: "4", End: "/div[1]/p[3]", EndOffset: "18"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("computeRanges = %+v, want %+v", got, want)
	}
}

// TestComputeRangesCountsOnlySameTagSiblings guards the one detail most
// likely to be gotten wrong by accident: annotator's xpath position counts
// only siblings sharing the boundary element's own tag name, so a heading
// interleaved between paragraphs must not shift a later paragraph's number.
func TestComputeRangesCountsOnlySameTagSiblings(t *testing.T) {
	htmlDoc := "<h2>A heading</h2>" +
		"<p>First paragraph.</p>" +
		"<h2>Another heading</h2>" +
		"<p>The quick brown fox.</p>"

	got := computeRanges(htmlDoc, "quick brown fox")
	// The target is the second <p>, despite being the fourth element overall
	// and the second <h2> having already appeared.
	want := []serializedRange{{Start: "/p[2]", StartOffset: "4", End: "/p[2]", EndOffset: "19"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("computeRanges = %+v, want %+v", got, want)
	}
}

// TestComputeRangesAcrossParagraphsWithNoGap covers the reason the search is
// whitespace-tolerant down to zero characters, not just "any run matches any
// run": adjacent block elements carry no whitespace between their text nodes
// at the DOM level at all, unlike increader's own multi-block quote, which
// joins separate blocks with a blank line for readability.
func TestComputeRangesAcrossParagraphsWithNoGap(t *testing.T) {
	htmlDoc := `<p>First paragraph ends here.</p><p>Second paragraph starts here.</p>`

	quote := "ends here.\n\nSecond paragraph starts"
	got := computeRanges(htmlDoc, quote)
	if got == nil {
		t.Fatal("computeRanges = nil, want a match spanning the paragraph boundary")
	}
	if got[0].Start != "/p[1]" || got[0].End != "/p[2]" {
		t.Errorf("range = %+v, want start in the first paragraph and end in the second", got[0])
	}
}

// TestComputeRangesBoundaryInsideInlineElement checks that when a selection
// boundary falls inside inline markup, the path resolves to that inline
// element itself — its direct parent, exactly as xpath-range's own
// serialization() does — not the enclosing block.
func TestComputeRangesBoundaryInsideInlineElement(t *testing.T) {
	htmlDoc := `<p>Some text with <strong>an emphasised phrase</strong> in the middle.</p>`

	got := computeRanges(htmlDoc, "emphasised phrase")
	want := []serializedRange{{Start: "/p[1]/strong[1]", StartOffset: "3", End: "/p[1]/strong[1]", EndOffset: "20"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("computeRanges = %+v, want %+v", got, want)
	}
}

// TestComputeRangesBoundaryStraddlesInlineElement covers a start and end
// landing in different elements at different depths — the ordinary case for
// any selection that starts before, or ends after, a run of inline markup.
func TestComputeRangesBoundaryStraddlesInlineElement(t *testing.T) {
	htmlDoc := `<p>Some <strong>bold</strong> and plain text after.</p>`

	got := computeRanges(htmlDoc, "bold and plain")
	if got == nil {
		t.Fatal("computeRanges = nil")
	}
	if got[0].Start != "/p[1]/strong[1]" {
		t.Errorf("start = %q, want the start to resolve inside the inline element it begins in", got[0].Start)
	}
	if got[0].End != "/p[1]" {
		t.Errorf("end = %q, want the end to resolve to the paragraph, since it falls in plain text after the inline element", got[0].End)
	}
}

// TestComputeRangesNotFound is the fallback path: an extract's quote will
// not always be findable verbatim (the article changed, or the two parses
// diverge more than the whitespace tolerance can absorb), and that must
// produce no ranges rather than a wrong one or an error CreateHighlight
// would have to handle specially.
func TestComputeRangesNotFound(t *testing.T) {
	htmlDoc := `<p>This article says nothing of the sort.</p>`

	if got := computeRanges(htmlDoc, "a quote that does not appear"); got != nil {
		t.Errorf("computeRanges = %+v, want nil for a quote not present in the article", got)
	}
}

// TestComputeRangesFirstOccurrenceWins: an ambiguous match (the same text
// appearing twice) is resolved by taking the first occurrence, the same
// pragmatic choice increader's own paste-to-extract fallback makes for the
// identical ambiguity.
func TestComputeRangesFirstOccurrenceWins(t *testing.T) {
	htmlDoc := `<p>The repeated phrase appears here.</p><p>The repeated phrase appears here too.</p>`

	got := computeRanges(htmlDoc, "The repeated phrase")
	if got == nil || got[0].Start != "/p[1]" {
		t.Errorf("computeRanges = %+v, want the first paragraph", got)
	}
}
