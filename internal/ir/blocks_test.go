package ir

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, source string) *Article {
	t.Helper()
	article, err := ParseArticle(source)
	if err != nil {
		t.Fatalf("ParseArticle: %v", err)
	}
	return article
}

func TestBlockEnumeration(t *testing.T) {
	tests := []struct {
		name string
		html string
		want []string
	}{
		{
			name: "paragraphs in document order",
			html: `<p>First.</p><p>Second.</p><p>Third.</p>`,
			want: []string{"First.", "Second.", "Third."},
		},
		{
			name: "inline markup does not split a block",
			html: `<p>Plain <em>emphasised</em> and <strong>bold</strong>.</p>`,
			want: []string{"Plain emphasised and bold."},
		},
		{
			name: "a wrapper containing blocks does not emit itself",
			html: `<div><p>Inner one.</p><p>Inner two.</p></div>`,
			want: []string{"Inner one.", "Inner two."},
		},
		{
			name: "a wrapper holding bare text does emit",
			html: `<div>Bare text in a div.</div>`,
			want: []string{"Bare text in a div."},
		},
		{
			name: "blockquote wrapping a paragraph emits once",
			html: `<blockquote><p>Quoted.</p></blockquote>`,
			want: []string{"Quoted."},
		},
		{
			name: "list items are separate blocks",
			html: `<ul><li>One</li><li>Two</li></ul>`,
			want: []string{"One", "Two"},
		},
		{
			name: "headings are blocks",
			html: `<h2>A heading</h2><p>Body.</p>`,
			want: []string{"A heading", "Body."},
		},
		{
			name: "empty blocks are skipped so indices stay meaningful",
			html: `<p>Real.</p><p></p><p>   </p><p>Also real.</p>`,
			want: []string{"Real.", "Also real."},
		},
		{
			name: "entities are decoded, matching what the DOM reports",
			html: `<p>Caf&eacute; &amp; bar</p>`,
			want: []string{"Café & bar"},
		},
		{
			name: "nested inline elements flatten into one text run",
			html: `<p>a <em>b <strong>c</strong> d</em> e</p>`,
			want: []string{"a b c d e"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			article := mustParse(t, test.html)

			got := make([]string, 0, article.Len())
			for _, block := range article.Blocks() {
				got = append(got, block.Text)
			}

			if len(got) != len(test.want) {
				t.Fatalf("got %d blocks %q, want %d %q", len(got), got, len(test.want), test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Errorf("block %d = %q, want %q", i, got[i], test.want[i])
				}
			}
			// Indices must match position, since every offset the browser
			// sends is looked up by index.
			for i, block := range article.Blocks() {
				if block.Index != i {
					t.Errorf("block at position %d reports index %d", i, block.Index)
				}
			}
		})
	}
}

// TestBlockTextMatchesTextContent is the load-bearing invariant of the whole
// addressing scheme: Block.Text must equal what the browser's textContent
// returns, because that is what selection offsets are measured against. Any
// whitespace normalisation here would shift every offset in the block.
func TestBlockTextMatchesTextContent(t *testing.T) {
	// Source indentation lands inside the paragraph as real characters, and
	// the browser reports them.
	source := "<p>\n  Indented text\n  across lines.\n</p>"
	article := mustParse(t, source)

	got := article.Blocks()[0].Text
	want := "\n  Indented text\n  across lines.\n"
	if got != want {
		t.Errorf("Block.Text = %q, want %q (verbatim, not normalised)", got, want)
	}
}

func TestRangeText(t *testing.T) {
	article := mustParse(t, `<p>The quick brown fox.</p><p>Jumps over.</p><p>The lazy dog.</p>`)

	tests := []struct {
		name  string
		given Range
		want  string
	}{
		{
			name:  "within one block",
			given: Range{StartBlock: 0, StartOffset: 4, EndBlock: 0, EndOffset: 15},
			want:  "quick brown",
		},
		{
			name:  "whole block",
			given: Range{StartBlock: 1, StartOffset: 0, EndBlock: 1, EndOffset: 11},
			want:  "Jumps over.",
		},
		{
			name:  "spanning two blocks",
			given: Range{StartBlock: 0, StartOffset: 10, EndBlock: 1, EndOffset: 5},
			want:  "brown fox.\n\nJumps",
		},
		{
			name:  "spanning three blocks includes the middle whole",
			given: Range{StartBlock: 0, StartOffset: 16, EndBlock: 2, EndOffset: 8},
			want:  "fox.\n\nJumps over.\n\nThe lazy",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := article.Text(test.given)
			if err != nil {
				t.Fatalf("Text: %v", err)
			}
			if got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

// TestByteOffsetHandlesMultibyteCharacters guards the seam between how a
// browser measures a selection (JavaScript string .length, i.e. UTF-16 code
// units — one per rune for everything article text contains) and how this
// package addresses text (Go byte offsets). A soft hyphen, a curly quote or
// an em dash is multiple bytes but one rune; treating a browser-reported
// offset as a byte offset without converting drifts the position for every
// character after the first multi-byte one, so any selection extending past
// it would silently address the wrong text.
func TestByteOffsetHandlesMultibyteCharacters(t *testing.T) {
	// "Amer­ican" — a soft hyphen (2 UTF-8 bytes, 1 rune) inside a word,
	// exactly the shape that comes from real justified-text sources.
	article := mustParse(t, "<p>Amer­ican eco­nomists.</p>")

	// A browser selecting "eco­nomists" would report these rune offsets:
	// "Amer­ican " (soft hyphen, then a trailing space) is 10 runes, and the
	// word itself, soft hyphen included, is 11 more.
	byteRange, ok := article.ByteRange(Range{StartBlock: 0, StartOffset: 10, EndBlock: 0, EndOffset: 21})
	if !ok {
		t.Fatalf("ByteRange reported the range as out of bounds")
	}

	got, err := article.Text(byteRange)
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if want := "eco­nomists"; got != want {
		t.Errorf("got %q, want %q — a byte offset was used where a rune offset was meant", got, want)
	}
}

func TestRangeValidation(t *testing.T) {
	article := mustParse(t, `<p>Short.</p><p>Also short.</p>`)

	tests := []struct {
		name  string
		given Range
	}{
		{"start block past the end", Range{StartBlock: 9, EndBlock: 9, EndOffset: 1}},
		{"end block past the end", Range{StartBlock: 0, EndBlock: 9, EndOffset: 1}},
		{"negative start block", Range{StartBlock: -1, EndBlock: 0, EndOffset: 1}},
		{"end before start", Range{StartBlock: 1, EndBlock: 0, EndOffset: 1}},
		{"start offset past the block", Range{StartBlock: 0, StartOffset: 999, EndBlock: 0, EndOffset: 1000}},
		{"end offset past the block", Range{StartBlock: 0, EndBlock: 0, EndOffset: 999}},
		{"empty range", Range{StartBlock: 0, StartOffset: 3, EndBlock: 0, EndOffset: 3}},
		{"inverted offsets in one block", Range{StartBlock: 0, StartOffset: 5, EndBlock: 0, EndOffset: 2}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := article.Valid(test.given); err == nil {
				t.Error("expected an error, got nil")
			}
			if _, err := article.Text(test.given); err == nil {
				t.Error("Text accepted an invalid range")
			}
		})
	}
}

// TestOffsetsAtBlockBoundaries covers the positions most likely to be
// off by one: the very start and the very end of a block.
func TestOffsetsAtBlockBoundaries(t *testing.T) {
	article := mustParse(t, `<p>abcdef</p><p>ghijkl</p>`)

	got, err := article.Text(Range{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 6})
	if err != nil {
		t.Fatalf("full block: %v", err)
	}
	if got != "abcdef" {
		t.Errorf("full block = %q, want %q", got, "abcdef")
	}

	// An end offset equal to the block length is valid — it is exclusive.
	got, err = article.Text(Range{StartBlock: 0, StartOffset: 5, EndBlock: 1, EndOffset: 1})
	if err != nil {
		t.Fatalf("boundary span: %v", err)
	}
	if got != "f\n\ng" {
		t.Errorf("boundary span = %q, want %q", got, "f\n\ng")
	}
}

func TestNormalizeSpace(t *testing.T) {
	tests := []struct {
		given string
		want  string
	}{
		{"already normal", "already normal"},
		{"  leading and trailing  ", "leading and trailing"},
		{"collapse\n\nblank\tlines", "collapse blank lines"},
		{"\n  indented\n  source\n", "indented source"},
		{"", ""},
		{"   ", ""},
	}

	for _, test := range tests {
		if got := NormalizeSpace(test.given); got != test.want {
			t.Errorf("NormalizeSpace(%q) = %q, want %q", test.given, got, test.want)
		}
	}
}

// TestQuoteValidationTolerance documents why comparisons go through
// NormalizeSpace: the browser's own separator between blocks differs by engine,
// so a byte-exact check would reject legitimate multi-block selections.
func TestQuoteValidationTolerance(t *testing.T) {
	article := mustParse(t, `<p>First para.</p><p>Second para.</p>`)

	server, err := article.Text(Range{StartBlock: 0, StartOffset: 0, EndBlock: 1, EndOffset: 12})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}

	// What different browsers might send for the same selection.
	for _, fromBrowser := range []string{
		"First para.\n\nSecond para.",
		"First para.\nSecond para.",
		"First para. Second para.",
	} {
		if NormalizeSpace(server) != NormalizeSpace(fromBrowser) {
			t.Errorf("normalised server text %q does not match browser text %q",
				NormalizeSpace(server), NormalizeSpace(fromBrowser))
		}
	}

	// A genuinely different selection must still be rejected.
	if NormalizeSpace(server) == NormalizeSpace("First para. Different text.") {
		t.Error("normalisation is too lenient: it matched a different passage")
	}
}

func TestParseArticleHandlesEmptyInput(t *testing.T) {
	for _, source := range []string{"", "   ", "<p></p>", "<!-- just a comment -->"} {
		article, err := ParseArticle(source)
		if err != nil {
			t.Fatalf("ParseArticle(%q): %v", source, err)
		}
		if article.Len() != 0 {
			t.Errorf("ParseArticle(%q) produced %d blocks, want 0", source, article.Len())
		}
		// Rendering an empty article must not panic.
		if out := article.Render(RenderOptions{ReadPoint: NoReadPoint}); strings.TrimSpace(out) != "" {
			t.Errorf("Render of an empty article = %q, want empty", out)
		}
	}
}
