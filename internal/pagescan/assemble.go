package pagescan

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// paragraphMark is the character the prompt asks the model to prefix a
// paragraph-opening line with, so Assemble can tell "a new paragraph
// starts here" apart from "this line just happens to start after a hard
// line break" without any layout information beyond the line text itself.
const paragraphMark = "¶"

// terminalChars, closerChars and openerChars back the sentence-boundary
// regexes below. A "terminal" ends a sentence; a run of closerChars
// (closing quotes/brackets) can trail it without moving the boundary; an
// opener can lead the next sentence when we're looking for where one
// starts.
const (
	terminalChars = ".?!…"
	closerChars   = "\"”’)]"
	openerChars   = "\"“‘(["
)

// terminalPattern matches one terminal mark plus its full run of trailing
// closers — used both to detect "this line ends a sentence" (endOfLineRe)
// and, character class only, to drive the hand-rolled scans below that
// stand in for the lookaheads Python's re module has and Go's RE2-based
// regexp package does not.
const terminalPattern = `[.?!…]["”’)\]]*`

var endOfLineRe = regexp.MustCompile(terminalPattern + `$`)

// endsSentence reports whether line's own trimmed text ends a sentence: a
// terminal mark (. ? ! …) optionally followed by closing quotes/brackets,
// at the very end of the line.
func endsSentence(line string) bool {
	return endOfLineRe.MatchString(strings.TrimSpace(line))
}

// isTerminal, isCloser and isOpener classify single runes against the
// character classes above.
func isTerminal(r rune) bool { return strings.ContainsRune(terminalChars, r) }
func isCloser(r rune) bool   { return strings.ContainsRune(closerChars, r) }
func isOpener(r rune) bool   { return strings.ContainsRune(openerChars, r) }

// Go note: Python's START/END regexes use a lookahead, (?=\s|$) and
// (?=["“‘(\[]?[A-Z]), to test what follows a terminal without consuming it.
// RE2 (Go's regexp package) deliberately has no lookahead — it trades that
// expressiveness for a guarantee that every match runs in linear time. So
// sentenceEnds and sentenceStart below match only the terminal-plus-closers
// with a regexp, then walk the following runes by hand to apply the same
// test Python's lookahead would. Both also redo, in a small loop, the
// backtracking a real regex engine would do on the closer run: a greedy
// match of every closer eats too much when the *next* character doesn't
// qualify (e.g. `.")x` with no space before the x), so on failure they
// retry with one fewer trailing closer, down to the bare terminal mark,
// before giving up on that position and moving on.

// sentenceEnds returns, in left-to-right order, the byte offsets in text
// right after each point a sentence ends — mirroring Python's
// END.finditer(text), one non-overlapping match per position.
func sentenceEnds(text string) []int {
	var ends []int
	pos := 0
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !isTerminal(r) {
			pos += size
			continue
		}
		closerEnd := closerRunEnd(text, pos+size)
		accepted, ok := shrinkToAccepted(text, pos+size, closerEnd, func(end int) (int, bool) {
			if end == len(text) || isSpaceAt(text, end) {
				return end, true
			}
			return 0, false
		})
		if ok {
			ends = append(ends, accepted)
			pos = accepted
		} else {
			pos += size
		}
	}
	return ends
}

// sentenceStart finds the first point in text where a new sentence begins
// after some terminal: a terminal mark (plus closers) followed by
// whitespace, followed by an optional opening quote/bracket and an
// upper-case letter. It returns the byte offset of that opener/letter —
// where the clean sentence actually begins — mirroring Python's
// START.search(text).end().
func sentenceStart(text string) (int, bool) {
	pos := 0
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !isTerminal(r) {
			pos += size
			continue
		}
		closerEnd := closerRunEnd(text, pos+size)
		if cut, ok := shrinkToAccepted(text, pos+size, closerEnd, func(end int) (int, bool) {
			return afterSentenceStart(text, end)
		}); ok {
			return cut, true
		}
		pos += size
	}
	return 0, false
}

// closerRunEnd returns the byte offset right after the longest run of
// closerChars starting at from.
func closerRunEnd(text string, from int) int {
	end := from
	for end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		if !isCloser(r) {
			break
		}
		end += size
	}
	return end
}

// shrinkToAccepted tries accept(closerEnd), then accept at each shorter
// closer run down to accept(bareEnd), returning the first non-empty result
// accept produces — the backtracking described above.
func shrinkToAccepted(text string, bareEnd, closerEnd int, accept func(end int) (int, bool)) (int, bool) {
	end := closerEnd
	for {
		if cut, ok := accept(end); ok {
			return cut, true
		}
		if end <= bareEnd {
			return 0, false
		}
		_, size := utf8.DecodeLastRuneInString(text[:end])
		end -= size
	}
}

func isSpaceAt(text string, pos int) bool {
	r, _ := utf8.DecodeRuneInString(text[pos:])
	return unicode.IsSpace(r)
}

// afterSentenceStart checks whether text[from:] begins with whitespace,
// then an optional opener, then an upper-case letter, returning the offset
// of that opener/letter.
func afterSentenceStart(text string, from int) (int, bool) {
	if from >= len(text) {
		return 0, false
	}
	r, size := utf8.DecodeRuneInString(text[from:])
	if !unicode.IsSpace(r) {
		return 0, false
	}
	ws := from + size
	for ws < len(text) {
		wr, wsize := utf8.DecodeRuneInString(text[ws:])
		if !unicode.IsSpace(wr) {
			break
		}
		ws += wsize
	}
	if ws >= len(text) {
		return 0, false
	}
	cur, curSize := utf8.DecodeRuneInString(text[ws:])
	if isOpener(cur) {
		if ws+curSize >= len(text) {
			return 0, false
		}
		next, _ := utf8.DecodeRuneInString(text[ws+curSize:])
		if unicode.IsUpper(next) {
			return ws, true
		}
		return 0, false
	}
	if unicode.IsUpper(cur) {
		return ws, true
	}
	return 0, false
}

// trimStart drops everything before the first sentence start in text,
// unchanged if there is none.
func trimStart(text string) string {
	if cut, ok := sentenceStart(text); ok {
		return text[cut:]
	}
	return text
}

// trimEnd drops everything after the last sentence end in text, unchanged
// if there is none.
func trimEnd(text string) string {
	ends := sentenceEnds(text)
	if len(ends) == 0 {
		return text
	}
	return text[:ends[len(ends)-1]]
}

var wordBeforeHyphenRe = regexp.MustCompile(`([\p{L}\p{N}_]+)-$`)
var wordAfterRe = regexp.MustCompile(`^[\p{L}\p{N}_]+`)

// Go note: Python's \w is Unicode-aware by default; Go's regexp \w is
// ASCII-only. [\p{L}\p{N}_] is the Unicode-aware equivalent Python's \w
// actually matches, so wordBeforeHyphenRe/wordAfterRe use that instead.

func wordBeforeHyphen(line string) string {
	m := wordBeforeHyphenRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}

func wordAfter(s string) string {
	return wordAfterRe.FindString(s)
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func startsLower(s string) bool {
	if s == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsLower(r)
}

func firstRuneUpper(s string) bool {
	if s == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsUpper(r)
}

// glue joins two consecutive lines of a fragment. A hyphen ending the left
// line might be nothing but a line-break artifact ("educa-" / "tion") or it
// might belong to the word itself ("long-" / "term"); the only signal we
// have to tell them apart is whether the model's own transcription of the
// passage (which was built by an eye that can see the page, not just read
// the line list) spells the joined word with a hyphen. Kept only then;
// dropped otherwise.
func glue(left, right, modelText string) string {
	if strings.HasSuffix(left, "-") && startsLower(right) {
		compound := wordBeforeHyphen(left) + "-" + wordAfter(right)
		if compound != "-" && strings.Contains(collapseWhitespace(modelText), compound) {
			return left + right
		}
		return left[:len(left)-1] + right
	}
	return left + " " + right
}

// romanValues maps a lower-case Roman numeral digit to its value.
var romanValues = map[byte]int{'i': 1, 'v': 5, 'x': 10, 'l': 50, 'c': 100, 'd': 500, 'm': 1000}

// romanValue parses s (already known non-empty) as a subtractive-notation
// Roman numeral, returning ok=false if any character isn't a Roman digit.
func romanValue(s string) (int, bool) {
	s = strings.ToLower(s)
	for i := 0; i < len(s); i++ {
		if _, ok := romanValues[s[i]]; !ok {
			return 0, false
		}
	}
	total := 0
	for i := 0; i < len(s); i++ {
		v := romanValues[s[i]]
		if i+1 < len(s) && romanValues[s[i+1]] > v {
			total -= v
		} else {
			total += v
		}
	}
	return total, true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// pageNumber turns a printed page label into a sort key and a page number.
// key orders front-matter Roman numerals ahead of the Arabic-numbered body
// (Roman values are small; Arabic labels get 10000 added) so "xii" sorts
// before "1" the way it does in the physical book. ok is false for an
// empty, unread, or otherwise unparseable label — the caller then treats
// the page as "unlabelled" (see loadPages).
func pageNumber(label string) (key, number int, ok bool) {
	label = strings.TrimSpace(label)
	if label == "" {
		return 0, 0, false
	}
	if isAllDigits(label) {
		n, err := strconv.Atoi(label)
		if err != nil {
			return 0, 0, false
		}
		return 10000 + n, n, true
	}
	if r, isRoman := romanValue(label); isRoman {
		return r, r, true
	}
	return 0, 0, false
}

// orderedPage is one page of one scan, positioned into the book's overall
// page order.
type orderedPage struct {
	scanID    int64
	pageIndex int
	label     string // trimmed printed label; "" if none/unread/unparseable
	key       int    // sort key; shared with the most recent labelled page when half
	number    int    // meaningful only when hasNumber
	hasNumber bool   // this page's own label parsed to a number
	half      bool   // true when this page had to borrow a neighbour's key
	page      Page
	group     int    // chapter group, set by chapterGroups
	evenHead  string // running head last seen on an even page in this group
	oddHead   string // running head last seen on an odd page in this group
}

// pageRefLabel is the label RefPrefix should use for p: its own printed
// label when it could be placed by page number, "" (forcing the
// scan-id-based form) otherwise. Both Pages and Assemble reduce a page to
// this same label before building a ref, so the two packages of code never
// disagree about what a given page's ref looks like.
func pageRefLabel(p orderedPage) string {
	if p.half {
		return ""
	}
	return p.label
}

// loadPages flattens every scan's pages into book order: sorted by page
// number, with unlabelled pages slotted in just after the most recent
// labelled page that precedes them in upload order, and a retaken page
// (the same label photographed twice) collapsed to only its last shot.
func loadPages(scans []Scan) []orderedPage {
	var pages []orderedPage
	for _, scan := range scans {
		for pageIndex, page := range scan.Result.Pages {
			label := strings.TrimSpace(page.Label)
			key, number, ok := pageNumber(label)
			pages = append(pages, orderedPage{
				scanID: scan.ID, pageIndex: pageIndex, label: label,
				key: key, number: number, hasNumber: ok, page: page,
			})
		}
	}

	// Unlabelled pages sit just after the labelled page that precedes them
	// in upload order (scan id, then position in the photo).
	lastKey := 0
	for i := range pages {
		if pages[i].hasNumber {
			lastKey = pages[i].key
			pages[i].half = false
		} else {
			pages[i].key = lastKey
			pages[i].half = true
		}
	}

	// Retakes: the same labelled page photographed twice — the later scan
	// wins. by_label indexes the last (in upload order) non-half page seen
	// for each label; a non-half page survives only if it is that one.
	byLabel := make(map[string]int)
	for i := range pages {
		if pages[i].label != "" && !pages[i].half {
			byLabel[pages[i].label] = i
		}
	}
	kept := pages[:0:0]
	for i := range pages {
		if pages[i].half || byLabel[pages[i].label] == i {
			kept = append(kept, pages[i])
		}
	}
	pages = kept

	sort.SliceStable(pages, func(a, b int) bool {
		pa, pb := pages[a], pages[b]
		if pa.key != pb.key {
			return pa.key < pb.key
		}
		if pa.half != pb.half {
			return pb.half // false sorts before true
		}
		if pa.scanID != pb.scanID {
			return pa.scanID < pb.scanID
		}
		return pa.pageIndex < pb.pageIndex
	})
	return pages
}

// smallWords are the words titleCase keeps lower-case when they fall in
// the middle of a heading, following the usual title-case convention.
var smallWords = map[string]bool{
	"a": true, "an": true, "and": true, "as": true, "at": true, "but": true,
	"by": true, "for": true, "from": true, "in": true, "nor": true, "of": true,
	"on": true, "or": true, "the": true, "to": true, "via": true, "vs": true,
	"with": true,
}

// titleCase renders a running head for display. A head that isn't entirely
// upper-case is assumed to already be printed the way it should read (a
// book that prints "Chapter Two" doesn't need help); an all-caps head
// (most running heads are set in small caps/caps on the page) is
// lower-cased and re-capitalised, leaving small words lower-case unless
// they open or close the head.
func titleCase(head string) string {
	words := strings.Fields(head)
	if head != strings.ToUpper(head) {
		return strings.Join(words, " ")
	}
	out := make([]string, len(words))
	for i, w := range words {
		lw := strings.ToLower(w)
		if i > 0 && i < len(words)-1 && smallWords[lw] {
			out[i] = lw
		} else {
			out[i] = capitalizeFirst(lw)
		}
	}
	return strings.Join(out, " ")
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

var chapterHeadRe = regexp.MustCompile(`(?i)^chapter\s+(\S+)$`)

// deriveChapterName names a chapter group from its even (verso) and odd
// (recto) running heads. Books commonly print the chapter number on one
// parity of page and the chapter title on the other; when the even head
// reads like "Chapter N" and an odd head is present, that combination
// wins. Otherwise whichever head is present carries the name; a group with
// no head at all (never containing a numbered, headed page — e.g. every
// page in it was unlabelled and skipped) gets "".
func deriveChapterName(evenHead, oddHead string) string {
	if m := chapterHeadRe.FindStringSubmatch(evenHead); m != nil && oddHead != "" {
		return "Chapter " + m[1] + ": " + titleCase(oddHead)
	}
	if oddHead != "" {
		return titleCase(oddHead)
	}
	if evenHead != "" {
		return titleCase(evenHead)
	}
	return ""
}

// chapterGroups walks pages in book order, assigning each a chapter group
// (writing p.group/evenHead/oddHead in place) and returns each group's
// derived chapter name.
//
// A page with no page number or a blank running head is skipped (grouped
// with whatever came before it) — a running head is only trustworthy on a
// page the model could place. Otherwise, a chapter boundary falls exactly
// where a same-parity running head changes: the reader only photographs
// marked pages, so a book's constant verso title ("never differs, so never
// closes a group") pairs with a recto head that changes once per chapter to
// produce one group per chapter, which is the reason this needs no
// heading/outline data at all.
func chapterGroups(pages []orderedPage) map[int]string {
	group := 0
	var even, odd string
	for i := range pages {
		p := &pages[i]
		head := collapseWhitespace(p.page.RunningHead)
		if !p.hasNumber || head == "" {
			p.group = group
			continue
		}
		if p.number%2 == 0 {
			if even != "" && head != even {
				group++
				odd = ""
			}
			even = head
		} else {
			if odd != "" && head != odd {
				group++
				even = ""
			}
			odd = head
		}
		p.group = group
		p.evenHead, p.oddHead = even, odd
	}

	type heads struct{ even, odd string }
	byGroup := make(map[int]*heads)
	for i := range pages {
		p := pages[i]
		gh, ok := byGroup[p.group]
		if !ok {
			gh = &heads{}
			byGroup[p.group] = gh
		}
		if p.evenHead != "" {
			gh.even = p.evenHead
		}
		if p.oddHead != "" {
			gh.odd = p.oddHead
		}
	}
	names := make(map[int]string, len(byGroup))
	for g, gh := range byGroup {
		names[g] = deriveChapterName(gh.even, gh.odd)
	}
	return names
}

// fragment is one mark's text and the boundary information the join/trim
// passes need, built from one page's already-numbered lines.
type fragment struct {
	pageIdx        int // index into the pages slice this fragment belongs to
	index          int // position within that page's own Marks array
	text           string
	startsMid      bool
	endsMid        bool
	atTop          bool
	atBottom       bool
	hasHandwriting bool
	modelText      string
}

// buildFragment turns one mark on one page into a fragment: the marked
// lines glued into text, clamped and order-corrected against the page's
// actual line count (a model can report a range slightly off, or
// back-to-front), plus whether the fragment's own first/last line already
// reads as a clean sentence boundary.
func buildFragment(pageIdx int, page Page, mark Mark, index int) (fragment, bool) {
	n := len(page.Lines)
	isPara := make([]bool, n)
	lines := make([]string, n)
	for i, l := range page.Lines {
		trimmed := strings.TrimSpace(l)
		isPara[i] = strings.HasPrefix(trimmed, paragraphMark)
		lines[i] = strings.TrimSpace(strings.TrimLeft(trimmed, paragraphMark))
	}

	f := clampLine(mark.FirstLine, n) - 1
	l := clampLine(mark.LastLine, n) - 1
	if l < f {
		f, l = l, f
	}

	// A bracket's top tick often sits level with a section heading, and the
	// model then sometimes counts the heading into the range, or reports the
	// tick as a mark of its own. A heading is never part of a passage, so
	// heading lines are dropped from either end of the range, and a mark
	// that covered nothing else is no passage at all. Left in, "1. Han Feizi
	// on Law" was trimmed to its last "sentence end" and imported as "1.".
	headings := headingSet(page.Headings)
	for f <= l && headings[collapseWhitespace(lines[f])] {
		f++
	}
	for l >= f && headings[collapseWhitespace(lines[l])] {
		l--
	}
	if f > l {
		return fragment{}, false
	}

	// A highlight, underline or circle covers exactly the words it covers,
	// which only the model's own reading of the passage records — lines
	// are whole lines. So for those the model's text is the passage, and
	// the sentence snapping and page-turn joining below, which exist for a
	// bracket's whole-sentence meaning, do not apply.
	if exactKinds[mark.Kind] {
		if text := collapseWhitespace(mark.Text); text != "" {
			return fragment{
				pageIdx:        pageIdx,
				index:          index,
				text:           text,
				atTop:          f == 0,
				atBottom:       l == n-1,
				hasHandwriting: mark.HasHandwriting,
				modelText:      mark.Text,
			}, true
		}
	}

	text := lines[f]
	for i := f + 1; i <= l; i++ {
		if isPara[i] {
			text = text + "\n\n" + lines[i]
		} else {
			text = glue(text, lines[i], mark.Text)
		}
	}

	// A line straight after a heading begins a sentence even when the model
	// did not mark it as a new paragraph.
	startsClean := isPara[f] ||
		(f > 0 && (endsSentence(lines[f-1]) || headings[collapseWhitespace(lines[f-1])])) ||
		(f == 0 && firstRuneUpper(lines[0]))
	return fragment{
		pageIdx:        pageIdx,
		index:          index,
		text:           text,
		startsMid:      !startsClean,
		endsMid:        !endsSentence(lines[l]),
		atTop:          f == 0,
		atBottom:       l == n-1,
		hasHandwriting: mark.HasHandwriting,
		modelText:      mark.Text,
	}, true
}

// exactKinds are the mark kinds that cover exactly the words they touch,
// rather than whole sentences the way a margin bracket or line does.
var exactKinds = map[string]bool{"highlight": true, "underline": true, "circle": true}

// headingSet collects a page's headings, whitespace-collapsed, for comparing
// against its lines.
func headingSet(headings []string) map[string]bool {
	set := make(map[string]bool, len(headings))
	for _, heading := range headings {
		if squashed := collapseWhitespace(heading); squashed != "" {
			set[squashed] = true
		}
	}
	return set
}

func clampLine(v, n int) int {
	if v < 1 {
		v = 1
	}
	if v > n {
		v = n
	}
	return v
}

// gatherFragments builds every fragment across pages, in book order; a
// page with no transcribed lines contributes none of its marks (there's
// nothing to snap the mark's range against). Mark index i is always the
// mark's own position within its page's Marks array, so a ref built from
// it stays stable regardless of what any other page contributes.
//
// A mark whose first or last line is 0 is skipped rather than clamped to
// line 1: line numbers count from one, so 0 is what a missing or null value
// decodes to, and clamping it would invent a passage starting at the top of
// the page that the reader never marked. Out-of-range values that are
// present (a model counting one line past the end) are still clamped, in
// buildFragment.
func gatherFragments(pages []orderedPage) []fragment {
	var frags []fragment
	for i, p := range pages {
		if len(p.page.Lines) == 0 {
			continue
		}
		for markIdx, mark := range p.page.Marks {
			if mark.FirstLine == 0 || mark.LastLine == 0 {
				continue
			}
			frag, ok := buildFragment(i, p.page, mark, markIdx)
			if !ok {
				continue
			}
			frags = append(frags, frag)
		}
	}
	return frags
}

// passageBuild is a Passage under construction, still carrying its chapter
// group id (resolved to a name only once every passage has been built).
type passageBuild struct {
	ref          string
	absorbedRefs []string
	page         string
	ordinal      int
	text         string
	notePending  bool
	group        int
	scanIDs      []int64
}

// Assemble turns every finished scan of one book into its passages.
//
// existing maps an already-stored passage's external ref to its current
// chapter, so a chapter the reader renamed by hand is inherited by any new
// passage Assemble now places in the same chapter group (rather than
// reverting to the running-head-derived name on every re-Assemble). nil is
// fine — a book with nothing stored yet has nothing to inherit.
func Assemble(scans []Scan, existing map[string]string) []Passage {
	pages := loadPages(scans)
	names := chapterGroups(pages)
	frags := gatherFragments(pages)

	var built []passageBuild
	consumed := make(map[int]bool, len(frags))
	for k := 0; k < len(frags); k++ {
		if consumed[k] {
			continue
		}
		parts := []int{k}
		// Join forward while the current last piece runs off the bottom of
		// its page mid-sentence and the very next fragment in the list
		// opens the very next page, at its top, also mid-sentence — a
		// passage the reader bracketed across a page break.
		for {
			last := frags[parts[len(parts)-1]]
			lastPage := pages[last.pageIdx]
			if !(last.atBottom && last.endsMid) || k+len(parts) >= len(frags) {
				break
			}
			nextIdx := k + len(parts)
			next := frags[nextIdx]
			nextPage := pages[next.pageIdx]
			adjacent := lastPage.hasNumber && nextPage.number == lastPage.number+1 &&
				!nextPage.half && next.index == 0
			if !(adjacent && next.atTop && next.startsMid) {
				break
			}
			parts = append(parts, nextIdx)
		}
		for _, idx := range parts[1:] {
			consumed[idx] = true
		}

		first := frags[parts[0]]
		last := frags[parts[len(parts)-1]]
		firstPage := pages[first.pageIdx]
		lastPage := pages[last.pageIdx]

		text := first.text
		for _, idx := range parts[1:] {
			next := frags[idx]
			text = glue(text, next.text, first.modelText+" "+next.modelText)
		}
		if first.startsMid {
			text = trimStart(text)
		}
		if last.endsMid {
			text = trimEnd(text)
		}

		absorbed := make([]string, 0, len(parts)-1)
		scanIDSet := make(map[int64]bool, len(parts))
		notePending := false
		for _, idx := range parts {
			f := frags[idx]
			p := pages[f.pageIdx]
			scanIDSet[p.scanID] = true
			if f.hasHandwriting {
				notePending = true
			}
			if idx != parts[0] {
				absorbed = append(absorbed, RefPrefix(p.scanID, p.pageIndex, pageRefLabel(p))+":m"+strconv.Itoa(f.index))
			}
		}
		scanIDs := make([]int64, 0, len(scanIDSet))
		for id := range scanIDSet {
			scanIDs = append(scanIDs, id)
		}
		sort.Slice(scanIDs, func(i, j int) bool { return scanIDs[i] < scanIDs[j] })

		page := firstPage.label
		if first.pageIdx != last.pageIdx {
			page = firstPage.label + "–" + lastPage.label
		}
		halfBonus := 0
		if firstPage.half {
			halfBonus = 500
		}

		built = append(built, passageBuild{
			ref:          RefPrefix(firstPage.scanID, firstPage.pageIndex, pageRefLabel(firstPage)) + ":m" + strconv.Itoa(first.index),
			absorbedRefs: absorbed,
			page:         page,
			ordinal:      firstPage.key*1000 + halfBonus + first.index,
			text:         strings.TrimSpace(text),
			notePending:  notePending,
			group:        firstPage.group,
			scanIDs:      scanIDs,
		})
	}

	// Chapter: a group inherits the current chapter of whichever
	// already-stored passage in it has the lowest ordinal, else takes the
	// derived name.
	order := make([]int, len(built))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return built[order[a]].ordinal < built[order[b]].ordinal })
	chapterOfGroup := make(map[int]string)
	for _, idx := range order {
		ps := built[idx]
		if ch, ok := existing[ps.ref]; ok && ch != "" {
			if _, has := chapterOfGroup[ps.group]; !has {
				chapterOfGroup[ps.group] = ch
			}
		}
	}

	result := make([]Passage, len(built))
	for i, ps := range built {
		chapter, ok := chapterOfGroup[ps.group]
		if !ok {
			chapter = names[ps.group]
		}
		result[i] = Passage{
			Ref:          ps.ref,
			AbsorbedRefs: ps.absorbedRefs,
			Page:         ps.page,
			Ordinal:      ps.ordinal,
			Text:         ps.text,
			NotePending:  ps.notePending,
			Chapter:      chapter,
			ScanIDs:      ps.scanIDs,
		}
	}
	return result
}
