package ir

import (
	"strconv"
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

func TestImageCollection(t *testing.T) {
	tests := []struct {
		name string
		html string
		want []Image
	}{
		{
			name: "a bare image between paragraphs trails the one before it",
			html: `<p>First.</p><img src="a.png" alt="A"><p>Second.</p>`,
			want: []Image{{AfterBlock: 0, Src: "a.png", Alt: "A"}},
		},
		{
			name: "an image before the first block has no block to trail",
			html: `<img src="a.png" alt="A"><p>First.</p>`,
			want: []Image{{AfterBlock: -1, Src: "a.png", Alt: "A"}},
		},
		{
			name: "a figure with a caption: the image precedes its own caption block",
			html: `<p>First.</p><figure><img src="a.png" alt="A"><figcaption>Caption.</figcaption></figure>`,
			want: []Image{{AfterBlock: 0, Src: "a.png", Alt: "A"}},
		},
		{
			name: "a captionless figure between paragraphs",
			html: `<p>First.</p><figure><img src="a.png" alt="A"></figure><p>Second.</p>`,
			want: []Image{{AfterBlock: 0, Src: "a.png", Alt: "A"}},
		},
		{
			name: "several images in document order",
			html: `<img src="a.png"><p>First.</p><img src="b.png"><img src="c.png">`,
			want: []Image{
				{AfterBlock: -1, Src: "a.png"},
				{AfterBlock: 0, Src: "b.png"},
				{AfterBlock: 0, Src: "c.png"},
			},
		},
		{
			name: "no images",
			html: `<p>Just text.</p>`,
			want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			article := mustParse(t, test.html)
			got := article.Images()
			if len(got) != len(test.want) {
				t.Fatalf("Images() = %+v, want %+v", got, test.want)
			}
			for i := range test.want {
				if got[i] != test.want[i] {
					t.Errorf("image %d = %+v, want %+v", i, got[i], test.want[i])
				}
			}
		})
	}
}

// TestImagesAreNotBlocks: a figure caption still becomes its own addressable
// block — captions are readable text — but the image beside it must never
// silently turn into one too, or an extract's block/offset addressing would
// disagree with itself between renders.
func TestImagesAreNotBlocks(t *testing.T) {
	article := mustParse(t, `<figure><img src="a.png"><figcaption>Caption.</figcaption></figure>`)

	blocks := article.Blocks()
	if len(blocks) != 1 || blocks[0].Text != "Caption." {
		t.Fatalf("Blocks() = %+v, want exactly one block, the caption", blocks)
	}
}

// TestRenderSkipsUnresolvedImages: an image with no entry in ImageURLs is
// omitted entirely rather than rendered with its original address — falling
// back would defeat the point of resolving images server-side in the first
// place, see RenderOptions.ImageURLs.
func TestRenderSkipsUnresolvedImages(t *testing.T) {
	article := mustParse(t, `<p>First.</p><img src="a.png" alt="A">`)

	out := article.Render(RenderOptions{ReadPoint: NoReadPoint})
	if strings.Contains(out, "<img") {
		t.Errorf("an unresolved image was rendered anyway:\n%s", out)
	}
}

// TestRenderEmitsResolvedImages checks both that a resolved image renders at
// all, and that it lands after the block it trailed rather than at some
// arbitrary position.
func TestRenderEmitsResolvedImages(t *testing.T) {
	article := mustParse(t, `<p>First.</p><img src="a.png" alt="A caption"><p>Second.</p>`)

	out := article.Render(RenderOptions{
		ReadPoint: NoReadPoint,
		ImageURLs: map[string]ResolvedImage{"a.png": {URL: "/documents/1/images/1"}},
	})

	firstEnd := strings.Index(out, `data-b="0">First.</p>`)
	imgAt := strings.Index(out, `<img src="/documents/1/images/1" alt="A caption"`)
	secondAt := strings.Index(out, `data-b="1">Second.`)
	if firstEnd == -1 || imgAt == -1 || secondAt == -1 {
		t.Fatalf("render did not contain the expected pieces:\n%s", out)
	}
	if !(firstEnd < imgAt && imgAt < secondAt) {
		t.Errorf("image did not land between the two blocks it sits between:\n%s", out)
	}
}

// TestRenderEmitsSizeAttributesOnlyWhenBothAreKnown: width and height must
// appear together, so the browser can reserve the image's box before its
// bytes decode (see migrations/011_image_dimensions.sql and app.css's
// #article img rule), and must be absent — not written as "0" — when either
// dimension is unknown, since a literal 0x0 box would be worse than no
// hint at all.
func TestRenderEmitsSizeAttributesOnlyWhenBothAreKnown(t *testing.T) {
	tests := []struct {
		name          string
		width, height int
		wantAttrs     bool
	}{
		{"both known", 800, 600, true},
		{"width unknown", 0, 600, false},
		{"height unknown", 800, 0, false},
		{"both unknown", 0, 0, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			article := mustParse(t, `<img src="a.png" alt="A">`)

			out := article.Render(RenderOptions{
				ReadPoint: NoReadPoint,
				ImageURLs: map[string]ResolvedImage{
					"a.png": {URL: "/documents/1/images/1", Width: test.width, Height: test.height},
				},
			})

			hasWidth := strings.Contains(out, `width="`)
			hasHeight := strings.Contains(out, `height="`)
			if hasWidth != test.wantAttrs || hasHeight != test.wantAttrs {
				t.Errorf("Render() = %q, want width/height attrs present = %v", out, test.wantAttrs)
			}
			if test.wantAttrs {
				want := `width="` + strconv.Itoa(test.width) + `" height="` + strconv.Itoa(test.height) + `"`
				if !strings.Contains(out, want) {
					t.Errorf("Render() = %q, want to contain %q", out, want)
				}
			}
			// loading="lazy" is only safe once the box is reserved, so the two
			// must always travel together — see renderImage.
			if !strings.Contains(out, `loading="lazy"`) {
				t.Errorf("Render() = %q, missing loading=\"lazy\"", out)
			}
		})
	}
}

// TestRenderEscapesImageAttributes: alt text and a resolved src both come
// from outside increader (the article, and this package's own caller,
// respectively), so both are re-escaped here rather than trusted — the same
// defence-in-depth openTag already applies to link hrefs.
func TestRenderEscapesImageAttributes(t *testing.T) {
	article := mustParse(t, `<img src="a.png" alt="&quot;&gt;&lt;script&gt;alert(1)&lt;/script&gt;">`)

	out := article.Render(RenderOptions{
		ReadPoint: NoReadPoint,
		ImageURLs: map[string]ResolvedImage{"a.png": {URL: `x" onerror="alert(1)`}},
	})

	if strings.Contains(out, "<script>") || strings.Contains(out, `onerror="alert`) {
		t.Errorf("image attributes were not escaped:\n%s", out)
	}
}

// TestTablesAreNotBlocks: before Table existed, blockTags included td/th, so
// each cell of a grid became its own disconnected paragraph — a comparison
// table rendered as an unreadable stream of headers and values with no row
// or column left to tell them apart. A table must produce zero Blocks now,
// the same way an image already does.
func TestTablesAreNotBlocks(t *testing.T) {
	article := mustParse(t, `<p>Before.</p>`+
		`<table><tbody><tr><th>Speakers</th><td>83 million</td></tr></tbody></table>`+
		`<p>After.</p>`)

	blocks := article.Blocks()
	if len(blocks) != 2 || blocks[0].Text != "Before." || blocks[1].Text != "After." {
		t.Fatalf("Blocks() = %+v, want exactly the two paragraphs either side of the table", blocks)
	}
}

// TestTableRendersAsTable checks the table survives Render as an actual
// <table>, in place between the blocks it sits among, rather than being
// dropped or flattened.
func TestTableRendersAsTable(t *testing.T) {
	article := mustParse(t, `<p>Before.</p>`+
		`<table><tbody><tr><th>Speakers</th><td>83 million</td></tr></tbody></table>`+
		`<p>After.</p>`)

	out := article.Render(RenderOptions{ReadPoint: NoReadPoint})

	beforeAt := strings.Index(out, `data-b="0">Before.`)
	tableAt := strings.Index(out, `<div class="table-wrap"><table>`)
	afterAt := strings.Index(out, `data-b="1">After.`)
	if beforeAt == -1 || tableAt == -1 || afterAt == -1 {
		t.Fatalf("render did not contain the expected pieces:\n%s", out)
	}
	if !(beforeAt < tableAt && tableAt < afterAt) {
		t.Errorf("table did not land between the two blocks it sits between:\n%s", out)
	}
	want := `<tr><th>Speakers</th><td>83 million</td></tr>`
	if !strings.Contains(out, want) {
		t.Errorf("Render() = %q, want to contain %q", out, want)
	}
}

// TestTableSpanAttributes: colspan/rowspan are what keep a merged header
// cell honest, so they have to survive; a value that does not look like a
// small integer must not, since renderTableChild writes it straight into
// raw HTML rather than trusting the sanitiser already caught it.
func TestTableSpanAttributes(t *testing.T) {
	tests := []struct {
		name string
		attr string
		want string
	}{
		{"an ordinary colspan survives", `colspan="2"`, `colspan="2"`},
		{"an ordinary rowspan survives", `rowspan="3"`, `rowspan="3"`},
		// Single-quoted so the value the HTML tokenizer hands to node.Attr
		// genuinely contains a literal double quote — if writeSpanAttr ever
		// stopped re-checking and copied that value straight into the
		// double-quoted attribute this function itself writes, it would
		// break out of it and inject onmouseover as a real attribute.
		{"a value breaking attribute-value context is dropped", `colspan='2" onmouseover="alert(1)'`, ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			article := mustParse(t, `<table><tbody><tr><td `+test.attr+`>Cell</td></tr></tbody></table>`)
			out := article.Render(RenderOptions{ReadPoint: NoReadPoint})

			if test.want != "" && !strings.Contains(out, test.want) {
				t.Errorf("Render() = %q, want to contain %q", out, test.want)
			}
			if test.want == "" && (strings.Contains(out, "colspan") || strings.Contains(out, "onmouseover")) {
				t.Errorf("Render() = %q, want the malformed attribute dropped entirely", out)
			}
		})
	}
}

// TestTableImagesAreResolved: an image inside a table must go through the
// same resolve-and-cache map as any other article image (see
// web.resolveImages) rather than pointing straight at its original host —
// the same privacy reasoning renderImage already applies, just reached by a
// different path since a table is walked separately.
func TestTableImagesAreResolved(t *testing.T) {
	article := mustParse(t, `<table><tbody><tr><td><img src="flag.png" alt="Flag"></td></tr></tbody></table>`)

	// Unresolved: dropped, the same as a bare article image with nothing in
	// ImageURLs.
	out := article.Render(RenderOptions{ReadPoint: NoReadPoint})
	if strings.Contains(out, "<img") {
		t.Errorf("an unresolved table image was rendered anyway:\n%s", out)
	}

	// Article.Images() has to surface it too, or resolveImages never learns
	// there was anything here to fetch in the first place.
	images := article.Images()
	if len(images) != 1 || images[0].Src != "flag.png" {
		t.Fatalf("Images() = %+v, want the one image inside the table", images)
	}

	// Resolved: rendered with the cached address, not the original.
	out = article.Render(RenderOptions{
		ReadPoint: NoReadPoint,
		ImageURLs: map[string]ResolvedImage{"flag.png": {URL: "/documents/1/images/1"}},
	})
	if !strings.Contains(out, `<img src="/documents/1/images/1" alt="Flag" loading="lazy">`) {
		t.Errorf("Render() = %q, want the resolved image inline in the cell", out)
	}
}

// TestTableLinksAreSanitized: a link inside a table gets the same href
// re-check openTag already applies to one inside an ordinary extract —
// renderTableChild must not have quietly reopened a hole article.HTML
// already closed just because the surrounding element is a <table> instead
// of a <p>.
func TestTableLinksAreSanitized(t *testing.T) {
	article := mustParse(t, `<table><tbody><tr><td><a href="javascript:alert(1)">Click</a></td></tr></tbody></table>`)

	out := article.Render(RenderOptions{ReadPoint: NoReadPoint})
	if strings.Contains(out, "javascript:") {
		t.Errorf("Render() = %q, want the unsafe href dropped", out)
	}
	if !strings.Contains(out, "Click") {
		t.Errorf("Render() = %q, want the link's text to survive even without its href", out)
	}
}

// TestTableTextIsNotLocatable documents the trade-off directly: since a
// table's cells are no longer Blocks, Locate — the mechanism that turns a
// wallabag/KOReader highlight's quoted text, or a fresh selection in this
// reader, into a Range — can never find a passage that lives only inside a
// table. That mirrors an image already being unselectable, and it is the
// same reasoning: a grid has no "passage" for a highlight to be a substring
// of. An import that hits this is not silently lost — insertHighlights
// stores it unanchored, and it renders on its own via annotationHTML rather
// than in place — but it will not appear as a mark in the table itself.
func TestTableTextIsNotLocatable(t *testing.T) {
	article := mustParse(t, `<p>Before.</p>`+
		`<table><tbody><tr><th>Speakers</th><td>83 million</td></tr></tbody></table>`)

	if _, found := article.Locate("83 million"); found {
		t.Errorf("Locate found text that only exists inside a table; want it unlocatable")
	}
}
