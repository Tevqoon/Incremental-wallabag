// Package pagescan turns a photograph of a pencil-marked book page into
// finished passages.
//
// The reader photographs a page (or an open two-page spread); the photo goes
// to a vision model (via OpenRouter, see client.go) that transcribes every
// printed line, numbered, and reports which line range each margin mark
// covers. Deciding exactly which words a bracket or underline means — snap
// to sentence boundaries, join a passage that runs off the bottom of one
// page onto the top of the next, dedupe a retaken photo, group pages into
// chapters by their running heads — is then done deterministically here, in
// Assemble. That split is load-bearing, not a style choice: against
// hand-read ground truth, letting the model choose the line range and doing
// the snapping in code was the only arrangement that got every test passage
// exactly right, and it is why Assemble exists as a pure function you can
// golden-test against real scans instead of trusting the model to get
// sentence boundaries right on its own.
//
// A dependency-free leaf, like internal/proofread: nothing here knows about
// SQLite, HTTP handlers, or increader's own "element" concept. A Result is
// only ever a stored or freshly-returned model response; a Scan pairs one
// with the store id it belongs to; Assemble turns a book's worth of Scans
// into Passages ready for the store to save. The store, not this package,
// decides which photos count as "finished" and feeds them in.
package pagescan

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

//go:embed prompt.txt
var prompt string

// Mark is one reader-marked passage on one page, exactly as the vision
// model reports it — the input to fragment-building in assemble.go.
type Mark struct {
	FirstLine             int    `json:"first_line"`
	LastLine              int    `json:"last_line"`
	Text                  string `json:"text"`
	Kind                  string `json:"kind"`
	HasHandwriting        bool   `json:"has_handwriting"`
	ContinuesFromPrevious bool   `json:"continues_from_previous_page"`
	ContinuesToNext       bool   `json:"continues_to_next_page"`
}

// Page is one page of one photo, as the model transcribed it. Label and
// RunningHead decode a JSON null to "" for free: encoding/json leaves a
// string field at its zero value when the source value is null, so no
// custom UnmarshalJSON is needed for that.
type Page struct {
	Label       string   `json:"page_label"`
	RunningHead string   `json:"running_head"`
	Headings    []string `json:"headings"`
	Lines       []string `json:"lines"`
	Marks       []Mark   `json:"marks"`
}

// Result is one photo's full model response.
type Result struct {
	Pages []Page `json:"pages"`
}

// Scan is one finished photo: the store id it is saved under, and its
// parsed model response. Assemble takes a book's Scans in upload order —
// that order is significant, it is how a retake ("the same page,
// photographed again because the first shot was blurry") is told apart
// from a genuinely new page.
type Scan struct {
	ID     int64
	Result Result
}

// Passage is one assembled passage, ready for the store to save.
//
// Ref is a stable external id built from the page's own printed label
// ("scan:p140:m0") or, when the model could not read a label, from the scan
// and page position instead ("scan:s12.0:m1") — stable across re-Assemble
// as long as the underlying scan isn't edited, which is what lets the store
// diff a new Assemble run against what it already has instead of treating
// every run as a clean slate.
type Passage struct {
	Ref          string
	AbsorbedRefs []string
	Page         string
	Ordinal      int
	Text         string
	NotePending  bool
	Chapter      string
	ScanIDs      []int64
}

// PageInfo describes one page after ordering and retake de-duplication —
// what Pages returns, for the web layer to show which photo holds which
// page without re-running the whole of Assemble.
type PageInfo struct {
	ScanID      int64
	PageIndex   int
	Label       string
	RefPrefix   string
	RunningHead string
	Headings    []string
	MarkCount   int

	// FirstLine is the page's first printed line, without the paragraph
	// mark — how the reader finds, in the book in front of them, a page the
	// model could not number. The photo itself is not kept.
	FirstLine string
}

// codeFence strips a ```json ... ``` wrapper some models add despite being
// asked for strict JSON — the same defensive unwrap internal/proofread
// applies to its own model responses.
var codeFence = regexp.MustCompile("^```[a-zA-Z]*\\n?|```$")

// ParseResult decodes a stored or freshly returned model response. It is an
// error if raw is not JSON at all, or is JSON with no "pages" key — a
// response missing the key entirely is a malformed answer, but an empty
// "pages" array is a legitimate one (a photo with no page fully in frame).
func ParseResult(raw string) (Result, error) {
	content := strings.TrimSpace(codeFence.ReplaceAllString(strings.TrimSpace(raw), ""))

	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &probe); err != nil {
		return Result{}, fmt.Errorf("pagescan: parse model response: %w", err)
	}
	if _, ok := probe["pages"]; !ok {
		return Result{}, fmt.Errorf("pagescan: model response has no \"pages\" key")
	}

	var result Result
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return Result{}, fmt.Errorf("pagescan: parse model response: %w", err)
	}
	return result, nil
}

// SetPageLabel rewrites one page's page_label inside a stored raw result —
// the reader correcting a misread page number, or supplying one the model
// couldn't find, by hand. It round-trips through Result, so any field the
// model returned that Result doesn't model (there are none today, but a
// future prompt change might add one) would be silently dropped; that is
// judged acceptable here because a page_label edit is the only write this
// package makes to an already-stored scan.
func SetPageLabel(raw string, pageIndex int, label string) (string, error) {
	result, err := ParseResult(raw)
	if err != nil {
		return "", err
	}
	if pageIndex < 0 || pageIndex >= len(result.Pages) {
		return "", fmt.Errorf("pagescan: page index %d out of range (result has %d pages)", pageIndex, len(result.Pages))
	}
	result.Pages[pageIndex].Label = label

	encoded, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("pagescan: encode result: %w", err)
	}
	return string(encoded), nil
}

// RefPrefix is the first half of a passage/page ref, shared between Assemble
// and Pages so the two always agree on what a given page's ref looks like.
// label must be the page's own printed label with "" standing for "this
// page could not be placed by page number" (an unread label, or one that
// didn't parse as a number — see pageNumber in assemble.go) rather than
// literally whatever page_label held; Pages and Assemble both already make
// that reduction before calling this.
func RefPrefix(scanID int64, pageIndex int, label string) string {
	if label != "" {
		return "scan:p" + label
	}
	return fmt.Sprintf("scan:s%d.%d", scanID, pageIndex)
}

// Pages describes every page across scans, in the same order Assemble
// assigns them (ordered by page number, retakes collapsed to the last
// photo taken) — for the web layer's "which photo has page N" view, without
// it having to re-derive that ordering itself.
func Pages(scans []Scan) []PageInfo {
	pages := loadPages(scans)
	out := make([]PageInfo, len(pages))
	for i, p := range pages {
		label := pageRefLabel(p)
		out[i] = PageInfo{
			ScanID:      p.scanID,
			PageIndex:   p.pageIndex,
			Label:       label,
			RefPrefix:   RefPrefix(p.scanID, p.pageIndex, label),
			RunningHead: p.page.RunningHead,
			Headings:    p.page.Headings,
			MarkCount:   len(p.page.Marks),
			FirstLine:   firstPrintedLine(p.page.Lines),
		}
	}
	return out
}

// firstPrintedLine is the first non-empty transcribed line of a page, with
// the model's paragraph mark stripped and whitespace collapsed.
func firstPrintedLine(lines []string) string {
	for _, line := range lines {
		text := collapseWhitespace(strings.TrimLeft(strings.TrimSpace(line), paragraphMark))
		if text != "" {
			return text
		}
	}
	return ""
}
