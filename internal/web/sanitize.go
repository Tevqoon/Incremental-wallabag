package web

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// invisibleFormatting is the set of Unicode characters that carry no
// meaning as plain text but frequently ride along in scraped press HTML:
// soft hyphens marking a typesetter's optional line-break point, and a
// handful of zero-width joiners and marks. Confirmed on a real article
// (soft hyphens throughout an FT piece, littering every export of the
// highlight's text) rather than assumed from a general character list.
//
// Spelled out as \u escapes rather than typed literally: these characters
// are invisible by definition, so a literal in source would be exactly as
// unverifiable by eye as the bug this exists to fix.
var invisibleFormatting = map[rune]bool{
	'\u00ad': true, // soft hyphen
	'\u200b': true, // zero-width space
	'\u200c': true, // zero-width non-joiner
	'\u200d': true, // zero-width joiner
	'\u2060': true, // word joiner
	'\ufeff': true, // byte-order mark / zero-width no-break space
}

// stripInvisibleFormatting removes invisibleFormatting's characters from
// HTML text. Safe to run over the whole markup string, not just text nodes:
// none of these characters have any legitimate role in tag or attribute
// syntax, so nothing structural can depend on one surviving.
func stripInvisibleFormatting(s string) string {
	return strings.Map(func(r rune) rune {
		if invisibleFormatting[r] {
			return -1
		}
		return r
	}, s)
}

// sanitize runs the bluemonday policy and then strips invisibleFormatting.
//
// This must be the only path from raw article HTML to anything ir.ParseArticle
// or Locate ever sees: block text, offsets and rendered markup are all derived
// from whatever string comes out of here, so stripping later — say, from an
// already-parsed Block.Text — would shorten it without shortening the DOM
// node backing it, and silently misalign every offset downstream.
//
// sourceURL is the article's own URL — empty for anything that is not a
// whole article read at some address (a book annotation, an extract read
// back through here for offset purposes only). See rewriteSamePageLinks for
// what it is used for; passed through even when empty, since that rewrite is
// itself a safe no-op with nothing to compare against.
func (s *Server) sanitize(rawHTML, sourceURL string) string {
	preprocessed := rewriteFootnotes(rewriteEmbeds(rawHTML))
	sanitized := s.policy.Sanitize(preprocessed)
	return stripInvisibleFormatting(rewriteSamePageLinks(sanitized, sourceURL))
}

// rewriteSamePageLinks turns a link that only points back into the article's
// own page — the common shape of a Substack footnote, whose reference and
// back-link both carry the *article's own URL* plus a fragment — into a bare
// fragment link, so clicking it jumps within the reader instead of
// navigating out to the original.
//
// Deliberately a pass over already-sanitised HTML, not a rewrite folded into
// the pre-sanitise step rewriteFootnotes already does. bluemonday's policy
// disallows relative URLs outright (see newPolicy) precisely so a relative
// link in an article cannot resolve against increader's own origin — an
// href written as a bare "#fragment" before sanitising would be stripped by
// that same rule, not preserved by it. Running after lets the fragment
// survive: bluemonday has already validated the link's full absolute form
// as a legitimate http(s) URL, and shortening an already-approved URL down
// to its fragment introduces nothing a browser could resolve anywhere but
// the current page.
func rewriteSamePageLinks(sanitizedHTML, sourceURL string) string {
	if sourceURL == "" {
		return sanitizedHTML
	}
	source, err := url.Parse(sourceURL)
	if err != nil {
		return sanitizedHTML
	}

	doc, err := html.Parse(strings.NewReader(sanitizedHTML))
	if err != nil {
		return sanitizedHTML
	}

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.DataAtom == atom.A {
			rewriteHrefIfSamePage(node, source)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	var out strings.Builder
	if err := html.Render(&out, doc); err != nil {
		return sanitizedHTML
	}
	return out.String()
}

// rewriteHrefIfSamePage shortens node's href to a bare fragment when it
// points at the same page as source and carries one — matched on scheme,
// host and path, ignoring query and fragment, which is what "the same page"
// means for a link that only differs by which anchor it jumps to.
func rewriteHrefIfSamePage(node *html.Node, source *url.URL) {
	for i, attribute := range node.Attr {
		if attribute.Key != "href" {
			continue
		}
		target, err := url.Parse(attribute.Val)
		if err != nil || target.Fragment == "" {
			return
		}
		if target.Scheme == source.Scheme && target.Host == source.Host && target.Path == source.Path {
			node.Attr[i].Val = "#" + target.Fragment
		}
		return
	}
}

// newPolicy builds the sanitiser applied to every article body before it is
// parsed or rendered.
//
// Article HTML is arbitrary content from the open web. Even though increader is
// only reachable over Tailscale, a malicious article rendered in the reader
// would run with full access to increader's own origin — it could create
// extracts, dismiss material, or read the whole library. Sanitising is not
// optional because the deployment is private.
//
// The policy is also load-bearing for correctness, not just safety: block
// indices and character offsets are computed against the *sanitised* HTML, so
// this function defines the coordinate system that the browser and the server
// agree on. It must therefore be deterministic, which bluemonday is.
func newPolicy() *bluemonday.Policy {
	policy := bluemonday.UGCPolicy()

	// Structural elements a readable article needs and UGCPolicy omits.
	policy.AllowElements("figure", "figcaption", "picture", "section", "article")

	// store.annotationHTML marks a reader's own note on an imported passage
	// with this class, so the note can be told apart from the passage when
	// rendered. UGCPolicy drops class attributes wholesale and would take
	// that one with it.
	//
	// Matched against the exact literal rather than a pattern, and on the one
	// element that ever carries it: the policy also runs over article HTML
	// from the open web, and an article able to give itself any class it
	// liked could dress a paragraph up as part of the interface.
	policy.AllowAttrs("class").
		Matching(regexp.MustCompile(`^annotation-note$`)).
		OnElements("p")

	// rewriteFootnotes marks the number it moves to the front of a footnote's
	// own paragraph with this class, so it can be told apart from the
	// footnote's text — same reasoning and same narrow scoping as
	// annotation-note above.
	policy.AllowAttrs("class").
		Matching(regexp.MustCompile(`^footnote-number$`)).
		OnElements("a")

	// Images are worth keeping — many articles are unreadable without their
	// diagrams. The cost is that loading one tells its host you are reading
	// this article, which for a private reader is a modest but real leak.
	policy.AllowImages()
	policy.AllowAttrs("alt", "title").OnElements("img")

	// Links open elsewhere and must not hand the opener a window reference.
	policy.RequireNoFollowOnLinks(true)
	policy.RequireNoReferrerOnLinks(true)
	policy.AddTargetBlankToFullyQualifiedLinks(true)

	// Only absolute web URLs survive. A relative link in an article would
	// otherwise resolve against increader's own origin and point at the app.
	policy.AllowURLSchemes("http", "https", "mailto")
	policy.RequireParseableURLs(true)
	policy.AllowRelativeURLs(false)

	return policy
}
