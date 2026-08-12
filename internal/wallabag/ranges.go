package wallabag

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// invisibleFormatting mirrors internal/web's stripInvisibleFormatting: this
// package deliberately does not depend on ir or web (see recoverQuote's own
// tests), so the same small character set is kept here rather than shared.
// Soft hyphens in particular are common in press-typeset HTML (confirmed on
// a real Financial Times article) and, left in, turn up as stray glyphs in
// anything that later exports the stored quote as plain text.
//
// Spelled out as \u escapes rather than typed literally: these characters
// are invisible by definition, so a literal in source would be exactly as
// unverifiable by eye as the bug this exists to fix.
var invisibleFormatting = map[rune]bool{
	'\u00ad': true, // soft hyphen
	'\u200b': true, // zero-width space
	'\u200c': true, // zero-width non-joiner
	'\u200d': true, // zero-width joiner
	'\u2060': true, // word joiner
	'\ufeff': true, // byte-order mark / zero-width no-break space
}

// stripInvisibleFormatting removes invisibleFormatting's characters.
func stripInvisibleFormatting(s string) string {
	return strings.Map(func(r rune) rune {
		if invisibleFormatting[r] {
			return -1
		}
		return r
	}, s)
}

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

// QuoteOccurrences reports where quote occurs in rawHTML, as offsets into the
// same flattened text computeRanges locates against — the concatenation of
// every text node's data, in document order, with nothing inserted between
// elements (see flattenText). Matching is the same whitespace-tolerant sense
// locateQuote itself uses, via the shared locateQuoteAll.
//
// This exists for a caller outside this package that needs to ask "would
// creating a highlight from this quote land unambiguously in this HTML" —
// wallabag's own annotator resolves a highlight by quote-search exactly like
// this when the caller does not have a range yet, so more than one
// occurrence is not a hypothetical, it is the same ambiguity computeRanges
// itself resolves by silently taking the first match (see
// TestComputeRangesFirstOccurrenceWins). A caller that cares whether that
// silent choice picked the right passage needs the full count and location
// of every occurrence, not just the one computeRanges quietly settled on.
//
// Only the START offset of each occurrence is reported, not the full
// [start,end) pair computeRanges itself needs: a caller asking "how many,
// and roughly where" has no use for the end offset, and adding it back is a
// trivial extension the day something does.
//
// Returns nil if rawHTML cannot be parsed, or quote does not occur at all —
// not an empty non-nil slice, matching locateQuoteAll's own convention for
// "no matches", which in turn matches regexp.FindAllStringIndex's.
func QuoteOccurrences(rawHTML, quote string) []int {
	root := parseBody(rawHTML)
	if root == nil {
		return nil
	}
	flat := flattenText(collectTextNodes(root))

	matches := locateQuoteAll(flat, quote)
	if matches == nil {
		return nil
	}
	starts := make([]int, len(matches))
	for i, match := range matches {
		starts[i] = match[0]
	}
	return starts
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

// quoteRegex builds the whitespace-tolerant search pattern both locateQuote
// and locateQuoteAll match with, out of a literal quote: each run of
// whitespace in quote becomes \s* — not just a different run of whitespace,
// but optionally none at all — against the search text. That zero-width
// tolerance is deliberate, not a looser bound picked for safety: increader's
// own multi-block quotes join separate blocks with a blank line for
// readability, but adjacent block elements carry no whitespace between
// their text nodes at the DOM level, so a quote spanning a paragraph break
// would otherwise never match at all.
//
// Factored out into one helper specifically so locateQuote (first match) and
// locateQuoteAll (every match) can never drift into two different notions of
// "matches this quote" — the two are meant to agree on every location a
// quote is found, one just enumerates where locateQuote's own free variable
// choice of "first" landed among.
//
// Returns ok=false for an empty or all-whitespace quote, or a pattern
// regexp.Compile rejects (QuoteMeta escapes everything itself, but the \s*
// substitution runs after escaping and has not been proven to always yield
// something Compile accepts for arbitrary input).
func quoteRegex(quote string) (*regexp.Regexp, bool) {
	trimmed := strings.TrimSpace(quote)
	if trimmed == "" {
		return nil, false
	}
	pattern := regexp.QuoteMeta(trimmed)
	pattern = whitespaceRun.ReplaceAllString(pattern, `\s*`)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false
	}
	return re, true
}

// locateQuote finds quote's [start, end) byte range within flat, the whole
// article's text with no separators inserted between elements — matching
// annotator's own concatenation, which inserts none either. See quoteRegex
// for what "finds" tolerates.
//
// Only the first match is returned: computeRanges, this function's only
// caller, needs exactly one range to serialize, and taking the first is the
// same pragmatic tie-break increader's own paste-to-extract fallback makes
// for the identical ambiguity (see TestComputeRangesFirstOccurrenceWins). A
// caller that instead needs to know about every match — whether the first
// one was even the right choice — wants locateQuoteAll, not this.
func locateQuote(flat, quote string) (start, end int, ok bool) {
	re, ok := quoteRegex(quote)
	if !ok {
		return 0, 0, false
	}
	loc := re.FindStringIndex(flat)
	if loc == nil {
		return 0, 0, false
	}
	return loc[0], loc[1], true
}

// locateQuoteAll is locateQuote's every-match counterpart: every
// non-overlapping [start, end) byte range within flat where quote matches,
// in the same whitespace-tolerant sense locateQuote itself resolves against
// — built from the identical quoteRegex, so the "first match" and "every
// match" questions can never silently disagree about what counts as a match
// for the same quote. That drift — one of the two getting its own,
// independently-typed-out copy of the \s* substitution that later falls out
// of sync with the other — is exactly the bug this shared helper exists to
// make structurally impossible rather than merely unlikely.
//
// Returns nil, not an empty non-nil slice, when quote does not occur at all
// or cannot be turned into a pattern — matching FindAllStringIndex's own
// convention for "no matches", which QuoteOccurrences, this function's
// caller outside this package, in turn also relies on.
func locateQuoteAll(flat, quote string) [][]int {
	re, ok := quoteRegex(quote)
	if !ok {
		return nil
	}
	return re.FindAllStringIndex(flat, -1)
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

// recoverQuote is computeRanges' reverse: given rawHTML (the article content
// exactly as wallabag served it — the same input computeRanges takes) and a
// highlight's own already-resolved ranges, reconstructs the text those
// ranges actually cover.
//
// This is what lets a highlight's real text survive wallabag's own quote
// field being truncated (see maxHighlightQuoteLength): the range itself,
// once resolved, still points at the article's actual, complete text — the
// same text wallabag's own reader highlights when it draws the annotation
// in place, because wallabag draws from the range, not from quote.
//
// Best-effort, matching computeRanges: reports false if rawHTML cannot be
// parsed, ranges is empty, or resolving any single range fails for any
// reason — most likely because the article changed upstream since the
// highlight was made, the same risk increader's own text-based Locate
// already accepts for exactly the same reason.
func recoverQuote(rawHTML string, ranges []serializedRange) (string, bool) {
	if len(ranges) == 0 {
		return "", false
	}

	root := parseBody(rawHTML)
	if root == nil {
		return "", false
	}
	nodes := collectTextNodes(root)

	passages := make([]string, 0, len(ranges))
	for _, r := range ranges {
		passage, ok := recoverOneRange(root, nodes, r)
		if !ok {
			return "", false
		}
		passages = append(passages, passage)
	}
	return stripInvisibleFormatting(strings.Join(passages, " ")), true
}

// recoverOneRange resolves a single range against root — the same <body>
// node parseBody returned in recoverQuote, and the same root xpathTo's paths
// were computed relative to when this range was first serialized. Walking
// up from a node instead, rather than threading root through, would land one
// level too high the moment <body> itself has a parent — <html> — that was
// never part of the path in the first place.
func recoverOneRange(root *html.Node, nodes []*html.Node, r serializedRange) (string, bool) {
	startOffset, err := strconv.Atoi(r.StartOffset)
	if err != nil {
		return "", false
	}
	endOffset, err := strconv.Atoi(r.EndOffset)
	if err != nil {
		return "", false
	}

	startParent, ok := elementAtXPath(root, r.Start)
	if !ok {
		return "", false
	}
	endParent, ok := elementAtXPath(root, r.End)
	if !ok {
		return "", false
	}

	startNode, startByte, ok := resolveBoundary(startParent, startOffset)
	if !ok {
		return "", false
	}
	endNode, endByte, ok := resolveBoundary(endParent, endOffset)
	if !ok {
		return "", false
	}

	return extractBetween(nodes, startNode, startByte, endNode, endByte)
}

// xpathSegmentPattern matches one path segment as xpathTo writes it, e.g.
// "p[2]".
var xpathSegmentPattern = regexp.MustCompile(`^([a-zA-Z0-9]+)\[(\d+)\]$`)

// elementAtXPath is xpathTo's reverse: resolves a "/tag[N]/tag[N]/..." path,
// relative to root, back to the element it names.
func elementAtXPath(root *html.Node, path string) (*html.Node, bool) {
	path = strings.Trim(path, "/")
	if path == "" {
		return root, true
	}

	current := root
	for _, segment := range strings.Split(path, "/") {
		match := xpathSegmentPattern.FindStringSubmatch(segment)
		if match == nil {
			return nil, false
		}
		position, err := strconv.Atoi(match[2])
		if err != nil || position < 1 {
			return nil, false
		}
		next, ok := nthChildWithTag(current, match[1], position)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

// nthChildWithTag is sameTagPosition's reverse: elem's position-th direct
// element child sharing tag, 1-indexed — the same counting rule
// sameTagPosition itself uses, so a path xpathTo wrote resolves back to
// exactly the element it named.
func nthChildWithTag(parent *html.Node, tag string, position int) (*html.Node, bool) {
	count := 0
	for child := parent.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.Data == tag {
			count++
			if count == position {
				return child, true
			}
		}
	}
	return nil, false
}

// resolveBoundary is serializeBoundary's reverse: given the element
// serializeBoundary computed a path to, and the UTF-16 offset into that
// element's own text (its direct and indirect text-node descendants,
// concatenated — the same set collectTextNodes(parent) walks in
// serializeBoundary itself), finds the specific text node and byte offset
// within it that the offset lands on.
func resolveBoundary(parent *html.Node, utf16Offset int) (*html.Node, int, bool) {
	consumed := 0
	for _, node := range collectTextNodes(parent) {
		length := utf16Length(node.Data)
		if utf16Offset <= consumed+length {
			return node, byteOffsetForUTF16(node.Data, utf16Offset-consumed), true
		}
		consumed += length
	}
	return nil, 0, false
}

// byteOffsetForUTF16 converts a UTF-16 code-unit offset into s — the unit
// JavaScript, and so every offset wallabag ever recorded, counts a string's
// length in — into the byte offset Go's own UTF-8 slicing needs. The reverse
// of utf16Length applied to a prefix of s.
func byteOffsetForUTF16(s string, target int) int {
	units := 0
	for i, r := range s {
		if units >= target {
			return i
		}
		if r > 0xFFFF {
			units += 2 // outside the BMP: a surrogate pair in UTF-16
		} else {
			units++
		}
	}
	return len(s)
}

// blockAncestorTags are the elements extractBetween treats as separating two
// pieces of text enough to need a space between them when they are rejoined
// — a deliberate, independent approximation of ir.blockTags, not an import
// of it: this package stays free of increader's own reading model, the same
// reason computeRanges takes rawHTML instead of reaching for ir.Article's
// parsed, sanitised view of the same content.
var blockAncestorTags = map[atom.Atom]bool{
	atom.P: true, atom.Li: true, atom.Blockquote: true, atom.Pre: true,
	atom.H1: true, atom.H2: true, atom.H3: true,
	atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Dd: true, atom.Dt: true, atom.Td: true, atom.Th: true,
	atom.Figcaption: true, atom.Div: true, atom.Section: true, atom.Article: true,
}

// nearestBlockAncestor walks up from node to the nearest ancestor
// extractBetween treats as a block boundary, or as far up as there is to go
// if none of node's ancestors are one.
func nearestBlockAncestor(node *html.Node) *html.Node {
	n := node.Parent
	for n != nil && !(n.Type == html.ElementNode && blockAncestorTags[n.DataAtom]) {
		if n.Parent == nil {
			return n
		}
		n = n.Parent
	}
	return n
}

// extractBetween concatenates the text nodes package nodes covers from
// (startNode, startByte) through (endNode, endByte) inclusive of both
// boundaries, in the document order nodes is already in.
//
// A space is inserted between two consecutive nodes whenever their nearest
// block ancestors differ. Adjacent block-level siblings — two paragraphs the
// highlight spans, most importantly — carry no whitespace between their text
// nodes at the DOM level at all (the same fact
// TestComputeRangesAcrossParagraphsWithNoGap exists to cover on the way
// out), so without this a recovered multi-paragraph passage would run its
// paragraphs together with nothing between them: text Article.Locate, which
// this feeds, could then never match, since Locate's own search space always
// has exactly one space at a block boundary.
//
// Comparing the nearest *block* ancestor rather than the immediate parent —
// confirmed against a real highlight crossing a "2002<sub>24ya</sub>"-style
// annotation, adjacent inline markup with deliberately no space at all — is
// what keeps that case from getting the same treatment: "2002" and "24ya"
// sit in different immediate elements but the same enclosing paragraph, and
// the immediate-parent version of this inserted a space between them that
// broke every match past that point, since one extra character early in the
// recovered text derails an exact substring search for everything after it.
func extractBetween(nodes []*html.Node, startNode *html.Node, startByte int, endNode *html.Node, endByte int) (string, bool) {
	var (
		b          strings.Builder
		collecting bool
		previous   *html.Node
	)
	for _, node := range nodes {
		if node == startNode {
			collecting = true
		}
		if !collecting {
			continue
		}
		if previous != nil && nearestBlockAncestor(previous) != nearestBlockAncestor(node) {
			b.WriteByte(' ')
		}
		previous = node

		switch {
		case node == startNode && node == endNode:
			b.WriteString(node.Data[startByte:endByte])
			return b.String(), true
		case node == startNode:
			b.WriteString(node.Data[startByte:])
		case node == endNode:
			b.WriteString(node.Data[:endByte])
			return b.String(), true
		default:
			b.WriteString(node.Data)
		}
	}
	// startNode was never seen, or endNode never followed it — most likely a
	// range whose end comes before its start, or a boundary from a
	// different tree entirely.
	return "", false
}

// QuoteAnchored reports whether ranges still resolve against rawHTML to text
// matching quote — the check a caller outside this package needs before
// trusting that a highlight's already-stored location still points at the
// right passage, rather than at whatever the source article now happens to
// have in that spot after an upstream edit. It is recoverQuote plus exactly
// one more question: recoverQuote alone reports what text a range resolves
// to today, saying nothing about whether that is still the text anyone
// actually meant to highlight.
//
// ranges is json.RawMessage, not []serializedRange, because that is the
// exact shape a caller holding a source.Highlight has to give: see that
// field's own comment in source.go for why it is carried opaquely rather
// than as a typed slice outside this package in the first place.
//
// Comparison is whitespace-tolerant rather than byte-exact, deliberately: a
// quote as a caller holds it may have travelled through storage or been
// reformatted since it was first extracted, and increader's own multi-block
// quotes join separate blocks with a blank line that has no counterpart at
// the DOM level at all (the same gap locateQuote's own \s* tolerance exists
// to cross). None of that reflects the article having actually changed
// underneath the highlight, so a byte-exact comparison would report a
// perfectly still-anchored highlight as having moved for no reason but its
// own formatting history. Fields+Join is a coarser normalisation than
// locateQuote's regex — it cannot tolerate a whitespace run collapsing to
// zero characters at a block boundary the way locateQuote can — but nothing
// here needs that: unlike locateQuote, this is not searching for quote
// inside a larger text, it is comparing two already-delimited strings for
// equivalence, where collapsing all whitespace runs to a single space is
// sufficient and simpler.
//
// A quote wallabag truncated to maxHighlightQuoteLength bytes and suffixed
// with "…" (see truncateQuote) is handled explicitly: recovered text that
// merely starts with the truncated quote's own body is still reported as
// anchored. A truncated quote can never equal the full text a resolved range
// recovers — it is missing everything past the cut, by construction — so
// without this every highlight long enough to have been truncated would be
// reported as unanchored regardless of whether it had actually moved at all,
// which is exactly backwards for the callers this exists to serve.
//
// Malformed JSON, an empty ranges array, or a resolution that fails all
// report false rather than panicking or returning an error: recoverQuote and
// the xpath-resolution it calls into already fail closed on a malformed or
// stale range for the same reason (the article having changed, or the range
// simply never having been valid), and this adds nothing past their own
// best-effort convention beyond the final text comparison.
func QuoteAnchored(rawHTML string, ranges json.RawMessage, quote string) bool {
	if len(ranges) == 0 {
		return false
	}
	var parsed []serializedRange
	if err := json.Unmarshal(ranges, &parsed); err != nil {
		return false
	}

	recovered, ok := recoverQuote(rawHTML, parsed)
	if !ok {
		return false
	}

	normalize := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	recoveredNorm := normalize(recovered)
	quoteNorm := normalize(quote)

	if recoveredNorm == quoteNorm {
		return true
	}

	if body, wasTruncated := strings.CutSuffix(quoteNorm, "…"); wasTruncated && body != "" {
		return strings.HasPrefix(recoveredNorm, body)
	}
	return false
}
