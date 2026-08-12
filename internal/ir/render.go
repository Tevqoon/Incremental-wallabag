package ir

import (
	"regexp"
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

// NoReadPoint means an article has not been read into yet.
const NoReadPoint = -1

// RenderOptions controls the reader view.
type RenderOptions struct {
	// Marks are passages already extracted, shown highlighted.
	Marks []Mark

	// ReadPoint is the block where reading stopped, or NoReadPoint. The block
	// is tagged so the reader can see where they left off — SuperMemo shows the
	// read point on return rather than only scrolling to it, and being able to
	// see the boundary between read and unread is most of its value.
	ReadPoint int

	// ImageURLs resolves an Image's original Src to somewhere safe to load it
	// from, plus its intrinsic size if known — this package has no I/O, so it
	// never fetches, measures or rewrites a URL itself; the caller resolves
	// every image up front and hands back this map. An image whose Src has no
	// entry here is skipped entirely rather than rendered with its original
	// URL, which would defeat the point of resolving it in the first place —
	// see the web package's image cache.
	ImageURLs map[string]ResolvedImage
}

// ResolvedImage is somewhere safe to load an image from, plus its intrinsic
// pixel size if it is known. Width or Height being 0 means "unknown" — see
// renderImage, which omits both attributes rather than emit a bogus zero.
type ResolvedImage struct {
	URL           string
	Width, Height int
}

// Render produces the reader view: every block as a top-level element carrying
// its index in a data-b attribute, with existing extracts wrapped in <mark>,
// and any images interleaved at the position they held in the article.
//
// The data-b attributes are the contract with the browser. Client-side, a
// selection is reported as the enclosing block's data-b plus a character offset
// into that element's textContent; server-side, those two numbers index into
// exactly the same enumeration. Nothing else needs to agree between the two.
func (a *Article) Render(options RenderOptions) string {
	windows := a.windowsByBlock(options.Marks)
	imagesAfter := a.imagesByBlock()
	tablesAfter := a.tablesByBlock()

	var out strings.Builder
	for _, image := range imagesAfter[-1] {
		renderImage(image, options.ImageURLs, &out)
	}
	for _, table := range tablesAfter[-1] {
		renderTable(table, options.ImageURLs, &out)
	}

	for _, block := range a.blocks {
		tag, class := renderTag(block.node)

		// The read point marks the *start* of what is still unread, so it sits
		// on the block reading stopped at rather than the one before it.
		if block.Index == options.ReadPoint && options.ReadPoint > 0 {
			class = strings.TrimSpace(class + " read-point")
		}

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

		for _, image := range imagesAfter[block.Index] {
			renderImage(image, options.ImageURLs, &out)
		}
		for _, table := range tablesAfter[block.Index] {
			renderTable(table, options.ImageURLs, &out)
		}
	}
	return out.String()
}

// imagesByBlock groups the article's images by the block they trail, so
// Render can look up "what comes right after block N" (or before the first
// block, for -1) in one map access per position instead of scanning the
// whole list at every block.
func (a *Article) imagesByBlock() map[int][]Image {
	byBlock := make(map[int][]Image, len(a.images))
	for _, image := range a.images {
		byBlock[image.AfterBlock] = append(byBlock[image.AfterBlock], image)
	}
	return byBlock
}

// renderImage writes one image, if it resolved to somewhere safe to load —
// see RenderOptions.ImageURLs. An unresolved image is skipped rather than
// rendered with its original URL: falling back would defeat the point of
// resolving it server-side in the first place.
//
// width and height are emitted together, or not at all: they are what lets
// the browser reserve the image's box before its bytes arrive, and
// loading="lazy" is only safe with that box reserved — an unreserved lazy
// image is exactly what makes every image on the page collapse to zero
// height in a batch swap (see migrations/011_image_dimensions.sql). Drop the
// dimensions without also dropping loading="lazy" and that bug comes back.
func renderImage(image Image, resolved map[string]ResolvedImage, out *strings.Builder) {
	target, ok := resolved[image.Src]
	if !ok || target.URL == "" {
		return
	}
	out.WriteString(`<figure class="article-image"><img src="` + html.EscapeString(target.URL) + `" alt="`)
	out.WriteString(html.EscapeString(image.Alt))
	out.WriteString(`"`)
	if target.Width > 0 && target.Height > 0 {
		out.WriteString(` width="` + strconv.Itoa(target.Width) + `" height="` + strconv.Itoa(target.Height) + `"`)
	}
	out.WriteString(` loading="lazy"></figure>`)
}

// tablesByBlock groups the article's tables by the block they trail —
// mirrors imagesByBlock exactly, same reason.
func (a *Article) tablesByBlock() map[int][]Table {
	byBlock := make(map[int][]Table, len(a.tables))
	for _, table := range a.tables {
		byBlock[table.AfterBlock] = append(byBlock[table.AfterBlock], table)
	}
	return byBlock
}

// renderTable writes one table verbatim from its own sanitised node, wrapped
// in a horizontally scrollable container so a wide grid cannot force the
// whole page wider than the reader's own prose column (see .table-wrap in
// app.css). Unlike a Block, a table is never re-derived from parsed text: a
// grid has no linear order to flatten it back into without losing exactly
// the row/column structure this exists to keep.
func renderTable(table Table, resolved map[string]ResolvedImage, out *strings.Builder) {
	out.WriteString(`<div class="table-wrap"><table>`)
	for child := table.node.FirstChild; child != nil; child = child.NextSibling {
		renderTableChild(child, resolved, out)
	}
	out.WriteString(`</table></div>`)
}

// renderTableChild serialises one node inside a table, recursively. It
// extends the sanitiser the same trust article.HTML's clip already does for
// an extract, with the same two exceptions: <img>, which is swapped for its
// resolved, cached address rather than kept pointing at the original host
// (see resolveImages), and <a>, whose href openTag re-verifies. Every other
// tag is emitted by name with no attributes beyond the couple of explicitly
// allowed ones below — never a raw copy of node.Attr — since this writes raw
// HTML straight into the page and must not assume the sanitiser upstream
// already caught everything.
func renderTableChild(node *html.Node, resolved map[string]ResolvedImage, out *strings.Builder) {
	if node.Type == html.TextNode {
		out.WriteString(html.EscapeString(node.Data))
		return
	}
	if node.Type != html.ElementNode {
		return
	}

	switch node.DataAtom {
	case atom.Img:
		renderInlineImage(Image{Src: attr(node, "src"), Alt: attr(node, "alt")}, resolved, out)
		return
	case atom.Br:
		out.WriteString("<br>")
		return
	case atom.A:
		out.WriteString(openTag(node))
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			renderTableChild(child, resolved, out)
		}
		out.WriteString("</a>")
		return
	}

	tag := node.Data
	out.WriteString("<" + tag)
	if node.DataAtom == atom.Td || node.DataAtom == atom.Th {
		writeSpanAttr(node, "colspan", out)
		writeSpanAttr(node, "rowspan", out)
	}
	out.WriteString(">")
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		renderTableChild(child, resolved, out)
	}
	out.WriteString("</" + tag + ">")
}

// spanAttr matches a colspan/rowspan value the same way bluemonday's own
// Integer pattern would — re-checked here for the same reason openTag
// re-checks a link's href: this writes the value straight into raw HTML, so
// it cannot lean on an upstream guarantee it has no way to verify itself.
var spanAttr = regexp.MustCompile(`^[0-9]{1,3}$`)

func writeSpanAttr(node *html.Node, name string, out *strings.Builder) {
	if value := attr(node, name); spanAttr.MatchString(value) {
		out.WriteString(" " + name + `="` + value + `"`)
	}
}

// renderInlineImage is renderImage without the <figure> wrapper: a table
// cell is not the illustration's own block, just wherever it happens to sit
// in the grid, and one more level of block structure would only fight the
// table's own layout. Also unlike renderImage, width/height are left off —
// the small icons this exists for (flags, glyphs) do not need their box
// reserved against layout shift the way a full illustration does.
func renderInlineImage(image Image, resolved map[string]ResolvedImage, out *strings.Builder) {
	target, ok := resolved[image.Src]
	if !ok || target.URL == "" {
		return
	}
	out.WriteString(`<img src="` + html.EscapeString(target.URL) + `" alt="`)
	out.WriteString(html.EscapeString(image.Alt))
	out.WriteString(`" loading="lazy">`)
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
		// A quoted list — <blockquote><ul><li>...</li></ul></blockquote>,
		// the same pull-quote shape as insideBlockquote's own doc comment
		// but with a list in place of paragraphs — hits the identical leaf
		// rule: the <li>s claim the blocks, so without this check the quote
		// styling below would apply to every other tag but never to one
		// that also happens to be a list item. Rendering the block as
		// <blockquote class="list-item"> rather than swapping the class for
		// something quote-specific keeps both CSS rules — the bullet and
		// the left border — applying at once, since neither is keyed off
		// the other.
		if insideBlockquote(node) {
			return "blockquote", "list-item"
		}
		return "p", "list-item"
	default:
		// store.annotationHTML marks a reader's own note on an imported
		// passage with this class so it renders visually distinct from the
		// passage above it — otherwise lost here, since every other <p>
		// falls through to the same bare default and this is the one place
		// a class survives sanitising only to be discarded on the way to the
		// page. bluemonday's policy (see newPolicy) is the only source of an
		// arbitrary class on a <p> reaching this node at all, so trusting it
		// here opens nothing wider than what already got through.
		if attr(node, "class") == "annotation-note" {
			return "p", "annotation-note"
		}
		if insideBlockquote(node) {
			return "blockquote", ""
		}
		return "p", ""
	}
}

// insideBlockquote reports whether node sits inside a <blockquote> that is
// not itself the block being rendered.
//
// A multi-paragraph pull quote — <blockquote><p>...</p><p>...</p></blockquote>,
// the shape Substack's own editor writes for any quote of more than one
// paragraph — hits collectBlocks' leaf rule: the <blockquote> contains block
// tags of its own, so it never emits a block itself, and each inner <p>
// becomes its own block instead, with that <p> as Block.node. Without this
// check, renderTag never sees the <blockquote> at all for such a block: it
// sees a plain <p> and falls through to the bare default, so the passage
// reaches the page as an ordinary paragraph, visually identical to the
// article's own prose, with the source's own left-border quote styling
// silently dropped. Walking every ancestor rather than just node.Parent
// handles a quote nested one level deeper too, e.g. a <div> wrapper some
// other publisher's markup puts between <blockquote> and <p>.
func insideBlockquote(node *html.Node) bool {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if parent.DataAtom == atom.Blockquote {
			return true
		}
	}
	return false
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
