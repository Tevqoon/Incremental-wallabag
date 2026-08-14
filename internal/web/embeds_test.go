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
	got := rewriteEmbeds("<p>Before.</p>"+substackTweetEmbed+"<p>After.</p>", "")

	if !strings.Contains(got, `<blockquote><a href="https://xcancel.com/nabeelqu/status/1">`) {
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

	got := rewriteEmbeds(html, "")

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
			got := rewriteEmbeds(test.html, "")
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

	got := server.sanitize(substackTweetEmbed, "")

	if !strings.Contains(got, `<blockquote><a href="https://xcancel.com/nabeelqu/status/1"`) {
		t.Errorf("sanitize did not rewrite the embed, got:\n%s", got)
	}
}

// danluuChartEmbed is one of the interactive charts in danluu.com/pl-tokens,
// copied verbatim from the published page. It is the case this rewrite
// exists for, and its shape is the whole problem: a relative src, a title
// that is the only readable thing about it, and no fallback content of any
// kind — once the frame goes, nothing records that a chart was there.
const danluuChartEmbed = `<iframe class="interactive-chart" ` +
	`src="/interactive/pl-tokens/v7-average-x-vs-correctness.html" ` +
	`title="Zstd cost or time versus correctness by language and effort" loading="lazy"></iframe>`

// TestRewriteEmbedsLinksAFrameToItsSource is the frame half of rewriteEmbeds'
// promise: a chart embedded as a bare iframe, which newPolicy strips without
// trace, becomes a blockquote linking to the chart at its original address.
func TestRewriteEmbedsLinksAFrameToItsSource(t *testing.T) {
	got := rewriteEmbeds("<p>Before.</p>"+danluuChartEmbed+"<p>After.</p>", "https://danluu.com/pl-tokens/")

	want := `<blockquote><a href="https://danluu.com/interactive/pl-tokens/v7-average-x-vs-correctness.html">` +
		`Embedded content: Zstd cost or time versus correctness by language and effort</a></blockquote>`
	if !strings.Contains(got, want) {
		t.Errorf("rewriteEmbeds did not link the frame to its source, got:\n%s", got)
	}
	if !strings.Contains(got, "<p>Before.</p>") || !strings.Contains(got, "<p>After.</p>") {
		t.Errorf("surrounding content should be untouched, got:\n%s", got)
	}
}

// TestRewriteEmbedsLabelsAnUntitledFrame covers the frame with nothing to
// describe it: the link still has to say that something was embedded here,
// because that fact is the part the reader cannot otherwise recover.
func TestRewriteEmbedsLabelsAnUntitledFrame(t *testing.T) {
	got := rewriteEmbeds(`<iframe src="https://example.com/widget"></iframe>`, "https://example.com/post")

	if !strings.Contains(got, `<blockquote><a href="https://example.com/widget">Embedded content</a></blockquote>`) {
		t.Errorf("an untitled frame should still become a labelled link, got:\n%s", got)
	}
	if strings.Contains(got, "Embedded content:") {
		t.Errorf("no title means no trailing colon, got:\n%s", got)
	}
}

// TestRewriteEmbedsLeavesUnfollowableFrames covers every src that must not
// become a link. A frame the reader cannot follow is worse as an <a> than as
// the deletion it already is — and a relative src with no article URL to
// resolve it against is exactly as unfollowable as a javascript: one.
func TestRewriteEmbedsLeavesUnfollowableFrames(t *testing.T) {
	tests := []struct {
		name, html, sourceURL string
	}{
		{"no src", `<iframe title="a chart"></iframe>`, "https://danluu.com/pl-tokens/"},
		{"about:blank", `<iframe src="about:blank"></iframe>`, "https://danluu.com/pl-tokens/"},
		{"javascript", `<iframe src="javascript:alert(1)"></iframe>`, "https://danluu.com/pl-tokens/"},
		{"data document", `<iframe src="data:text/html,<p>hi</p>"></iframe>`, "https://danluu.com/pl-tokens/"},
		{"relative src, no article URL", danluuChartEmbed, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := rewriteEmbeds(test.html, test.sourceURL)
			if strings.Contains(got, "<blockquote>") {
				t.Errorf("should not have produced a link, got:\n%s", got)
			}
		})
	}
}

// TestSanitizeLinksChartFrames is the integration point, and the reason the
// rewrite has to run before bluemonday rather than after: by the time the
// policy is done there is no iframe left to find, so a pass ordered after it
// would have nothing at all to work with.
func TestSanitizeLinksChartFrames(t *testing.T) {
	server := &Server{policy: newPolicy()}

	got := server.sanitize(danluuChartEmbed, "https://danluu.com/pl-tokens/")

	if !strings.Contains(got, `href="https://danluu.com/interactive/pl-tokens/v7-average-x-vs-correctness.html"`) {
		t.Errorf("sanitize did not preserve the chart link, got:\n%s", got)
	}
	if !strings.Contains(got, "Zstd cost or time versus correctness") {
		t.Errorf("sanitize dropped the frame's title, got:\n%s", got)
	}
	if strings.Contains(got, "<iframe") {
		t.Errorf("the frame itself must not survive the policy, got:\n%s", got)
	}
}

// TestRedirectToXcancel covers x.com and the older twitter.com domain, both
// with and without a www prefix, and leaves an unrelated URL alone.
func TestRedirectToXcancel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://x.com/nabeelqu/status/1", "https://xcancel.com/nabeelqu/status/1"},
		{"https://www.x.com/nabeelqu/status/1", "https://xcancel.com/nabeelqu/status/1"},
		{"https://twitter.com/nabeelqu/status/1", "https://xcancel.com/nabeelqu/status/1"},
		{"https://www.twitter.com/nabeelqu/status/1", "https://xcancel.com/nabeelqu/status/1"},
		{"https://example.com/nabeelqu/status/1", "https://example.com/nabeelqu/status/1"},
		{"not a url at all", "not a url at all"},
	}
	for _, test := range tests {
		if got := redirectToXcancel(test.in); got != test.want {
			t.Errorf("redirectToXcancel(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}
