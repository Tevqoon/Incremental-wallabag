package wallabag

import (
	"encoding/json"
	"reflect"
	"strings"
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

// TestRecoverQuoteRoundTrips is recoverQuote's core promise: whatever
// computeRanges resolved a quote to, recovering it back must return that
// same passage — the whole reason this exists is to get back exactly what a
// truncated wallabag quote could no longer describe on its own.
func TestRecoverQuoteRoundTrips(t *testing.T) {
	tests := []struct {
		name  string
		html  string
		quote string
	}{
		{
			name:  "simple paragraph",
			html:  `<p>Before text. </p><p>The quick brown fox jumps over the lazy dog.</p><p>After text.</p>`,
			quote: "quick brown fox",
		},
		{
			name:  "wrapped in a div",
			html:  "<div><p>Paragraph one.</p><p>Paragraph two.</p><p>The target passage is right here.</p></div>",
			quote: "target passage",
		},
		{
			name:  "spans a paragraph boundary with no gap between them",
			html:  `<p>First paragraph ends here.</p><p>Second paragraph starts here.</p>`,
			quote: "ends here.\n\nSecond paragraph starts",
		},
		{
			name:  "boundary inside inline markup",
			html:  `<p>Some text with <strong>an emphasised phrase</strong> in the middle.</p>`,
			quote: "emphasised phrase",
		},
		{
			name:  "boundary straddles inline markup",
			html:  `<p>Some <strong>bold</strong> and plain text after.</p>`,
			quote: "bold and plain",
		},
		{
			name: "a whole highlight far longer than wallabag's own quote field would keep",
			html: `<p>First paragraph of a long highlight.</p>` +
				`<p>Second paragraph, still part of the same highlight, going on for a while.</p>` +
				`<p>Third and final paragraph, well past where a 900-character quote would have been cut.</p>`,
			quote: "First paragraph of a long highlight.\n\nSecond paragraph, still part of the same highlight, going on for a while.\n\nThird and final paragraph, well past where a 900-character quote would have been cut.",
		},
	}

	// A trivial stand-in for ir.NormalizeSpace: this package deliberately
	// does not depend on ir, and the comparison only needs to agree that a
	// run of whitespace is a run of whitespace, not reproduce it exactly —
	// recoverQuote's actual caller (ir.Article.Locate) applies that same
	// normalisation to whatever it receives before ever comparing it to
	// anything, so a difference only in how much whitespace sits at a
	// boundary is not a real difference to that caller.
	normalize := func(s string) string { return strings.Join(strings.Fields(s), " ") }

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ranges := computeRanges(test.html, test.quote)
			if ranges == nil {
				t.Fatalf("test premise is wrong: computeRanges could not find %q", test.quote)
			}

			got, ok := recoverQuote(test.html, ranges)
			if !ok {
				t.Fatal("recoverQuote reported failure")
			}
			if normalize(got) != normalize(test.quote) {
				t.Errorf("recoverQuote = %q, want %q", got, test.quote)
			}
		})
	}
}

// TestRecoverQuoteFailsSafely covers the ways a range can fail to resolve —
// most plausibly because the article changed upstream since the highlight
// was made — without panicking or recovering garbage.
func TestRecoverQuoteFailsSafely(t *testing.T) {
	html := `<p>Some ordinary paragraph.</p>`

	tests := []struct {
		name   string
		html   string
		ranges []serializedRange
	}{
		{
			name:   "no ranges at all",
			html:   html,
			ranges: nil,
		},
		{
			name: "xpath names an element that is not there",
			html: html,
			ranges: []serializedRange{
				{Start: "/p[9]", StartOffset: "0", End: "/p[9]", EndOffset: "4"},
			},
		},
		{
			name: "offset is not a number",
			html: html,
			ranges: []serializedRange{
				{Start: "/p[1]", StartOffset: "not-a-number", End: "/p[1]", EndOffset: "4"},
			},
		},
		{
			name: "the article to resolve against no longer has any content",
			html: "",
			ranges: []serializedRange{
				{Start: "/p[1]", StartOffset: "0", End: "/p[1]", EndOffset: "4"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, ok := recoverQuote(test.html, test.ranges); ok {
				t.Error("recoverQuote reported success, want failure")
			}
		})
	}
}

// TestSourceResolveRange is the public entry point importHighlights and
// anchorHighlights actually use: decoding the raw JSON a Highlight carries
// and recovering text from it, matching source.RangeResolver.
func TestSourceResolveRange(t *testing.T) {
	html := `<p>Before text. </p><p>The quick brown fox jumps over the lazy dog.</p>`
	ranges := computeRanges(html, "quick brown fox")
	if ranges == nil {
		t.Fatal("test premise is wrong: computeRanges found nothing")
	}
	encoded, err := json.Marshal(ranges)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	source := &Source{}
	got, ok := source.ResolveRange(html, encoded)
	if !ok {
		t.Fatal("ResolveRange reported failure")
	}
	if got != "quick brown fox" {
		t.Errorf("ResolveRange = %q, want %q", got, "quick brown fox")
	}

	if _, ok := source.ResolveRange(html, nil); ok {
		t.Error("ResolveRange succeeded with no ranges, want failure")
	}
	if _, ok := source.ResolveRange(html, json.RawMessage(`not json`)); ok {
		t.Error("ResolveRange succeeded with malformed JSON, want failure")
	}
}
