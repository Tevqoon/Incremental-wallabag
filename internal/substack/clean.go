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

// choreClasses are the CSS class names cleanBody removes wholesale: an
// element carrying one of these, and everything inside it, is Substack's
// own UI chrome rather than article content — a subscribe prompt, a
// share/comment button row, or the paywall block itself. Overlaps
// paywallClasses in post.go on purpose: cleanBody strips the very paywall
// block hasPaywallMarker detects, for the (rare, since Ingest generally does
// not import a paywalled post at all — see isPaywalled) case where a mixed
// body slips through with genuine content alongside a trailing paywall
// stub.
//
// Sourced from publicly available Substack post markup rather than
// confirmed empirically — see paywallClasses' own comment in post.go, which
// needs the paywall subset of these same names and explains the same
// caveat once: this package was built with no network access to a live
// Substack account, so nothing here was checked against a genuine response.
var choreClasses = map[string]bool{
	"paywall":             true,
	"subscribe-widget":    true,
	"subscription-widget": true,
	"share-dialog":        true,
	"post-ufi":            true, // the like / comment / share row under a post
}

// cleanBody strips Substack's own UI chrome out of a post body — subscribe
// widgets, share/comment button rows, footer CTAs, and the paywall block
// itself — and strips invisibleFormatting's characters from what remains.
// It parses the markup with x/net/html and removes whole nodes rather than
// regexing the HTML text: a regex has no notion of "this closing tag
// belongs to that opening tag", so it either under-matches nested markup or
// over-matches into content that merely happens to contain similar text
// nearby.
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
	// had one. Some other code in this repo tolerates that (see
	// hasPaywallMarker in post.go, and rewriteEmbeds in
	// internal/web/embeds.go) because it either never re-serializes the
	// result or relies on a downstream bluemonday policy to strip the
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

// stripChoreNodes removes every descendant of n carrying one of
// choreClasses, in place, along with everything inside it.
//
// A tweet embed survives this untouched even though it, too, is a widget:
// tweetEmbedReplacement in internal/web/embeds.go identifies one by its
// data-component-name="Twitter2ToDOM" attribute, not by any CSS class, so
// nothing in choreClasses ever matches it — see cleanBody's point 3 above.
func stripChoreNodes(n *html.Node) {
	for child := n.FirstChild; child != nil; {
		next := child.NextSibling
		if child.Type == html.ElementNode && nodeHasClass(child, choreClasses) {
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
