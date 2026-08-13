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
// three paragraphs produces three paragraphs. This is the only copy of a
// passage's structure increader keeps once the extract is made: the parent
// article's own HTML can change or vanish on a re-fetch, and the plain-text
// Quote stored alongside this (see web.handleExtract) is deliberately what
// goes to wallabag — its highlight API has no field for markup at all — so
// whatever this function fails to carry over is gone from increader's own
// copy for good, not just unstyled on one page.
func (a *Article) HTML(r Range) (string, error) {
	if err := a.Valid(r); err != nil {
		return "", err
	}

	var out strings.Builder
	list := openList{}
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

		// A list item outside a blockquote gets its <ul>/<ol> back — see
		// openList — rather than falling through to extractTag's plain "p"
		// like every other tag does. One inside a blockquote is left to
		// extractTag instead: HTML has no way to nest "this is a list item"
		// and "this is a quotation" in one tag, and losing the bullet is the
		// smaller loss of the two, since a quoted list is rare and the
		// surrounding quote is what gives the passage its meaning.
		if block.node.DataAtom == atom.Li && !insideBlockquote(block.node) {
			list.add(block.node, fragment.String(), &out)
			continue
		}
		list.close(&out)

		tag := extractTag(block.node)
		out.WriteString("<" + tag + ">")
		out.WriteString(fragment.String())
		out.WriteString("</" + tag + ">")
	}
	list.close(&out)

	return out.String(), nil
}

// openList tracks the <ul>/<ol> HTML is partway through writing, so a run of
// consecutive <li> blocks becomes one shared list instead of each reverting
// to a bare, invalid <li> with nothing to contain it.
//
// Zero value is "no list open" — every method is safe to call on one before
// add's first call ever runs.
type openList struct {
	tag  string // "" means no list is currently open.
	open bool
}

// listTag reports whether node's own parent is an ordered list, so a
// numbered source list stays numbered rather than becoming generic bullets.
func listTag(node *html.Node) string {
	if node.Parent != nil && node.Parent.DataAtom == atom.Ol {
		return "ol"
	}
	return "ul"
}

// add writes one <li>, opening a new list first if none is open yet, or the
// open one is the wrong kind for node (a numbered list immediately followed
// by a bulleted one, or vice versa — two distinct source lists that happen
// to sit in consecutive blocks with nothing extracted between them).
func (l *openList) add(node *html.Node, inner string, out *strings.Builder) {
	tag := listTag(node)
	if l.open && l.tag != tag {
		l.close(out)
	}
	if !l.open {
		out.WriteString("<" + tag + ">")
		l.tag, l.open = tag, true
	}
	out.WriteString("<li>" + inner + "</li>")
}

// close ends whatever list is open, if any. Safe to call unconditionally
// between every non-<li> block and once more after the loop, since it is a
// no-op when nothing is open.
func (l *openList) close(out *strings.Builder) {
	if !l.open {
		return
	}
	out.WriteString("</" + l.tag + ">")
	l.open = false
}

// extractTag decides what element wraps a clipped block that HTML's own
// list handling above did not already take care of. Preformatted text,
// quotations and headings all carry meaning in their tag; everything else
// becomes a paragraph, because a table cell makes no sense outside its
// container the way a run of list items now does.
//
// insideBlockquote (shared with renderTag in render.go) catches the same
// shape that motivates it there: a multi-paragraph pull quote —
// <blockquote><p>...</p><p>...</p></blockquote>, what Substack's own editor
// writes for any quote of more than one paragraph — never has a block whose
// node is the <blockquote> itself, since collectBlocks' leaf rule lets the
// inner <p>s (or <li>s) claim the blocks instead. Selecting text from such a
// passage and extracting it would otherwise store the extract as a bare
// <p>, losing the quote styling permanently at the point of extraction
// rather than just at one render — every future render of that stored
// extract inherits the loss, not just the article's own live view of it.
func extractTag(node *html.Node) string {
	switch node.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		return node.Data
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
