package ir

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestExtractHTMLPreservesInlineMarkup(t *testing.T) {
	tests := []struct {
		name  string
		html  string
		given Range
		want  string
	}{
		{
			name:  "plain text slice",
			html:  `<p>The quick brown fox.</p>`,
			given: Range{StartBlock: 0, StartOffset: 4, EndBlock: 0, EndOffset: 15},
			want:  `<p>quick brown</p>`,
		},
		{
			name:  "emphasis wholly inside the range survives",
			html:  `<p>Plain <em>emphasised</em> tail.</p>`,
			given: Range{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 16},
			want:  `<p>Plain <em>emphasised</em></p>`,
		},
		{
			name:  "an element clipped in half keeps the surviving part",
			html:  `<p>Plain <em>emphasised</em> tail.</p>`,
			given: Range{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 10},
			want:  `<p>Plain <em>emph</em></p>`,
		},
		{
			name:  "an element entirely outside the range is dropped, not emptied",
			html:  `<p>Plain <em>emphasised</em> tail.</p>`,
			given: Range{StartBlock: 0, StartOffset: 17, EndBlock: 0, EndOffset: 22},
			want:  `<p>tail.</p>`,
		},
		{
			name:  "links keep their destination",
			html:  `<p>The <a href="https://example.com/x">quick brown</a> fox.</p>`,
			given: Range{StartBlock: 0, StartOffset: 4, EndBlock: 0, EndOffset: 15},
			want:  `<p><a href="https://example.com/x" rel="noopener noreferrer" target="_blank">quick brown</a></p>`,
		},
		{
			name:  "a partly selected link keeps its destination too",
			html:  `<p>The <a href="https://example.com/x">quick brown</a> fox.</p>`,
			given: Range{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 9},
			want:  `<p>The <a href="https://example.com/x" rel="noopener noreferrer" target="_blank">quick</a></p>`,
		},
		{
			name:  "nested inline elements are rebuilt",
			html:  `<p>a <em>b <strong>c</strong> d</em> e</p>`,
			given: Range{StartBlock: 0, StartOffset: 2, EndBlock: 0, EndOffset: 7},
			want:  `<p><em>b <strong>c</strong> d</em></p>`,
		},
		{
			name:  "each block becomes its own element",
			html:  `<p>First para.</p><p>Second para.</p>`,
			given: Range{StartBlock: 0, StartOffset: 6, EndBlock: 1, EndOffset: 6},
			want:  `<p>para.</p><p>Second</p>`,
		},
		{
			name:  "preformatted text keeps its tag",
			html:  "<pre>code here</pre>",
			given: Range{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 9},
			want:  `<pre>code here</pre>`,
		},
		{
			name:  "a list item becomes a paragraph outside its list",
			html:  `<ul><li>An item</li></ul>`,
			given: Range{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 7},
			want:  `<p>An item</p>`,
		},
		{
			// Offsets are measured against the decoded text "a < b & c"
			// (9 characters), not the 15-character source markup — the
			// browser measures the same way.
			name:  "special characters are re-escaped on the way out",
			html:  `<p>a &lt; b &amp; c</p>`,
			given: Range{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 9},
			want:  `<p>a &lt; b &amp; c</p>`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			article := mustParse(t, test.html)
			got, err := article.HTML(test.given)
			if err != nil {
				t.Fatalf("HTML: %v", err)
			}
			if got != test.want {
				t.Errorf("got  %s\nwant %s", got, test.want)
			}
		})
	}
}

// TestExtractHTMLRejectsDangerousHref is defence in depth. The sanitiser
// upstream should already have removed this, but HTML() emits raw markup into a
// template, so it must not depend on a guarantee made in another package.
func TestExtractHTMLRejectsDangerousHref(t *testing.T) {
	for _, href := range []string{
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"  javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)",
	} {
		article := mustParse(t, `<p><a href="`+href+`">click</a></p>`)
		got, err := article.HTML(Range{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 5})
		if err != nil {
			t.Fatalf("HTML: %v", err)
		}
		if got != `<p><a>click</a></p>` {
			t.Errorf("href %q produced %s, want the destination dropped", href, got)
		}
	}
}

func TestRenderMarksExtracts(t *testing.T) {
	tests := []struct {
		name  string
		html  string
		marks []Mark
		want  string
	}{
		{
			name: "no marks",
			html: `<p>Untouched text.</p>`,
			want: `<p data-b="0">Untouched text.</p>`,
		},
		{
			name: "one mark inside a paragraph",
			html: `<p>The quick brown fox.</p>`,
			marks: []Mark{{
				Range:     Range{StartBlock: 0, StartOffset: 4, EndBlock: 0, EndOffset: 15},
				ElementID: 7,
			}},
			want: `<p data-b="0">The <mark class="extract" data-element="7">quick brown</mark> fox.</p>`,
		},
		{
			name: "a mark spanning blocks marks each one",
			html: `<p>First para.</p><p>Second para.</p>`,
			marks: []Mark{{
				Range:     Range{StartBlock: 0, StartOffset: 6, EndBlock: 1, EndOffset: 6},
				ElementID: 3,
			}},
			want: `<p data-b="0">First <mark class="extract" data-element="3">para.</mark></p>` +
				`<p data-b="1"><mark class="extract" data-element="3">Second</mark> para.</p>`,
		},
		{
			name: "a mark crossing an inline element is split around it",
			html: `<p>a <em>bold</em> c</p>`,
			marks: []Mark{{
				Range:     Range{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 8},
				ElementID: 1,
			}},
			want: `<p data-b="0"><mark class="extract" data-element="1">a </mark>` +
				`<em><mark class="extract" data-element="1">bold</mark></em>` +
				`<mark class="extract" data-element="1"> c</mark></p>`,
		},
		{
			name: "block indices are stable across mixed block types",
			html: `<h2>Title</h2><ul><li>Item</li></ul><p>Body</p>`,
			want: `<h2 data-b="0">Title</h2><p class="list-item" data-b="1">Item</p><p data-b="2">Body</p>`,
		},
		{
			name: "a stale mark is skipped rather than breaking the page",
			html: `<p>Short.</p>`,
			marks: []Mark{{
				Range:     Range{StartBlock: 9, StartOffset: 0, EndBlock: 9, EndOffset: 5},
				ElementID: 1,
			}},
			want: `<p data-b="0">Short.</p>`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			article := mustParse(t, test.html)
			if got := article.Render(RenderOptions{Marks: test.marks, ReadPoint: NoReadPoint}); got != test.want {
				t.Errorf("got  %s\nwant %s", got, test.want)
			}
		})
	}
}

// TestRenderSamePageLinksOpenInPlace covers openTag's other job besides
// hrefs: a bare "#fragment" href — the shape web.rewriteSamePageLinks
// produces for a footnote that would otherwise point back at the article's
// own address — must not get target="_blank", or following it would open a
// second tab just to jump to an anchor on the page already open. id and
// class survive too, which is what gives that href somewhere to land and
// lets a footnote's number keep the styling telling it apart from the
// footnote's text.
func TestRenderSamePageLinksOpenInPlace(t *testing.T) {
	article := mustParse(t, `<p><a id="footnote-1" href="#footnote-anchor-1" class="footnote-number">1. </a>It took place in February.</p>`)

	got := article.Render(RenderOptions{ReadPoint: NoReadPoint})
	want := `<p data-b="0"><a href="#footnote-anchor-1" id="footnote-1" class="footnote-number">1. </a>It took place in February.</p>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestRenderExternalLinksStillOpenInANewTab is the regression guard beside
// the test above: an ordinary absolute link is not a same-page jump, and
// must keep opening in a new tab exactly as before.
func TestRenderExternalLinksStillOpenInANewTab(t *testing.T) {
	article := mustParse(t, `<p><a href="https://example.com/x">a link</a></p>`)

	got := article.Render(RenderOptions{ReadPoint: NoReadPoint})
	want := `<p data-b="0"><a href="https://example.com/x" rel="noopener noreferrer" target="_blank">a link</a></p>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestRenderMergesOverlappingMarks covers the normal workflow of re-reading a
// passage and extracting a longer version of it. Nested <mark> elements would
// render as meaningless darker bands.
func TestRenderMergesOverlappingMarks(t *testing.T) {
	article := mustParse(t, `<p>The quick brown fox jumps.</p>`)

	got := article.Render(RenderOptions{
		ReadPoint: NoReadPoint,
		Marks: []Mark{
			{Range: Range{StartBlock: 0, StartOffset: 4, EndBlock: 0, EndOffset: 15}, ElementID: 1},
			{Range: Range{StartBlock: 0, StartOffset: 10, EndBlock: 0, EndOffset: 19}, ElementID: 2},
		},
	})

	want := `<p data-b="0">The <mark class="extract" data-element="1">quick brown fox</mark> jumps.</p>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestRenderAndHTMLAgreeOnOffsets is the round trip that matters: whatever
// Render marks as highlighted must be exactly what HTML would extract for the
// same range. If these two walks ever disagree, highlights drift away from the
// text they represent.
func TestRenderAndHTMLAgreeOnOffsets(t *testing.T) {
	source := `<p>Intro text with <a href="https://example.com">a link</a> and <em>emphasis</em> here.</p>` +
		`<p>A second paragraph for spanning.</p>`
	article := mustParse(t, source)

	ranges := []Range{
		{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 5},
		{StartBlock: 0, StartOffset: 17, EndBlock: 0, EndOffset: 23},
		{StartBlock: 0, StartOffset: 6, EndBlock: 0, EndOffset: 37},
		{StartBlock: 0, StartOffset: 30, EndBlock: 1, EndOffset: 8},
	}

	for _, r := range ranges {
		wantText, err := article.Text(r)
		if err != nil {
			t.Fatalf("Text%v: %v", r, err)
		}

		// The text inside the <mark> elements of a render must reconstruct the
		// same passage Text reports.
		rendered := article.Render(RenderOptions{
			Marks:     []Mark{{Range: r, ElementID: 1}},
			ReadPoint: NoReadPoint,
		})
		gotText := textInsideMarks(t, rendered)

		if NormalizeSpace(gotText) != NormalizeSpace(wantText) {
			t.Errorf("range %v: marked text %q, want %q", r, gotText, wantText)
		}
	}
}

// textInsideMarks parses rendered output and reconstructs the highlighted
// passage, joining blocks the same way Article.Text does so the two are
// comparable.
func textInsideMarks(t *testing.T, rendered string) string {
	t.Helper()
	article, err := ParseArticle("<div>" + rendered + "</div>")
	if err != nil {
		t.Fatalf("re-parse rendered output: %v", err)
	}

	var perBlock []string

	var walk func(node *html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && hasAttr(node, "data-b") {
			if marked := markedText(node); marked != "" {
				perBlock = append(perBlock, marked)
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(article.root)

	return strings.Join(perBlock, "\n\n")
}

// markedText concatenates the text of every <mark> inside one block.
func markedText(block *html.Node) string {
	var collected string
	var walk func(node *html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "mark" {
			collected += textContent(node)
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(block)
	return collected
}

func hasAttr(node *html.Node, name string) bool {
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			return true
		}
	}
	return false
}

// TestRenderMarksTheReadPoint covers the visible read point. SuperMemo shows
// where you stopped on return rather than only scrolling there, and seeing the
// boundary between read and unread is most of its value.
func TestRenderMarksTheReadPoint(t *testing.T) {
	article := mustParse(t, `<p>First.</p><p>Second.</p><p>Third.</p>`)

	got := article.Render(RenderOptions{ReadPoint: 1})

	want := `<p data-b="0">First.</p>` +
		`<p class="read-point" data-b="1">Second.</p>` +
		`<p data-b="2">Third.</p>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestReadPointZeroIsNotMarked keeps an unread article clean: block 0 is where
// everything starts, so marking it would put a "read to here" flag on every
// article that has never been opened.
func TestReadPointZeroIsNotMarked(t *testing.T) {
	article := mustParse(t, `<p>First.</p><p>Second.</p>`)

	for _, readPoint := range []int{0, NoReadPoint} {
		got := article.Render(RenderOptions{ReadPoint: readPoint})
		if strings.Contains(got, "read-point") {
			t.Errorf("ReadPoint %d marked a block: %s", readPoint, got)
		}
	}
}

// TestReadPointCombinesWithMarks checks the two annotations coexist: a block can
// be both where reading stopped and where an extract was taken.
func TestReadPointCombinesWithMarks(t *testing.T) {
	article := mustParse(t, `<p>First.</p><p>Second passage.</p>`)

	got := article.Render(RenderOptions{
		ReadPoint: 1,
		Marks: []Mark{{
			Range:     Range{StartBlock: 1, StartOffset: 0, EndBlock: 1, EndOffset: 6},
			ElementID: 4,
		}},
	})

	if !strings.Contains(got, `class="read-point" data-b="1"`) {
		t.Errorf("read point lost when the block also carries a mark:\n%s", got)
	}
	if !strings.Contains(got, `<mark class="extract" data-element="4">Second</mark>`) {
		t.Errorf("mark lost when the block is also the read point:\n%s", got)
	}
}

// TestReadPointOnAListItemKeepsBothClasses guards the class concatenation: a
// list item already carries a class, and the read point must not replace it.
func TestReadPointOnAListItemKeepsBothClasses(t *testing.T) {
	article := mustParse(t, `<ul><li>One</li><li>Two</li></ul>`)

	got := article.Render(RenderOptions{ReadPoint: 1})

	if !strings.Contains(got, `class="list-item read-point"`) {
		t.Errorf("classes were not combined:\n%s", got)
	}
}
