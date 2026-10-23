package ingest

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// forWallabag adapts a post's content to what wallabag's create/update
// endpoints actually preserve, rather than what they merely accept.
//
// Confirmed against a real import, not assumed: a live Substack post whose
// body_html contained a genuine mid-article <h1> — Substack uses h1 for a
// section heading well past the first paragraph, not only for the page's
// own title — went into wallabag as sent and came back with that heading
// silently gone, both from what wallabag itself renders and from what
// increader later synced back down. Everything else in the same body
// survived untouched, and the entry's own title (a separate field,
// unaffected) was unaffected too.
//
// The most likely explanation is a sanitiser on wallabag's own storage path
// that treats <h1> as reserved for the page's own title — a common
// convention, one h1 per page — and strips any it finds inside submitted
// body content; h2 through h6 are ordinary section headings by that same
// convention and were not observed to have the same problem. Demoting is
// the fix rather than avoiding the tag outright: it costs nothing (equal
// byte length either way — "h1" and "h2" are the same length, so this
// cannot itself perturb planOne's own byte-count comparisons) and the
// heading still renders as a heading, just not the one wallabag apparently
// refuses to keep.
//
// Applied once, here, rather than by each producer feeding into this
// package (see the package doc comment on why there is one sink and,
// eventually, several producers): this is a property of what wallabag will
// actually store, not of Substack specifically, so a future producer gets
// the same accommodation for free rather than having to know about it.
func forWallabag(contentHTML string) string {
	if strings.TrimSpace(contentHTML) == "" {
		return contentHTML
	}

	// ParseFragment against a <body> context, not html.Parse — contentHTML
	// is an article body fragment, not a full document; see cleanBody's own
	// doc comment in internal/substack/clean.go for the fuller reasoning,
	// which applies unchanged here.
	context := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := html.ParseFragment(strings.NewReader(contentHTML), context)
	if err != nil {
		// Malformed enough that parsing it here is not safe; send it exactly
		// as given rather than risk mangling it further attempting to fix a
		// heading level in something this cannot even parse.
		return contentHTML
	}

	root := &html.Node{Type: html.ElementNode, Data: "div"}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	demoteH1(root)

	var out strings.Builder
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&out, c); err != nil {
			return contentHTML
		}
	}
	return out.String()
}

// demoteH1 rewrites every <h1> under n to <h2>, in place, recursively —
// attributes (there are normally none on a Substack heading, but this
// leaves whatever is there untouched either way) and content survive
// unchanged; only the tag name changes.
func demoteH1(n *html.Node) {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.DataAtom == atom.H1 {
			child.Data = "h2"
			child.DataAtom = atom.H2
		}
		demoteH1(child)
	}
}

// sameVisibleText reports whether a and b, two HTML bodies, read the same:
// equal text once tags are dropped, entities decoded, and whitespace and
// invisible formatting characters removed. Markup-only differences —
// attributes, entity spelling, element nesting, whitespace between blocks
// (wallabag adds a newline between every pair) — never make two bodies
// differ by this test; added, removed or edited words always do.
//
// Whitespace is dropped outright rather than collapsed: whether two blocks
// or an inline element's edges are separated by whitespace in the markup
// differs between serialisers without changing what a reader sees, and a
// change that only moves whitespace is not one worth rewriting an entry
// for.
func sameVisibleText(a, b string) bool {
	return visibleText(a) == visibleText(b)
}

// invisibleFormatting mirrors internal/substack's own copy (unexported
// there, as here): characters that change nothing a reader sees, which
// cleanBody strips from what it sends but an entry stored by some other
// route may still carry.
var invisibleFormatting = map[rune]bool{
	'\u00ad': true, // soft hyphen
	'\u200b': true, // zero-width space
	'\u200c': true, // zero-width non-joiner
	'\u200d': true, // zero-width joiner
	'\u2060': true, // word joiner
	'\ufeff': true, // byte-order mark / zero-width no-break space
}

// visibleText returns contentHTML's text content with whitespace and
// invisible formatting removed. Text inside <script>, <style> and
// <template> is skipped, since none of it is shown to a reader. A body that
// cannot be parsed falls back to itself, so it only ever compares equal to
// an identical body.
func visibleText(contentHTML string) string {
	context := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := html.ParseFragment(strings.NewReader(contentHTML), context)
	if err != nil {
		return contentHTML
	}
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			text.WriteString(strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) || invisibleFormatting[r] {
					return -1
				}
				return r
			}, n.Data))
		case n.Type == html.ElementNode && (n.DataAtom == atom.Script || n.DataAtom == atom.Style || n.DataAtom == atom.Template):
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return text.String()
}
