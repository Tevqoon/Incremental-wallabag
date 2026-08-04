package web

import (
	"strings"
	"testing"
)

// substackTweetEmbed mirrors the real shape Substack renders for a tweet
// quoted into a post — the same nested avatar/name/text/stats structure
// confirmed against a real saved article, trimmed of the styling classes
// bluemonday would strip anyway.
const substackTweetEmbed = `<a href="https://x.com/nabeelqu/status/1" target="_blank" ` +
	`rel="noopener noreferrer" data-component-name="Twitter2ToDOM" class="pencraft pc-reset">` +
	`<div class="pencraft twitter-embed"><div class="row"><div class="avatar"><img src="a.jpg" alt="X avatar for @nabeelqu"/></div>` +
	`<p>Nabeel S. Qureshi@nabeelqu</p></div>` +
	`<p>*Another* apparently AI-generated story wins a literary prize.</p>` +
	`<div class="stats"><p>12:57 PM · Jun 20, 2026 · 869K Views</p><p>141 Replies · 186 Reposts · 1.66K Likes</p></div>` +
	`</div></a>`

// TestRewriteEmbedsProducesAWorkingLink is rewriteEmbeds' core promise: a
// Substack tweet embed — a link wrapping several paragraphs that the
// reader's block model (see ir.ParseArticle) would otherwise flatten into
// several disconnected, unstyled paragraphs with no link left at all —
// becomes one blockquote wrapping one link back to the original tweet.
func TestRewriteEmbedsProducesAWorkingLink(t *testing.T) {
	got := rewriteEmbeds("<p>Before.</p>" + substackTweetEmbed + "<p>After.</p>")

	if !strings.Contains(got, `<blockquote><a href="https://x.com/nabeelqu/status/1">`) {
		t.Errorf("rewriteEmbeds did not produce the expected blockquote+link, got:\n%s", got)
	}
	if !strings.Contains(got, "Nabeel S. Qureshi @nabeelqu") {
		t.Errorf("missing space between name and handle, got:\n%s", got)
	}
	if !strings.Contains(got, `*Another* apparently AI-generated story wins a literary prize.`) {
		t.Errorf("tweet text missing, got:\n%s", got)
	}
	if strings.Contains(got, "869K Views") || strings.Contains(got, "Replies") {
		t.Errorf("stats line should not be collected, got:\n%s", got)
	}
	if !strings.Contains(got, "<p>Before.</p>") || !strings.Contains(got, "<p>After.</p>") {
		t.Errorf("surrounding content should be untouched, got:\n%s", got)
	}
}

// TestRewriteEmbedsIgnoresOtherSubstackWidgets covers the other Substack
// component this same markup pattern appears on (an inline image), which
// already renders fine and must be left alone.
func TestRewriteEmbedsIgnoresOtherSubstackWidgets(t *testing.T) {
	html := `<a href="https://example.com/x" data-component-name="Image2ToDOM">` +
		`<img src="a.jpg" alt="a picture"/></a>`

	got := rewriteEmbeds(html)

	if !strings.Contains(got, `data-component-name="Image2ToDOM"`) {
		t.Errorf("an unrelated Substack widget should not be touched, got:\n%s", got)
	}
	if strings.Contains(got, "<blockquote>") {
		t.Errorf("should not have produced a blockquote for a non-tweet widget, got:\n%s", got)
	}
}

// TestRewriteEmbedsFailsSafely covers the shapes that must not panic or
// produce a broken link: a widget missing its href, and a widget with no
// paragraph text to summarise at all.
func TestRewriteEmbedsFailsSafely(t *testing.T) {
	tests := []struct {
		name string
		html string
	}{
		{"no href", `<a data-component-name="Twitter2ToDOM"><p>Someone</p><p>said something</p></a>`},
		{"no paragraphs", `<a href="https://x.com/a/1" data-component-name="Twitter2ToDOM"><div><img src="a.jpg"/></div></a>`},
		{"not valid html at all", `<a data-component-name="Twitter2ToDOM" href="https://x.com/a/1">`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := rewriteEmbeds(test.html)
			if strings.Contains(got, `href=""`) {
				t.Errorf("produced a link with an empty href, got:\n%s", got)
			}
		})
	}
}

// TestSanitizeRewritesTweetEmbeds is the integration point: sanitize (what
// every article rendering path actually calls) must run rewriteEmbeds
// before bluemonday, not after — bluemonday would otherwise strip
// data-component-name, the only thing telling rewriteEmbeds which <a> to
// rewrite, before it ever gets the chance to look.
func TestSanitizeRewritesTweetEmbeds(t *testing.T) {
	server := &Server{policy: newPolicy()}

	got := server.sanitize(substackTweetEmbed)

	if !strings.Contains(got, `<blockquote><a href="https://x.com/nabeelqu/status/1"`) {
		t.Errorf("sanitize did not rewrite the embed, got:\n%s", got)
	}
}
