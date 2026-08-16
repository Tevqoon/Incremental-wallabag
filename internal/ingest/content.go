package ingest

import (
	"strings"

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
