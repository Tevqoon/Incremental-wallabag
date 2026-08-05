package web

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// footnoteComponent is the data-component-name value Substack's own
// footnote-list widget carries on its outer <div> — see rewriteFootnotes.
const footnoteComponent = "FootnoteToDOM"

// rewriteFootnotes finds Substack's own footnote-list entries in rawHTML and
// folds each one's number into the start of its own text, as one paragraph.
//
// The widget's real shape is a bare number link sitting beside a sibling div
// that holds the actual footnote text:
//
//	<div data-component-name="FootnoteToDOM" class="footnote">
//	  <a id="footnote-5" href="...#footnote-anchor-5" class="footnote-number">5</a>
//	  <div class="footnote-content"><p>It took place in February...</p></div>
//	</div>
//
// That number is never inline text of any paragraph — it is a link sitting
// directly inside a div, next to another div, with no <p> of its own. The
// reader's block model (see ir.ParseArticle) only emits a block for the
// deepest recognised element in a chain, and a bare <a> is not one of the
// recognised elements at all: the number is not merged into a neighbouring
// block, it is dropped from the article outright, the same failure mode
// rewriteEmbeds exists to fix for tweet embeds. No sanitiser policy change
// can help either — the number never reaches rendering in the first place.
//
// The fix is the same shape as rewriteEmbeds': normalise the widget into
// markup the block model already handles, before sanitising ever sees it —
// here, by moving the number to be the first inline content of the
// footnote's own paragraph, which is a block ir.ParseArticle already keeps.
func rewriteFootnotes(rawHTML string) string {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return rawHTML
	}

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		for child := node.FirstChild; child != nil; {
			next := child.NextSibling
			if replacement := footnoteReplacement(child); replacement != nil {
				node.InsertBefore(replacement, child)
				node.RemoveChild(child)
			} else {
				walk(child)
			}
			child = next
		}
	}
	walk(doc)

	var out strings.Builder
	if err := html.Render(&out, doc); err != nil {
		return rawHTML
	}
	return out.String()
}

// footnoteReplacement builds the paragraph to replace node with, or nil if
// node is not one of Substack's footnote-list entries.
func footnoteReplacement(node *html.Node) *html.Node {
	if node.Type != html.ElementNode || node.DataAtom != atom.Div ||
		attrValue(node, "data-component-name") != footnoteComponent {
		return nil
	}

	var number, content *html.Node
	for c := node.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		switch {
		case c.DataAtom == atom.A && number == nil:
			number = c
		case c.DataAtom == atom.Div && content == nil:
			content = c
		}
	}
	if number == nil || content == nil {
		return nil
	}

	var firstParagraph *html.Node
	for c := content.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.DataAtom == atom.P {
			firstParagraph = c
			break
		}
	}
	if firstParagraph == nil {
		return nil
	}

	// Kept: id and href, so the number is still both a valid jump target for
	// the inline reference (matching id) and its own working link back up to
	// it (href) — see rewriteSamePageLinks, which turns that href from an
	// absolute link at the article's own URL into one that actually works
	// once the article is read here rather than at its original address.
	// Dropped: everything styling-only (class, contenteditable, target),
	// which bluemonday's policy would strip anyway.
	marker := &html.Node{Type: html.ElementNode, Data: "a", DataAtom: atom.A}
	for _, key := range []string{"id", "href"} {
		if value := attrValue(number, key); value != "" {
			marker.Attr = append(marker.Attr, html.Attribute{Key: key, Val: value})
		}
	}
	marker.Attr = append(marker.Attr, html.Attribute{Key: "class", Val: "footnote-number"})
	marker.AppendChild(&html.Node{Type: html.TextNode, Data: textOf(number) + ". "})
	firstParagraph.InsertBefore(marker, firstParagraph.FirstChild)

	// content's paragraphs move up to replace the whole widget; a plain div
	// wrapper carries them across the one-node-for-one-node swap the walker
	// above does. It is never itself a block — ir.ParseArticle's leaf rule
	// skips a div whose children already produced one — so this is purely
	// structural, not a paragraph gaining an extra invisible layer.
	wrapper := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	for c := content.FirstChild; c != nil; {
		next := c.NextSibling
		content.RemoveChild(c)
		wrapper.AppendChild(c)
		c = next
	}
	return wrapper
}
