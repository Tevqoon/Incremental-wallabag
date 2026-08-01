package wallabag

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// serializedRange is one entry of an annotation's "ranges" array, in the
// exact shape wallabag's vendored annotator fork (github.com/wallabag/annotator,
// src/xpath-range) serializes and expects back — confirmed against real
// ranges already present in this account, not just read off that fork's
// source. Offsets are JSON strings, not numbers: that is how a native
// annotation encodes them, and it is what wallabag's own client needs to see
// to resolve the range at all.
type serializedRange struct {
	Start       string `json:"start"`
	StartOffset string `json:"startOffset"`
	End         string `json:"end"`
	EndOffset   string `json:"endOffset"`
}

// computeRanges locates quote inside rawHTML and returns the range wallabag's
// own web and Android clients need to draw it as a highlight in place,
// rather than just storing the text with nothing to anchor it to.
//
// rawHTML must be the article content exactly as wallabag itself serves it —
// the same string its own reading view drops straight inside the <article>
// element that its annotator.js fork is initialised on
// (templates/Entry/entry.html.twig: `{{ entry.content|raw }}`, nothing in
// between). That agreement between the two clients' idea of the DOM is the
// entire reason this function takes rawHTML as an argument rather than
// reaching for increader's own parsed, sanitised, block-indexed view of the
// same article: that view is free to diverge from wallabag's raw content —
// different sanitiser policy, different block splitting — without any of it
// mattering here, because this never touches it.
//
// Best-effort: returns nil if quote cannot be found, or the boundary
// resolution below fails for any reason. A highlight with no range still
// gets created — this only decides whether wallabag's own reader can draw it
// in place, not whether the annotation exists at all.
func computeRanges(rawHTML, quote string) []serializedRange {
	root := parseBody(rawHTML)
	if root == nil {
		return nil
	}

	allText := collectTextNodes(root)
	flat := flattenText(allText)

	start, end, ok := locateQuote(flat, quote)
	if !ok {
		return nil
	}

	startNode, startOffset, ok := nodeAtOffset(allText, start, false)
	if !ok {
		return nil
	}
	endNode, endOffset, ok := nodeAtOffset(allText, end, true)
	if !ok {
		return nil
	}

	startPath, startWithin, ok := serializeBoundary(startNode, startOffset, root)
	if !ok {
		return nil
	}
	endPath, endWithin, ok := serializeBoundary(endNode, endOffset, root)
	if !ok {
		return nil
	}

	return []serializedRange{{
		Start:       startPath,
		StartOffset: strconv.Itoa(startWithin),
		End:         endPath,
		EndOffset:   strconv.Itoa(endWithin),
	}}
}

// parseBody parses rawHTML and returns its <body> — the root every path is
// computed relative to. html.Parse always produces a full document even from
// a bare fragment, wrapping it in <html><body>...; that wrapper is exactly
// equivalent to wallabag's own <article> element, since both merely hold
// entry.content's top-level nodes as direct children with nothing else
// between them and content that starts numbering at [1].
func parseBody(rawHTML string) *html.Node {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return nil
	}
	var find func(*html.Node) *html.Node
	find = func(n *html.Node) *html.Node {
		if n.Type == html.ElementNode && n.DataAtom == atom.Body {
			return n
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if found := find(c); found != nil {
				return found
			}
		}
		return nil
	}
	return find(doc)
}

// collectTextNodes returns every text node under root, in document order.
// annotator's own getTextNodes gets there via an odd reverse-then-reverse
// walk over lastChild/previousSibling; the result is plain document order
// either way, so this just walks forward.
func collectTextNodes(root *html.Node) []*html.Node {
	var nodes []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			nodes = append(nodes, n)
		case html.CommentNode:
			// Skipped, matching annotator's own getTextNodes.
		default:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
	}
	walk(root)
	return nodes
}

func flattenText(nodes []*html.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString(n.Data)
	}
	return b.String()
}

// whitespaceRun matches one or more whitespace characters, for building a
// tolerant search pattern out of a literal quote.
var whitespaceRun = regexp.MustCompile(`\s+`)

// locateQuote finds quote's [start, end) byte range within flat, the whole
// article's text with no separators inserted between elements — matching
// annotator's own concatenation, which inserts none either.
//
// Matching is whitespace-tolerant, and deliberately tolerates a whitespace
// run in quote matching zero characters in flat, not just a different run:
// increader's own multi-block quotes join separate blocks with a blank line
// for readability, but adjacent block elements carry no whitespace between
// their text nodes at the DOM level, so a quote spanning a paragraph break
// would otherwise never match at all.
func locateQuote(flat, quote string) (start, end int, ok bool) {
	trimmed := strings.TrimSpace(quote)
	if trimmed == "" {
		return 0, 0, false
	}
	pattern := regexp.QuoteMeta(trimmed)
	pattern = whitespaceRun.ReplaceAllString(pattern, `\s*`)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return 0, 0, false
	}
	loc := re.FindStringIndex(flat)
	if loc == nil {
		return 0, 0, false
	}
	return loc[0], loc[1], true
}

// nodeAtOffset finds the text node containing byte offset target, within the
// concatenation of nodes in document order, and the offset within that node.
//
// atEnd controls which side of an exact node boundary wins, because the two
// boundaries of a range are not symmetric here: a start offset prefers
// rolling forward into the next node (annotator's
// getFirstTextNodeNotBefore), while an end offset stays within the node
// whose content it closes out (BrowserRange's own end-of-range handling
// keeps the reference on the earlier node rather than the empty start of the
// next one). Using the same rule for both would resolve one of them into
// the wrong node whenever a boundary lands exactly between two text nodes.
func nodeAtOffset(nodes []*html.Node, target int, atEnd bool) (*html.Node, int, bool) {
	pos := 0
	for i, n := range nodes {
		length := len(n.Data)
		next := pos + length
		switch {
		case target < next:
			return n, target - pos, true
		case target == next:
			if atEnd || i == len(nodes)-1 {
				return n, length, true
			}
			// A start boundary tied exactly to this node's end belongs to
			// the start of the next node instead; keep going.
		}
		pos = next
	}
	return nil, 0, false
}

// serializeBoundary is xpath-range's own serialization(): the path of the
// boundary node's parent element, and the offset within that element's own
// text — not the whole document — up to the boundary.
//
// The offset is counted in UTF-16 code units, not bytes: it was a browser
// running JavaScript that produced every offset already stored this way,
// since JavaScript string length counts UTF-16 units. A byte count agrees
// with that everywhere the article is plain ASCII and drifts out from under
// it at the first soft hyphen, curly quote or em dash — precisely the
// characters real article text is full of, and precisely the bug increader
// already had to fix once for its own, entirely separate offset handling
// (see Article.ByteOffset). This is that same fix, independently, because
// this package deliberately does not share code with increader's own
// reading model.
func serializeBoundary(node *html.Node, offsetInNode int, root *html.Node) (path string, offset int, ok bool) {
	parent := node.Parent
	if parent == nil {
		return "", 0, false
	}
	path, ok = xpathTo(parent, root)
	if !ok {
		return "", 0, false
	}

	var preceding strings.Builder
	for _, sibling := range collectTextNodes(parent) {
		if sibling == node {
			preceding.WriteString(sibling.Data[:offsetInNode])
			return path, utf16Length(preceding.String()), true
		}
		preceding.WriteString(sibling.Data)
	}
	return "", 0, false
}

// utf16Length is how long s would report itself as in JavaScript.
func utf16Length(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// xpathTo builds elem's path relative to root, one segment per element
// ancestor: tagname[N], where N is elem's 1-indexed position among sibling
// elements sharing its own tag name.
//
// This is annotator's simpleXPathJQuery ported directly —
// $(elem.parentNode).children(tagName).index(elem) + 1 — rather than
// approximated with, say, position among all children regardless of tag:
// wallabag's own client resolves the path with exactly this rule and no
// other, so anything else would silently fail to highlight instead of
// merely highlighting the wrong text.
func xpathTo(elem, root *html.Node) (string, bool) {
	var segments []string
	for n := elem; n != root; n = n.Parent {
		if n == nil || n.Type != html.ElementNode {
			return "", false
		}
		segments = append(segments, n.Data+"["+strconv.Itoa(sameTagPosition(n))+"]")
	}
	for i, j := 0, len(segments)-1; i < j; i, j = i+1, j-1 {
		segments[i], segments[j] = segments[j], segments[i]
	}
	return "/" + strings.Join(segments, "/"), true
}

// sameTagPosition is elem's 1-indexed position among its parent's element
// children that share its own tag name.
func sameTagPosition(elem *html.Node) int {
	pos := 0
	for sibling := elem.Parent.FirstChild; sibling != nil; sibling = sibling.NextSibling {
		if sibling.Type == html.ElementNode && sibling.Data == elem.Data {
			pos++
		}
		if sibling == elem {
			break
		}
	}
	return pos
}
