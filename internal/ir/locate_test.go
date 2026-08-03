package ir

import "testing"

func TestLocate(t *testing.T) {
	source := "<p>The quick brown fox.</p>" +
		"<p>\n  It jumps over\n  the lazy dog.\n</p>" +
		"<p>A third paragraph.</p>"

	tests := []struct {
		name  string
		quote string
		want  Range
		found bool
	}{
		{
			name:  "exact match in the first block",
			quote: "quick brown",
			want:  Range{StartBlock: 0, StartOffset: 4, EndBlock: 0, EndOffset: 15},
			found: true,
		},
		{
			name:  "match in a later block",
			quote: "A third paragraph.",
			want:  Range{StartBlock: 2, StartOffset: 0, EndBlock: 2, EndOffset: 18},
			found: true,
		},
		{
			// The other system's copy of the article was wrapped differently,
			// so its quote has single spaces where this copy has newlines.
			name:  "whitespace differences are ignored",
			quote: "It jumps over the lazy dog.",
			want:  Range{StartBlock: 1, StartOffset: 3, EndBlock: 1, EndOffset: 32},
			found: true,
		},
		{
			name:  "leading and trailing whitespace in the quote is ignored",
			quote: "   quick brown   ",
			want:  Range{StartBlock: 0, StartOffset: 4, EndBlock: 0, EndOffset: 15},
			found: true,
		},
		{
			name:  "a passage that is not there is not found",
			quote: "this text appears nowhere",
			found: false,
		},
		{
			// A quote of any real length very often crosses a paragraph
			// break — this is the case that broke a real wallabag-imported
			// highlight and never showed it as a mark in the parent article.
			name:  "a quote spanning blocks is located",
			quote: "quick brown fox. It jumps over",
			want:  Range{StartBlock: 0, StartOffset: 4, EndBlock: 1, EndOffset: 16},
			found: true,
		},
		{
			name:  "an empty quote is not located",
			quote: "   ",
			found: false,
		},
	}

	article := mustParse(t, source)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, found := article.Locate(test.quote)

			if found != test.found {
				t.Fatalf("found = %v, want %v (got %+v)", found, test.found, got)
			}
			if !test.found {
				return
			}
			if got != test.want {
				t.Errorf("got %+v, want %+v", got, test.want)
			}

			// The located range must actually cover the quoted text, which is
			// the only thing that makes the resulting highlight meaningful.
			text, err := article.Text(got)
			if err != nil {
				t.Fatalf("Text%v: %v", got, err)
			}
			if NormalizeSpace(text) != NormalizeSpace(test.quote) {
				t.Errorf("located range covers %q, want %q",
					NormalizeSpace(text), NormalizeSpace(test.quote))
			}
		})
	}
}

// TestLocateRoundTripsEveryBlock is a broader sweep: whatever text a block
// holds, locating it must land back on that block.
func TestLocateRoundTripsEveryBlock(t *testing.T) {
	article := mustParse(t,
		`<h2>A heading</h2>`+
			`<p>Plain <em>emphasised</em> and <strong>bold</strong> text.</p>`+
			"<pre>  indented   code  </pre>"+
			`<ul><li>An item</li><li>Another item</li></ul>`)

	for _, block := range article.Blocks() {
		got, found := article.Locate(block.Text)
		if !found {
			t.Errorf("block %d (%q) could not be located", block.Index, block.Text)
			continue
		}
		if got.StartBlock != block.Index {
			t.Errorf("block %d located in block %d", block.Index, got.StartBlock)
		}

		text, err := article.Text(got)
		if err != nil {
			t.Fatalf("Text: %v", err)
		}
		if NormalizeSpace(text) != NormalizeSpace(block.Text) {
			t.Errorf("block %d round trip: %q, want %q",
				block.Index, NormalizeSpace(text), NormalizeSpace(block.Text))
		}
	}
}

// TestLocateRecoversAWallabagTruncatedQuote guards the fallback for a real
// observed case: wallabag's own annotation storage silently truncates a long
// quote and marks the cut with a trailing "…" (U+2026, no leading space) —
// text that can never appear in the article itself, so the untruncated quote
// can never match. Locate must still find where the passage started, from
// what text it does have.
func TestLocateRecoversAWallabagTruncatedQuote(t *testing.T) {
	source := "<p>The quick brown fox jumps over the lazy dog.</p>" +
		"<p>A second, unrelated paragraph.</p>"
	article := mustParse(t, source)

	got, found := article.Locate("The quick brown fox jumps over the la…")
	if !found {
		t.Fatal("a wallabag-truncated quote was not located")
	}

	want := Range{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 37}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	text, err := article.Text(got)
	if err != nil {
		t.Fatalf("Text%v: %v", got, err)
	}
	if text != "The quick brown fox jumps over the la" {
		t.Errorf("located range covers %q, want the untruncated prefix", text)
	}
}

// TestLocateDoesNotMisreadARealTrailingEllipsis guards the other direction:
// a quote that genuinely ends in "…" in the article itself must match as
// exactly that — including the ellipsis — on the first attempt, never fall
// through to the truncation fallback and lose it.
func TestLocateDoesNotMisreadARealTrailingEllipsis(t *testing.T) {
	article := mustParse(t, "<p>Well, this is awkward…</p>")

	got, found := article.Locate("this is awkward…")
	if !found {
		t.Fatal("a quote genuinely ending in an ellipsis was not located")
	}

	text, err := article.Text(got)
	if err != nil {
		t.Fatalf("Text%v: %v", got, err)
	}
	if text != "this is awkward…" {
		t.Errorf("located range covers %q, want the ellipsis included", text)
	}
}

func TestLocateHandlesUnicode(t *testing.T) {
	article := mustParse(t, `<p>Un café très chaud — naturellement.</p>`)

	got, found := article.Locate("café très chaud")
	if !found {
		t.Fatal("multi-byte text could not be located")
	}

	text, err := article.Text(got)
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if text != "café très chaud" {
		t.Errorf("located range covers %q, want %q", text, "café très chaud")
	}
}
