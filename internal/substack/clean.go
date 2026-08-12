package substack

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// invisibleFormatting and stripInvisibleFormatting duplicate
// internal/web/sanitize.go's and internal/wallabag/ranges.go's own copies of
// the same thing, on purpose — both of those files explain why, and the
// reasoning carries over unchanged: this package stays a leaf, importing
// nothing beyond the standard library, x/net/html, and internal/source (see
// the package doc comment in substack.go), and depending on either of those
// two packages just to reach one small character-stripping helper would
// trade that leaf status away for a dependency this package does not
// otherwise need anywhere else.
//
// A soft hyphen in particular turns up in real press-typeset HTML —
// wallabag's own copy of this comment notes a Financial Times article that
// had one, confirmed rather than assumed. Left in, these invisible
// characters surface as stray glyphs wherever a stored quote is later
// exported as plain text, and — the sharper problem for increader
// specifically — they silently break quote-based highlight anchoring, by
// shifting where a quote appears to sit in the text without changing
// anything a reader can see.
//
// Spelled out as \u escapes rather than typed literally, following the same
// rule internal/wallabag/ranges.go's copy states explicitly: these
// characters are invisible by definition, so a literal in source would be
// exactly as unverifiable by eye as the bug this exists to fix.
var invisibleFormatting = map[rune]bool{
	'\u00ad': true, // soft hyphen
	'\u200b': true, // zero-width space
	'\u200c': true, // zero-width non-joiner
	'\u200d': true, // zero-width joiner
	'\u2060': true, // word joiner
	'\ufeff': true, // byte-order mark / zero-width no-break space
}

// stripInvisibleFormatting removes invisibleFormatting's characters from a
// string. Safe to run over any text content: none of these characters have
// any legitimate visible role.
func stripInvisibleFormatting(s string) string {
	return strings.Map(func(r rune) rune {
		if invisibleFormatting[r] {
			return -1
		}
		return r
	}, s)
}

// subscribeComponentName is the data-component-name value Substack's own
// subscribe-widget carries — confirmed against a real, live, unauthenticated
// fetch of a free post's body_html on 2026-08-12. Matched the same way
// internal/web/embeds.go's rewriteEmbeds matches its own
// data-component-name="Twitter2ToDOM" tweet embeds, and for the same
// reason: a data attribute naming the specific widget is structural, not
// prose, so unlike a generic class or a string search it cannot
// false-positive on article text that merely mentions subscribing.
//
// This is the primary hook isSubscribeChrome uses. The earlier version of
// this file keyed removal on CSS classes guessed from public knowledge of
// Substack's markup rather than a real response — "paywall",
// "subscribe-widget" — and a live check disproved the guess outright: the
// class actually present is "subscription-widget" (see
// subscribeWidgetClassPrefix below), and no post body, free or paywalled,
// was found to carry any element with a class literally called "paywall" at
// all. See verifySession's own doc comment in session.go for the fuller
// finding this same live check produced about paywall detection in general.
const subscribeComponentName = "SubscribeWidgetToDOM"

// subscribeWidgetClassPrefix and subscribeWidgetExactClass are the fallback
// isSubscribeChrome uses for markup that predates subscribeComponentName,
// or for a wrapping element the attribute simply is not set on.
//
// Confirmed against the same live fetch as subscribeComponentName: a free,
// complete post's own body_html carried "subscription-widget",
// "subscription-widget-subscribe", and "subscription-widget-wrap-editor" on
// the elements composing its trailing subscribe prompt, alongside
// "show-subscribe". Matched by prefix on "subscription-widget" rather than
// each exact spelling seen, since there is no reason to assume every
// suffix variant Substack's markup uses has actually been observed — a
// differently-suffixed sibling class turning up on some other
// publication's post should still be recognised as part of the same widget
// family rather than silently surviving into ContentHTML.
const (
	subscribeWidgetClassPrefix = "subscription-widget"
	subscribeWidgetExactClass  = "show-subscribe"
)

// isSubscribeChrome reports whether n is (the root of) Substack's own
// subscribe-widget, by subscribeComponentName first and the
// subscribeWidgetClassPrefix/subscribeWidgetExactClass fallback second.
func isSubscribeChrome(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	for _, attr := range n.Attr {
		switch attr.Key {
		case "data-component-name":
			if attr.Val == subscribeComponentName {
				return true
			}
		case "class":
			for _, class := range strings.Fields(attr.Val) {
				if class == subscribeWidgetExactClass || strings.HasPrefix(class, subscribeWidgetClassPrefix) {
					return true
				}
			}
		}
	}
	return false
}

// cleanBody strips Substack's own subscribe-widget chrome out of a post
// body and strips invisibleFormatting's characters from what remains. It
// parses the markup with x/net/html and removes whole nodes rather than
// regexing the HTML text: a regex has no notion of "this closing tag
// belongs to that opening tag", so it either under-matches nested markup or
// over-matches into content that merely happens to contain similar text
// nearby.
//
// This deliberately does not also hunt for a "paywall block" or a
// share/comment button row the way an earlier version of this file did.
// Neither turned out to be real: a live check (see verifySession's doc
// comment in post.go) found that a genuine paywalled preview carries no
// paywall marker of any kind — nothing to strip even if this looked for
// it — and Ingest's session-verification guard means this package now never
// imports a preview in the first place, so there is nothing left needing a
// paywall-block removal to protect against. No share or comment button
// markup was found in body_html either; increader's reader chrome, not
// Substack's API response, is apparently where those live. If a real
// example of either ever turns up, extend isSubscribeChrome's approach
// rather than reintroducing a guessed class name.
//
// Three things this deliberately does NOT do, each of which looks like an
// omission on a first read and is not:
//
//  1. It does not sanitise. internal/web/sanitize.go owns that policy for
//     increader's own reader, and that policy is wrong here: it stamps
//     rel="nofollow noreferrer" target="_blank" onto every link and forbids
//     relative URLs outright, both of which are the read path's own
//     choices about how increader's reader should behave, not properties
//     of the content itself. Another wallabag client reading this same
//     imported entry later should see links exactly as Substack wrote
//     them, not as increader's reader would prefer to render them.
//  2. It does not rewrite image URLs. increader's own image proxy
//     (internal/web/images.go) keys a rewritten URL on a local document id
//     and a cached blob id, and neither exists yet at the point this runs —
//     this package only ever produces a source.Document in memory; it does
//     not store one anywhere. Rewriting an image URL here would produce a
//     URL that resolves to nothing until some later step gave it real ids
//     to key on, so this leaves Substack's own CDN URLs exactly as it found
//     them.
//  3. It does not touch tweet embeds. internal/web/embeds.go already
//     rewrites Substack's own Twitter2ToDOM widgets at read time, into
//     something increader's block model can render. Duplicating that logic
//     here would just mean two different pieces of increader disagreeing
//     about what a tweet embed should become; a tweet embed is left exactly
//     as fetched, and rewriteEmbeds handles it later, once, in one place.
func cleanBody(bodyHTML string) (string, []string) {
	if strings.TrimSpace(bodyHTML) == "" {
		return "", nil
	}

	// ParseFragment, not Parse: bodyHTML is an article body fragment — a
	// sequence of <p>, <div>, <figure> and similar siblings — not a
	// complete document. html.Parse always produces a full document,
	// wrapping whatever it is given in an implied
	// <html><head></head><body>...</body></html> even when the input never
	// had one. Some other code in this repo tolerates that (rewriteEmbeds in
	// internal/web/embeds.go, for one) because it either never re-serializes
	// the result or relies on a downstream bluemonday policy to strip the
	// wrapper back out. cleanBody has neither: its result is exactly what
	// becomes source.Document.ContentHTML, with no sanitiser downstream —
	// see point 1 above — so parsing as a fragment against a <body> context
	// is what keeps the output the same shape as the input, rather than
	// silently wrapping a clean article body in a spurious document shell.
	context := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := html.ParseFragment(strings.NewReader(bodyHTML), context)
	if err != nil {
		return bodyHTML, []string{fmt.Sprintf("substack: could not parse post body for cleanup, left uncleaned: %v", err)}
	}

	// A synthetic wrapper to hang the parsed top-level siblings off of:
	// stripChoreNodes removes a matched node via its parent's RemoveChild,
	// which needs every node it might remove — including one sitting at the
	// very top of the fragment, with no parent of its own from
	// ParseFragment — to actually have one.
	root := &html.Node{Type: html.ElementNode, Data: "div"}
	for _, n := range nodes {
		root.AppendChild(n)
	}

	stripChoreNodes(root)
	stripInvisibleText(root)

	var out strings.Builder
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&out, c); err != nil {
			return bodyHTML, []string{fmt.Sprintf("substack: could not render cleaned post body, left uncleaned: %v", err)}
		}
	}
	return out.String(), nil
}

// stripChoreNodes removes every descendant of n that isSubscribeChrome
// matches, in place, along with everything inside it.
//
// A tweet embed survives this untouched even though it, too, is identified
// by a data-component-name attribute: tweetEmbedReplacement in
// internal/web/embeds.go matches "Twitter2ToDOM" specifically, a different
// value from subscribeComponentName's "SubscribeWidgetToDOM", so the two
// never collide — see cleanBody's point 3 above. Likewise Image2ToDOM and
// PreformattedTextBlockToDOM (Substack's own image and code-block
// components, both confirmed present in real body_html) and the
// captioned-image-container / image2 / image2-inset / restack-image /
// is-viewable-img class family that goes with them are never touched: none
// of them is "SubscribeWidgetToDOM", and none carries a class matching
// subscribeWidgetClassPrefix or subscribeWidgetExactClass.
func stripChoreNodes(n *html.Node) {
	for child := n.FirstChild; child != nil; {
		next := child.NextSibling
		if isSubscribeChrome(child) {
			n.RemoveChild(child)
		} else {
			stripChoreNodes(child)
		}
		child = next
	}
}

// stripInvisibleText applies stripInvisibleFormatting to every text node
// under n, in place. Safe to run over the whole tree rather than confining
// it to some subset of text nodes: none of invisibleFormatting's characters
// have any legitimate role in an element's markup, only inside text
// content, so nothing structural can be affected.
func stripInvisibleText(n *html.Node) {
	if n.Type == html.TextNode {
		n.Data = stripInvisibleFormatting(n.Data)
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		stripInvisibleText(child)
	}
}
