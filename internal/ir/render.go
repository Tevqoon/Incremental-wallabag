package ir

import (
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Mark is a passage already extracted from an article, to be shown as
// highlighted so the reader can see what has been harvested.
type Mark struct {
	Range     Range
	ElementID int64
}

// Len returns the number of addressable blocks in the article.
func (a *Article) Len() int { return len(a.blocks) }

// Render produces the reader view: every block as a top-level element carrying
// its index in a data-b attribute, with existing extracts wrapped in <mark>.
//
// The data-b attributes are the contract with the browser. Client-side, a
// selection is reported as the enclosing block's data-b plus a character offset
// into that element's textContent; server-side, those two numbers index into
// exactly the same enumeration. Nothing else needs to agree between the two.
func (a *Article) Render(marks []Mark) string {
	windows := a.windowsByBlock(marks)

	var out strings.Builder
	for _, block := range a.blocks {
		tag, class := renderTag(block.node)

		out.WriteString("<" + tag)
		if class != "" {
			out.WriteString(` class="` + class + `"`)
		}
		out.WriteString(` data-b="` + strconv.Itoa(block.Index) + `">`)

		position := 0
		for child := block.node.FirstChild; child != nil; child = child.NextSibling {
			renderNode(child, &position, windows[block.Index], &out)
		}

		out.WriteString("</" + tag + ">")
	}
	return out.String()
}

// window is a highlighted span within a single block.
type window struct {
	from      int
	to        int
	elementID int64
}

// windowsByBlock projects marks (which may span several blocks) onto per-block
// spans, then merges overlaps so rendering can walk them in one pass.
func (a *Article) windowsByBlock(marks []Mark) map[int][]window {
	byBlock := make(map[int][]window)

	for _, mark := range marks {
		// A mark whose offsets no longer fit the article is skipped rather
		// than reported: the article may have been re-fetched and changed
		// shape, and a stale highlight should not stop the page rendering.
		if a.Valid(mark.Range) != nil {
			continue
		}

		for index := mark.Range.StartBlock; index <= mark.Range.EndBlock; index++ {
			from, to := 0, len(a.blocks[index].Text)
			if index == mark.Range.StartBlock {
				from = mark.Range.StartOffset
			}
			if index == mark.Range.EndBlock {
				to = mark.Range.EndOffset
			}
			if from < to {
				byBlock[index] = append(byBlock[index], window{from, to, mark.ElementID})
			}
		}
	}

	for index, spans := range byBlock {
		byBlock[index] = mergeWindows(spans)
	}
	return byBlock
}

// mergeWindows sorts spans and fuses any that touch or overlap, so no character
// is ever covered twice. Overlapping extracts are normal — re-reading a passage
// and extracting a longer version of it is the expected workflow — and nested
// <mark> elements would render as darker bands with no meaning.
func mergeWindows(spans []window) []window {
	if len(spans) < 2 {
		return spans
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })

	merged := spans[:1]
	for _, span := range spans[1:] {
		last := &merged[len(merged)-1]
		if span.from <= last.to {
			if span.to > last.to {
				last.to = span.to
			}
			continue
		}
		merged = append(merged, span)
	}
	return merged
}

// renderTag maps a block element onto what the reader view should emit.
//
// The reader flattens the article into a linear sequence of blocks, so a list
// item cannot keep its <li> — there is no <ul> around it. It becomes a
// paragraph with a class instead, and CSS draws the bullet.
func renderTag(node *html.Node) (tag, class string) {
	switch node.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		return node.Data, ""
	case atom.Pre:
		return "pre", ""
	case atom.Blockquote:
		return "blockquote", ""
	case atom.Li:
		return "p", "list-item"
	default:
		return "p", ""
	}
}

// renderNode writes one node of a block, applying highlight windows to text.
func renderNode(node *html.Node, position *int, windows []window, out *strings.Builder) {
	switch node.Type {
	case html.TextNode:
		start := *position
		*position += len(node.Data)
		writeHighlighted(node.Data, start, windows, out)

	case html.ElementNode:
		if node.DataAtom == atom.Br {
			out.WriteString("<br>")
			return
		}

		inline := inlineTags[node.DataAtom]
		if inline {
			out.WriteString(openTag(node))
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			renderNode(child, position, windows, out)
		}
		if inline {
			out.WriteString("</" + node.Data + ">")
		}
	}
}

// writeHighlighted emits one text node, splitting it wherever a highlight
// window starts or ends.
//
// start is the text node's own offset within the block, so that positions can
// be compared against windows, which are measured the same way.
func writeHighlighted(text string, start int, windows []window, out *strings.Builder) {
	if len(windows) == 0 {
		out.WriteString(html.EscapeString(text))
		return
	}

	for offset := 0; offset < len(text); {
		absolute := start + offset

		if span, inside := windowAt(absolute, windows); inside {
			end := clampEnd(span.to-start, offset, len(text))
			out.WriteString(`<mark class="extract" data-element="` +
				strconv.FormatInt(span.elementID, 10) + `">`)
			out.WriteString(html.EscapeString(text[offset:end]))
			out.WriteString("</mark>")
			offset = end
			continue
		}

		// Plain text runs until the next highlight begins, or to the end.
		end := len(text)
		if next, found := nextWindowStart(absolute, windows); found {
			end = clampEnd(next-start, offset, len(text))
		}
		out.WriteString(html.EscapeString(text[offset:end]))
		offset = end
	}
}

// clampEnd keeps a computed end position inside the text and strictly after the
// current offset. Without the lower bound a zero-width window would leave the
// loop above spinning forever.
func clampEnd(end, offset, length int) int {
	return min(max(end, offset+1), length)
}

func windowAt(position int, windows []window) (window, bool) {
	for _, span := range windows {
		if position >= span.from && position < span.to {
			return span, true
		}
	}
	return window{}, false
}

func nextWindowStart(position int, windows []window) (int, bool) {
	for _, span := range windows {
		if span.from > position {
			return span.from, true
		}
	}
	return 0, false
}
