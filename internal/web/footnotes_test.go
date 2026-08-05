package web

import (
	"strings"
	"testing"

	"github.com/Tevqoon/increader/internal/ir"
)

// substackFootnoteEntry mirrors the real shape confirmed against a saved
// article (experimental-history.com/p/incentives-are-for-losers): a bare
// number link sitting beside a sibling div holding the actual text, both
// wrapped in a div carrying Substack's own component marker.
const substackFootnoteEntry = `<div data-component-name="FootnoteToDOM" class="footnote">` +
	`<a id="footnote-2" href="https://www.example.com/p/some-post#footnote-anchor-2" ` +
	`contenteditable="false" target="_self" class="footnote-number">2</a>` +
	`<div class="footnote-content"><p>A few kids, the serious ones, brought their own helmets.</p></div>` +
	`</div>`

// TestRewriteFootnotesMovesTheNumberIntoTheParagraph is rewriteFootnotes'
// core promise: the number, which the reader's block model (see
// ir.ParseArticle) would otherwise drop entirely — a bare <a> beside a div
// is not itself a block, and is not inside the one that becomes one — ends
// up as real text inside the paragraph that survives.
func TestRewriteFootnotesMovesTheNumberIntoTheParagraph(t *testing.T) {
	got := rewriteFootnotes("<p>Before.</p>" + substackFootnoteEntry + "<p>After.</p>")

	if !strings.Contains(got, `<p><a id="footnote-2" href="https://www.example.com/p/some-post#footnote-anchor-2" class="footnote-number">2. </a>A few kids`) {
		t.Errorf("number was not folded into the footnote's own paragraph, got:\n%s", got)
	}
	if !strings.Contains(got, "<p>Before.</p>") || !strings.Contains(got, "<p>After.</p>") {
		t.Errorf("surrounding content should be untouched, got:\n%s", got)
	}
	if strings.Contains(got, `data-component-name="FootnoteToDOM"`) {
		t.Errorf("the widget wrapper should not survive, got:\n%s", got)
	}
}

// TestParsedArticleKeepsTheFootnoteNumber is the regression this whole fix
// is for: without rewriteFootnotes, ir.ParseArticle's block model drops the
// number outright, not just loses its styling — confirmed by asserting the
// number reaches a real Block, not just that rewriteFootnotes' own HTML
// output looks right.
func TestParsedArticleKeepsTheFootnoteNumber(t *testing.T) {
	server := &Server{policy: newPolicy()}
	sanitized := server.sanitize(substackFootnoteEntry, "")

	article, err := ir.ParseArticle(sanitized)
	if err != nil {
		t.Fatalf("ParseArticle: %v", err)
	}

	found := false
	for _, block := range article.Blocks() {
		if strings.HasPrefix(block.Text, "2. A few kids") {
			found = true
		}
	}
	if !found {
		t.Errorf("no block starts with the footnote number, blocks: %+v", article.Blocks())
	}
}

// TestRewriteFootnotesIgnoresOrdinaryDivs guards against over-matching: a
// div that merely happens to contain a link beside another div — not
// Substack's own footnote widget — must be left alone.
func TestRewriteFootnotesIgnoresOrdinaryDivs(t *testing.T) {
	html := `<div><a href="https://example.com">a link</a><div>some text</div></div>`

	got := rewriteFootnotes(html)

	if !strings.Contains(got, `<a href="https://example.com">a link</a>`) {
		t.Errorf("an unrelated div should not be touched, got:\n%s", got)
	}
}
