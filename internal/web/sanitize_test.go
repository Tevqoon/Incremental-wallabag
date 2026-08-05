package web

import (
	"strings"
	"testing"
)

// TestRewriteSamePageLinksShortensAFootnoteHref covers the actual complaint:
// a Substack footnote reference is an absolute link back to the article's
// own address plus a fragment — harmless to sanitise, but useless to click,
// since it navigates away to the original instead of jumping within the
// copy being read here.
func TestRewriteSamePageLinksShortensAFootnoteHref(t *testing.T) {
	source := "https://www.experimental-history.com/p/incentives-are-for-losers"
	html := `<p>schmuck.<a id="footnote-anchor-2" href="` + source + `#footnote-2">2</a></p>`

	got := rewriteSamePageLinks(html, source)

	if !strings.Contains(got, `href="#footnote-2"`) {
		t.Errorf("href was not shortened to a same-page fragment, got:\n%s", got)
	}
	if strings.Contains(got, source+"#") {
		t.Errorf("the original absolute URL should not survive, got:\n%s", got)
	}
}

// TestRewriteSamePageLinksLeavesOtherLinksAlone guards against
// over-matching: a link to a different page — even one sharing the same
// host — must keep pointing where it actually goes.
func TestRewriteSamePageLinksLeavesOtherLinksAlone(t *testing.T) {
	source := "https://www.experimental-history.com/p/incentives-are-for-losers"
	tests := map[string]string{
		"different path, same host": "https://www.experimental-history.com/p/another-post#section",
		"external link":             "https://en.wikipedia.org/wiki/European_wars_of_religion",
		"same page, no fragment":    source,
	}
	for name, href := range tests {
		t.Run(name, func(t *testing.T) {
			html := `<a href="` + href + `">x</a>`
			got := rewriteSamePageLinks(html, source)
			// html.Parse/Render always wraps a fragment in a full document,
			// even when nothing was rewritten — asserting the href survived
			// intact, not byte-for-byte equality with the input.
			if !strings.Contains(got, `href="`+href+`"`) {
				t.Errorf("href was rewritten when it should not have been, want %q intact, got:\n%s", href, got)
			}
		})
	}
}

// TestSanitizeRewritesFootnoteLinks is the integration point, mirroring
// TestSanitizeRewritesTweetEmbeds: sanitize must run the same-page rewrite
// after bluemonday, not before — an unrewritten "#footnote-2" would be
// stripped outright by the policy's AllowRelativeURLs(false), which exists
// precisely to stop a relative link in an article resolving against
// increader's own origin.
func TestSanitizeRewritesFootnoteLinks(t *testing.T) {
	server := &Server{policy: newPolicy()}
	source := "https://www.experimental-history.com/p/incentives-are-for-losers"
	html := `<p>schmuck.<a id="footnote-anchor-2" href="` + source + `#footnote-2">2</a></p>`

	got := server.sanitize(html, source)

	if !strings.Contains(got, `href="#footnote-2"`) {
		t.Errorf("sanitize did not shorten the footnote link, got:\n%s", got)
	}
}

// TestSanitizeWithNoSourceURLLeavesLinksAbsolute covers the no-op path — an
// extract or annotation with nothing to compare against must not have its
// links mangled.
func TestSanitizeWithNoSourceURLLeavesLinksAbsolute(t *testing.T) {
	server := &Server{policy: newPolicy()}
	html := `<p><a href="https://example.com/page#section">link</a></p>`

	got := server.sanitize(html, "")

	if !strings.Contains(got, `href="https://example.com/page#section"`) {
		t.Errorf("link was rewritten with no source URL to compare against, got:\n%s", got)
	}
}
