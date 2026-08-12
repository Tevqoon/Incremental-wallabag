package ir

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTML returns the article HTML a range covers, preserving inline markup.
//
// The alternative — keeping only plain text — would strip links out of every
// extract, and a link is often the most valuable thing in a quoted passage.
// So the range is clipped against the actual node tree: text nodes are cut at
// the offsets, and inline elements that survive the cut are rebuilt around
// whatever is left of their contents.
//
// Each block becomes its own element in the result, so a selection spanning
// three paragraphs produces three paragraphs.
func (a *Article) HTML(r Range) (string, error) {
	if err := a.Valid(r); err != nil {
		return "", err
	}

	var out strings.Builder
	for index := r.StartBlock; index <= r.EndBlock; index++ {
		block := a.blocks[index]

		from, to := 0, len(block.Text)
		if index == r.StartBlock {
			from = r.StartOffset
		}
		if index == r.EndBlock {
			to = r.EndOffset
		}

		// position is shared across the whole walk of one block: it tracks how
		// many characters of that block's text have been passed, which is what
		// the offsets are measured in.
		position := 0
		var fragment strings.Builder
		for child := block.node.FirstChild; child != nil; child = child.NextSibling {
			clip(child, &position, from, to, &fragment)
		}

		if strings.TrimSpace(fragment.String()) == "" {
			continue
		}

		tag := extractTag(block.node)
		out.WriteString("<" + tag + ">")
		out.WriteString(fragment.String())
		out.WriteString("</" + tag + ">")
	}

	return out.String(), nil
}

// extractTag decides what element wraps a clipped block. Preformatted text and
// quotations carry meaning in their tag; everything else becomes a paragraph,
// because a list item or table cell makes no sense outside its container.
//
// insideBlockquote (shared with renderTag in render.go) catches the same
// shape that motivates it there: a multi-paragraph pull quote —
// <blockquote><p>...</p><p>...</p></blockquote>, what Substack's own editor
// writes for any quote of more than one paragraph — never has a block whose
// node is the <blockquote> itself, since collectBlocks' leaf rule lets the
// inner <p>s claim the blocks instead. Selecting text from such a passage and
// extracting it would otherwise store the extract as a bare <p>, losing the
// quote styling permanently at the point of extraction rather than just at
// one render — every future render of that stored extract inherits the loss,
// not just the article's own live view of it.
func extractTag(node *html.Node) string {
	switch node.DataAtom {
	case atom.Pre:
		return "pre"
	case atom.Blockquote:
		return "blockquote"
	default:
		if insideBlockquote(node) {
			return "blockquote"
		}
		return "p"
	}
}

// clip writes the portion of node's text that falls within [from, to), keeping
// any inline elements that still have content after the cut.
//
// position is a pointer because it must advance across the whole sibling walk,
// not just within one subtree: offsets are measured against the block's entire
// text, so every text node passed anywhere in the tree moves it forward.
func clip(node *html.Node, position *int, from, to int, out *strings.Builder) {
	switch node.Type {
	case html.TextNode:
		start := *position
		*position += len(node.Data)

		// Intersect this text node's span with the requested range.
		lower := max(start, from)
		upper := min(*position, to)
		if lower < upper {
			out.WriteString(html.EscapeString(node.Data[lower-start : upper-start]))
		}

	case html.ElementNode:
		// A line break occupies no characters, so it has no span to intersect;
		// keep it when it sits strictly inside the range, and drop it at the
		// edges where it would only add leading or trailing blank space.
		if node.DataAtom == atom.Br {
			if *position > from && *position < to {
				out.WriteString("<br>")
			}
			return
		}

		// Render children into a scratch buffer first. Whether the element is
		// worth keeping depends on whether anything survived inside it, which
		// is only known after the walk.
		var inner strings.Builder
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			clip(child, position, from, to, &inner)
		}
		if inner.Len() == 0 {
			return
		}

		if !inlineTags[node.DataAtom] {
			// Unknown or block-level element inside a block: keep the text,
			// drop the wrapper rather than emitting structure into an extract.
			out.WriteString(inner.String())
			return
		}

		out.WriteString(openTag(node))
		out.WriteString(inner.String())
		out.WriteString("</" + node.Data + ">")
	}
}

// openTag renders an inline element's opening tag with a strict attribute
// allowlist.
//
// The input has already been through the sanitiser, but this function emits raw
// HTML into a template, so it re-checks rather than trusting a guarantee made
// somewhere else in the pipeline.
func openTag(node *html.Node) string {
	if node.DataAtom != atom.A {
		return "<" + node.Data + ">"
	}

	href := ""
	ok := false
	for _, attribute := range node.Attr {
		if attribute.Key == "href" && safeURL(attribute.Val) {
			href, ok = attribute.Val, true
			break
		}
	}
	if !ok {
		// A link whose destination did not survive the check keeps its text
		// but loses its destination.
		return "<a>"
	}

	var tag strings.Builder
	tag.WriteString(`<a href="` + html.EscapeString(href) + `"`)

	// A same-page jump — a bare "#fragment" href, the shape
	// web.rewriteSamePageLinks produces for a footnote that would otherwise
	// point back at the article's own address — opens in place. Anything
	// else opens in a new tab, so following a link never navigates the
	// reader itself away from the article.
	if !strings.HasPrefix(href, "#") {
		tag.WriteString(` rel="noopener noreferrer" target="_blank"`)
	}

	// id and class both already passed the sanitiser's own allowlist before
	// reaching here — id generally, class only for the couple of literal
	// values the reader relies on (an annotation's own note, a footnote's
	// number; see web.newPolicy). Carrying them through is what gives a
	// same-page href above somewhere to actually land, and lets a footnote
	// number keep the styling that tells it apart from the footnote's text.
	if id := attr(node, "id"); id != "" {
		tag.WriteString(` id="` + html.EscapeString(id) + `"`)
	}
	if class := attr(node, "class"); class != "" {
		tag.WriteString(` class="` + html.EscapeString(class) + `"`)
	}

	tag.WriteString(">")
	return tag.String()
}

// safeURL rejects schemes that can execute, most importantly javascript:.
// Relative URLs are allowed; they resolve against increader's own origin, which
// is harmless.
func safeURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "", "http", "https", "mailto":
		return true
	default:
		return false
	}
}
