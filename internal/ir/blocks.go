// Package ir implements incremental reading: addressing passages inside an
// article, turning them into extracts and cloze items, and scheduling when
// material comes back.
//
// It is a leaf package. Everything here is a pure function over values — no
// database, no HTTP, no clock beyond what callers pass in — so the algorithm
// can be read and tested without running the application.
package ir

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// blockTags are the elements that can hold a paragraph of readable text.
//
// Generic containers (div, section, article) are included because some articles
// put text directly in them, but the leaf rule below means they only ever
// produce a block when they contain no more specific one.
var blockTags = map[atom.Atom]bool{
	atom.P: true, atom.Li: true, atom.Blockquote: true, atom.Pre: true,
	atom.H1: true, atom.H2: true, atom.H3: true,
	atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Dd: true, atom.Dt: true, atom.Td: true, atom.Th: true,
	atom.Figcaption: true, atom.Div: true, atom.Section: true, atom.Article: true,
}

// inlineTags survive inside an extract. Anything else is unwrapped to its text,
// so an extract never carries layout or structure out of its context.
var inlineTags = map[atom.Atom]bool{
	atom.A: true, atom.Em: true, atom.I: true, atom.Strong: true, atom.B: true,
	atom.Code: true, atom.Sup: true, atom.Sub: true, atom.Mark: true,
	atom.Small: true, atom.Span: true, atom.U: true, atom.Abbr: true,
	atom.Cite: true, atom.Q: true, atom.Del: true, atom.Ins: true,
}

// Block is one addressable unit of an article: a paragraph, list item or
// heading. Positions in an article are expressed as a block index plus a
// character offset within that block.
type Block struct {
	// Index is the block's position in the article, counting from zero.
	Index int

	// Text is the block's plain text, formed by concatenating its descendant
	// text nodes verbatim.
	//
	// It deliberately matches the DOM's textContent exactly, with no whitespace
	// normalisation. The browser computes selection offsets against
	// textContent, so any normalisation here would shift every offset and
	// silently misplace extracts.
	Text string

	node *html.Node
}

// Image is a decorative picture found in the article, positioned relative to
// the block sequence but not part of it.
//
// It carries no text and cannot be selected, highlighted or extracted — an
// image is not addressable the way a Block is, it only has somewhere to
// render. Src is deliberately the *original* URL from the sanitised HTML,
// unresolved: this package has no I/O, so turning it into something a
// browser should actually load is the caller's job — see RenderOptions.
type Image struct {
	// AfterBlock is the index of the block this image follows in document
	// order, or -1 if it appears before the article's first block.
	AfterBlock int
	Src        string
	Alt        string
}

// Article is a parsed, addressable article.
//
// Construct it from the *sanitised* HTML, never the raw source. Sanitising
// removes elements and therefore changes block indices and text offsets; as
// long as both the rendering path and the extract path parse the same
// sanitised output, the addresses they exchange agree. Because sanitising is
// deterministic, that costs a re-sanitise rather than storing a second copy.
type Article struct {
	root   *html.Node
	blocks []Block
	images []Image
}

// ParseArticle parses sanitised article HTML and enumerates its blocks.
func ParseArticle(sanitizedHTML string) (*Article, error) {
	root, err := html.Parse(strings.NewReader(sanitizedHTML))
	if err != nil {
		return nil, fmt.Errorf("ir: parse article: %w", err)
	}

	article := &Article{root: root}
	collectBlocks(root, &article.blocks, &article.images)
	return article, nil
}

// collectBlocks walks the tree in document order, emitting one block per
// "leaf block" element: one that is a block tag and contains no block tag.
// Alongside that, every <img> anywhere in the tree is collected too, tagged
// with the block it trails — see Image.
//
// It reports whether the subtree produced any block, which is what lets a
// parent skip emitting itself when its children already covered the text.
func collectBlocks(node *html.Node, out *[]Block, images *[]Image) bool {
	emitted := false
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != html.ElementNode {
			continue
		}
		if child.DataAtom == atom.Img {
			*images = append(*images, Image{
				AfterBlock: len(*out) - 1,
				Src:        attr(child, "src"),
				Alt:        attr(child, "alt"),
			})
			continue
		}
		if collectBlocks(child, out, images) {
			emitted = true
		}
	}
	if emitted {
		return true
	}

	if node.Type != html.ElementNode || !blockTags[node.DataAtom] {
		return false
	}

	text := textContent(node)
	if strings.TrimSpace(text) == "" {
		// An empty paragraph is not addressable and would only produce
		// off-by-one confusion in the indices.
		return false
	}

	*out = append(*out, Block{Index: len(*out), Text: text, node: node})
	return true
}

// attr reads one attribute's value, or "" if the node does not carry it.
func attr(node *html.Node, key string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == key {
			return attribute.Val
		}
	}
	return ""
}

// Blocks returns the article's blocks in document order.
func (a *Article) Blocks() []Block { return a.blocks }

// Images returns the article's images, in document order, unresolved — see
// Image.
func (a *Article) Images() []Image { return a.images }

// textContent concatenates every descendant text node, exactly as the DOM
// property of the same name does.
func textContent(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			builder.WriteString(n.Data)
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return builder.String()
}

// Range addresses a span of text within an article, from a start position up
// to but not including an end position.
type Range struct {
	StartBlock  int
	StartOffset int
	EndBlock    int
	EndOffset   int
}

// Valid reports whether the range addresses real positions in the article.
func (a *Article) Valid(r Range) error {
	if r.StartBlock < 0 || r.StartBlock >= len(a.blocks) {
		return fmt.Errorf("ir: start block %d is outside the article (%d blocks)", r.StartBlock, len(a.blocks))
	}
	if r.EndBlock < 0 || r.EndBlock >= len(a.blocks) {
		return fmt.Errorf("ir: end block %d is outside the article (%d blocks)", r.EndBlock, len(a.blocks))
	}
	if r.EndBlock < r.StartBlock {
		return fmt.Errorf("ir: range ends (block %d) before it starts (block %d)", r.EndBlock, r.StartBlock)
	}
	if r.StartOffset < 0 || r.StartOffset > len(a.blocks[r.StartBlock].Text) {
		return fmt.Errorf("ir: start offset %d is outside block %d", r.StartOffset, r.StartBlock)
	}
	if r.EndOffset < 0 || r.EndOffset > len(a.blocks[r.EndBlock].Text) {
		return fmt.Errorf("ir: end offset %d is outside block %d", r.EndOffset, r.EndBlock)
	}
	if r.StartBlock == r.EndBlock && r.EndOffset <= r.StartOffset {
		return fmt.Errorf("ir: range in block %d is empty", r.StartBlock)
	}
	return nil
}

// Text returns the plain text a range covers, with blocks separated by blank
// lines.
//
// Browsers disagree about what separator Selection.toString() puts between
// blocks, so this is increader's own canonical rendering rather than an attempt
// to reproduce any particular browser's. Comparisons against a client-supplied
// quote go through NormalizeSpace, which makes the difference immaterial.
func (a *Article) Text(r Range) (string, error) {
	if err := a.Valid(r); err != nil {
		return "", err
	}

	if r.StartBlock == r.EndBlock {
		return a.blocks[r.StartBlock].Text[r.StartOffset:r.EndOffset], nil
	}

	parts := []string{a.blocks[r.StartBlock].Text[r.StartOffset:]}
	for index := r.StartBlock + 1; index < r.EndBlock; index++ {
		parts = append(parts, a.blocks[index].Text)
	}
	parts = append(parts, a.blocks[r.EndBlock].Text[:r.EndOffset])
	return strings.Join(parts, blockSeparator), nil
}

// blockSeparator joins blocks in the flat text an article renders to.
const blockSeparator = "\n\n"

// FlatOffset converts a block/offset position into an offset into the article's
// flat text — the string Text returns for a range covering everything.
//
// Cloze deletions need this. They are stored as offsets into an element's saved
// text, which is flat, but the browser reports positions in block coordinates
// like everything else. For a single-block element the two agree and the
// conversion is invisible; for one spanning paragraphs they diverge by the
// separators, and using block offsets directly would silently delete the wrong
// words.
func (a *Article) FlatOffset(block, offset int) (int, bool) {
	if block < 0 || block >= len(a.blocks) {
		return 0, false
	}
	if offset < 0 || offset > len(a.blocks[block].Text) {
		return 0, false
	}

	flat := 0
	for index := 0; index < block; index++ {
		flat += len(a.blocks[index].Text) + len(blockSeparator)
	}
	return flat + offset, true
}

// FlatText returns the article's entire text, laid out the way FlatOffset
// measures it.
func (a *Article) FlatText() string {
	texts := make([]string, 0, len(a.blocks))
	for _, block := range a.blocks {
		texts = append(texts, block.Text)
	}
	return strings.Join(texts, blockSeparator)
}

// ByteOffset converts a position within block, measured in runes, into the
// byte offset this package uses internally for slicing Block.Text. It
// reports false if block is out of range.
//
// A browser's Selection API measures a position by JavaScript string
// .length, which counts UTF-16 code units — one per rune for everything
// article text contains (soft hyphens, curly quotes, em dashes all live in
// the Basic Multilingual Plane). Go slices strings by byte, and article text
// is full of multi-byte UTF-8 characters, so a raw browser offset used
// directly as a byte index drifts out of alignment with the intended
// position after the first such character. Converting once here, at the one
// place browser-supplied offsets enter the system, keeps Text, HTML,
// FlatOffset, Render and every stored Range agreeing with each other in
// bytes, same as before this existed.
func (a *Article) ByteOffset(block, runeOffset int) (int, bool) {
	if block < 0 || block >= len(a.blocks) {
		return 0, false
	}
	return runeToByte(a.blocks[block].Text, runeOffset), true
}

// ByteRange converts r's offsets from runes to bytes; see ByteOffset.
func (a *Article) ByteRange(r Range) (Range, bool) {
	start, ok := a.ByteOffset(r.StartBlock, r.StartOffset)
	if !ok {
		return Range{}, false
	}
	end, ok := a.ByteOffset(r.EndBlock, r.EndOffset)
	if !ok {
		return Range{}, false
	}
	return Range{StartBlock: r.StartBlock, StartOffset: start, EndBlock: r.EndBlock, EndOffset: end}, true
}

// runeToByte converts a position measured in runes into the corresponding
// byte index of s, clamping to len(s) if the count runs past the end (Valid
// then rejects it with a proper error instead of this silently misreading).
func runeToByte(s string, n int) int {
	if n <= 0 {
		return 0
	}
	count := 0
	for i := range s {
		if count == n {
			return i
		}
		count++
	}
	return len(s)
}

// NormalizeSpace collapses every run of whitespace to a single space and trims
// the ends.
//
// Used only to compare two renderings of the same passage. The stored text
// keeps its original whitespace; normalising is for equality checks, where
// differing block separators and source indentation must not count as a
// mismatch.
func NormalizeSpace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
