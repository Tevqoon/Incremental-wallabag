package pagescan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// loadGoldenScans reads testdata/scans/*.json in filename order, assigning
// scan ids 1, 2, 3... — the same convention assemble_reference.py's own
// load() uses for its argv, which is how testdata/expected.json was
// produced.
func loadGoldenScans(t *testing.T) []Scan {
	t.Helper()
	files, err := filepath.Glob("testdata/scans/*.json")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatal("no testdata scans found")
	}
	scans := make([]Scan, len(files))
	for i, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var result Result
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		scans[i] = Scan{ID: int64(i + 1), Result: result}
	}
	return scans
}

// expectedPassage mirrors the field names testdata/expected.json was
// written with (see assemble_reference.py's own assemble()).
type expectedPassage struct {
	Ref          string   `json:"ref"`
	AbsorbedRefs []string `json:"absorbed_refs"`
	Page         string   `json:"page"`
	Ordinal      int      `json:"ordinal"`
	Text         string   `json:"text"`
	NotePending  bool     `json:"note_pending"`
	ScanIDs      []int64  `json:"scan_ids"`
	Chapter      string   `json:"chapter"`
}

func loadExpected(t *testing.T) []expectedPassage {
	t.Helper()
	raw, err := os.ReadFile("testdata/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected []expectedPassage
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	return expected
}

// diffText renders a small, readable pointer at the first byte where a and
// b diverge, for a test failure message — there is no diff dependency in
// this leaf package, so this is deliberately simple rather than a real
// line/word diff.
func diffText(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	window := func(s string, at int) string {
		start := at - 30
		if start < 0 {
			start = 0
		}
		end := at + 30
		if end > len(s) {
			end = len(s)
		}
		return s[start:end]
	}
	return fmt.Sprintf("first diff at byte %d:\n  got:  ...%s...\n  want: ...%s...", i, window(a, i), window(b, i))
}

func assertPassage(t *testing.T, got Passage, want expectedPassage) {
	t.Helper()
	if got.Ref != want.Ref {
		t.Errorf("Ref: got %q, want %q", got.Ref, want.Ref)
	}
	if !equalStrings(got.AbsorbedRefs, want.AbsorbedRefs) {
		t.Errorf("AbsorbedRefs: got %v, want %v", got.AbsorbedRefs, want.AbsorbedRefs)
	}
	if got.Page != want.Page {
		t.Errorf("Page: got %q, want %q", got.Page, want.Page)
	}
	if got.Ordinal != want.Ordinal {
		t.Errorf("Ordinal: got %d, want %d", got.Ordinal, want.Ordinal)
	}
	if got.Text != want.Text {
		t.Errorf("Text mismatch for %s:\n%s", want.Ref, diffText(got.Text, want.Text))
	}
	if got.NotePending != want.NotePending {
		t.Errorf("NotePending: got %v, want %v", got.NotePending, want.NotePending)
	}
	if got.Chapter != want.Chapter {
		t.Errorf("Chapter: got %q, want %q", got.Chapter, want.Chapter)
	}
	if !equalInt64s(got.ScanIDs, want.ScanIDs) {
		t.Errorf("ScanIDs: got %v, want %v", got.ScanIDs, want.ScanIDs)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAssembleGolden(t *testing.T) {
	scans := loadGoldenScans(t)
	expected := loadExpected(t)

	got := Assemble(scans, nil)
	if len(got) != len(expected) {
		t.Fatalf("got %d passages, want %d", len(got), len(expected))
	}
	for i := range expected {
		assertPassage(t, got[i], expected[i])
	}
}

func TestAssembleInheritance(t *testing.T) {
	scans := loadGoldenScans(t)
	existing := map[string]string{
		"scan:p157:m0": "3. On the Uses of Art",
		"scan:p72:m0":  "2. Legalism",
	}
	got := Assemble(scans, existing)

	wantChapters := []string{
		"2. Legalism",
		"2. Legalism",
		"3. On the Uses of Art",
		"3. On the Uses of Art",
		"On the Morality of Warfare",
	}
	if len(got) != len(wantChapters) {
		t.Fatalf("got %d passages, want %d", len(got), len(wantChapters))
	}
	for i, want := range wantChapters {
		if got[i].Chapter != want {
			t.Errorf("passage %d (%s): chapter got %q, want %q", i, got[i].Ref, got[i].Chapter, want)
		}
	}
}

// --- Synthetic, hand-built scenarios ---

func mark(first, last int, text string) Mark {
	return Mark{FirstLine: first, LastLine: last, Text: text}
}

func TestAssembleRetake(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{{
			Label: "5", Lines: []string{"Alpha version of the line."},
			Marks: []Mark{mark(1, 1, "Alpha version of the line.")},
		}}}},
		{ID: 2, Result: Result{Pages: []Page{{
			Label: "5", Lines: []string{"Beta retaken version of the line."},
			Marks: []Mark{mark(1, 1, "Beta retaken version of the line.")},
		}}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 1 {
		t.Fatalf("got %d passages, want 1", len(got))
	}
	if got[0].Ref != "scan:p5:m0" {
		t.Errorf("Ref: got %q, want scan:p5:m0 (unchanged by the retake)", got[0].Ref)
	}
	if got[0].Text != "Beta retaken version of the line." {
		t.Errorf("Text: got %q, want the later scan's text", got[0].Text)
	}
	if !equalInt64s(got[0].ScanIDs, []int64{2}) {
		t.Errorf("ScanIDs: got %v, want [2] (the retake, not the original)", got[0].ScanIDs)
	}
}

func TestAssembleUnlabelledPageBetweenLabelled(t *testing.T) {
	scans := []Scan{
		{ID: 7, Result: Result{Pages: []Page{
			{Label: "10", Lines: []string{"Ten first line."}, Marks: []Mark{mark(1, 1, "Ten first line.")}},
			{Label: "", Lines: []string{"Middle first line."}, Marks: []Mark{mark(1, 1, "Middle first line.")}},
			{Label: "11", Lines: []string{"Eleven first line."}, Marks: []Mark{mark(1, 1, "Eleven first line.")}},
		}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 3 {
		t.Fatalf("got %d passages, want 3", len(got))
	}
	if got[0].Ref != "scan:p10:m0" || got[0].Ordinal != 10010000 {
		t.Errorf("page 10: got ref=%q ordinal=%d", got[0].Ref, got[0].Ordinal)
	}
	if got[1].Ref != "scan:s7.1:m0" {
		t.Errorf("unlabelled page: got ref %q, want scan:s7.1:m0", got[1].Ref)
	}
	if got[1].Ordinal != 10010500 {
		t.Errorf("unlabelled page: got ordinal %d, want 10010500 (predecessor's key +500)", got[1].Ordinal)
	}
	if got[2].Ref != "scan:p11:m0" || got[2].Ordinal != 10011000 {
		t.Errorf("page 11: got ref=%q ordinal=%d", got[2].Ref, got[2].Ordinal)
	}
}

func TestAssembleRomanBeforeArabic(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{
			{Label: "1", Lines: []string{"Arabic body text line."}, Marks: []Mark{mark(1, 1, "Arabic body text line.")}},
			{Label: "iv", Lines: []string{"Roman front matter line."}, Marks: []Mark{mark(1, 1, "Roman front matter line.")}},
		}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 2 {
		t.Fatalf("got %d passages, want 2", len(got))
	}
	if got[0].Page != "iv" {
		t.Errorf("first passage: got page %q, want roman numeral page to sort first", got[0].Page)
	}
	if got[1].Page != "1" {
		t.Errorf("second passage: got page %q, want arabic page second", got[1].Page)
	}
}

func TestAssembleLonelyBottomContinuationTrimmedBack(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{{
			Label: "50",
			Lines: []string{
				"This is a complete opening sentence.",
				"This second line starts a new sentence but trails off without a period",
			},
			Marks: []Mark{mark(1, 2, "This is a complete opening sentence. This second line starts a new sentence but trails off without a period")},
		}}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 1 {
		t.Fatalf("got %d passages, want 1", len(got))
	}
	want := "This is a complete opening sentence."
	if got[0].Text != want {
		t.Errorf("Text: got %q, want %q (trimmed back to the last full sentence)", got[0].Text, want)
	}
}

func TestAssembleLonelyTopContinuationTrimmedForward(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{{
			Label: "60",
			Lines: []string{
				"mid-sentence continuation ends here. Now a fresh sentence begins and ends properly.",
			},
			Marks: []Mark{mark(1, 1, "mid-sentence continuation ends here. Now a fresh sentence begins and ends properly.")},
		}}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 1 {
		t.Fatalf("got %d passages, want 1", len(got))
	}
	want := "Now a fresh sentence begins and ends properly."
	if got[0].Text != want {
		t.Errorf("Text: got %q, want %q (trimmed forward to the first sentence start)", got[0].Text, want)
	}
}

func TestAssembleJoinAcrossPageBreakHyphenDropped(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{
			{
				Label: "20",
				Lines: []string{
					"First line of the chapter here.",
					"This chapter discusses eco-",
				},
				Marks: []Mark{mark(1, 2, "First line of the chapter here. This chapter discussion continues.")},
			},
		}}},
		{ID: 1, Result: Result{Pages: []Page{
			{
				Label: "21",
				Lines: []string{
					"nomic policy in some detail.",
					"Second line unrelated to the mark.",
				},
				Marks: []Mark{mark(1, 1, "nomic policy in some detail.")},
			},
		}}},
	}
	// Same scan id used for both photos on purpose — the join logic only
	// cares about page numbers and load order, not distinct scans.
	got := Assemble(scans, nil)
	if len(got) != 1 {
		t.Fatalf("got %d passages, want 1 (the two fragments should join across the page break)", len(got))
	}
	want := "First line of the chapter here. This chapter discusses economic policy in some detail."
	if got[0].Text != want {
		t.Errorf("Text: got %q, want %q (line-break hyphen dropped, no compound in the model text)", got[0].Text, want)
	}
	if got[0].Page != "20–21" {
		t.Errorf("Page: got %q, want \"20\\u201321\"", got[0].Page)
	}
	if len(got[0].AbsorbedRefs) != 1 || got[0].AbsorbedRefs[0] != "scan:p21:m0" {
		t.Errorf("AbsorbedRefs: got %v, want [scan:p21:m0]", got[0].AbsorbedRefs)
	}
}

func TestAssembleHyphenKeptForCompoundInModelText(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{{
			Label: "30",
			Lines: []string{
				"It is often in a person's self-",
				"interest to cooperate with others.",
			},
			Marks: []Mark{mark(1, 2, "It is often in a person's self-interest to cooperate with others.")},
		}}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 1 {
		t.Fatalf("got %d passages, want 1", len(got))
	}
	want := "It is often in a person's self-interest to cooperate with others."
	if got[0].Text != want {
		t.Errorf("Text: got %q, want %q (hyphen kept: the model's own text spells the compound)", got[0].Text, want)
	}
}

func TestAssembleParagraphMarker(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{{
			Label: "40",
			Lines: []string{
				"First paragraph opening line.",
				"¶ Second paragraph starts here and ends cleanly.",
			},
			Marks: []Mark{mark(1, 2, "First paragraph opening line.\n\nSecond paragraph starts here and ends cleanly.")},
		}}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 1 {
		t.Fatalf("got %d passages, want 1", len(got))
	}
	want := "First paragraph opening line.\n\nSecond paragraph starts here and ends cleanly."
	if got[0].Text != want {
		t.Errorf("Text: got %q, want %q (\\u00b6 line starts a new paragraph)", got[0].Text, want)
	}
}

func TestAssembleChapterGroupsByRectoHeadsWithConstantVerso(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{
			{Label: "10", RunningHead: "MY BOOK TITLE", Lines: []string{"Ten content line ends cleanly."}, Marks: []Mark{mark(1, 1, "Ten content line ends cleanly.")}},
			{Label: "11", RunningHead: "FIRST STORY", Lines: []string{"Eleven content line ends cleanly."}, Marks: []Mark{mark(1, 1, "Eleven content line ends cleanly.")}},
			{Label: "12", RunningHead: "MY BOOK TITLE", Lines: []string{"Twelve content line ends cleanly."}, Marks: []Mark{mark(1, 1, "Twelve content line ends cleanly.")}},
			{Label: "13", RunningHead: "SECOND STORY", Lines: []string{"Thirteen content line ends cleanly."}, Marks: []Mark{mark(1, 1, "Thirteen content line ends cleanly.")}},
		}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 4 {
		t.Fatalf("got %d passages, want 4", len(got))
	}
	wantChapters := []string{"First Story", "First Story", "First Story", "Second Story"}
	for i, want := range wantChapters {
		if got[i].Chapter != want {
			t.Errorf("passage %d (page %s): chapter got %q, want %q", i, got[i].Page, got[i].Chapter, want)
		}
	}
}

func TestAssembleOutOfRangeLinesClamped(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{{
			Label: "80",
			Lines: []string{"Only line present, ends cleanly."},
			Marks: []Mark{mark(-5, 99, "Only line present, ends cleanly.")},
		}}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 1 {
		t.Fatalf("got %d passages, want 1", len(got))
	}
	want := "Only line present, ends cleanly."
	if got[0].Text != want {
		t.Errorf("Text: got %q, want %q (out-of-range line numbers clamped)", got[0].Text, want)
	}
}

func TestAssemblePageWithNoLinesYieldsNoPassages(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{{
			Label: "90",
			Lines: nil,
			Marks: []Mark{mark(1, 1, "whatever")},
		}}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 0 {
		t.Fatalf("got %d passages, want 0 (a page with no lines contributes nothing)", len(got))
	}
}

func TestAssembleMarkWithoutLineNumbersIsSkipped(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{{
			Label: "91",
			Lines: []string{
				"First sentence ends here.",
				"Second sentence ends here too.",
			},
			Marks: []Mark{
				{Text: "the model forgot the line numbers"},
				mark(2, 2, "Second sentence ends here too."),
			},
		}}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 1 {
		t.Fatalf("got %d passages, want 1 (a mark with no line numbers must not become a passage)", len(got))
	}
	if got[0].Ref != "scan:p91:m1" {
		t.Errorf("Ref: got %q, want scan:p91:m1 (the skipped mark still holds index 0)", got[0].Ref)
	}
	if got[0].Text != "Second sentence ends here too." {
		t.Errorf("Text: got %q", got[0].Text)
	}
}

func TestAssembleTwoMarksOnOnePage(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{{
			Label: "95",
			Lines: []string{
				"First sentence ends here.",
				"Second sentence ends here too.",
				"Third sentence ends here also.",
			},
			Marks: []Mark{
				mark(1, 1, "First sentence ends here."),
				mark(3, 3, "Third sentence ends here also."),
			},
		}}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 2 {
		t.Fatalf("got %d passages, want 2", len(got))
	}
	if got[0].Ref != "scan:p95:m0" || got[0].Text != "First sentence ends here." {
		t.Errorf("first mark: got ref=%q text=%q", got[0].Ref, got[0].Text)
	}
	if got[1].Ref != "scan:p95:m1" || got[1].Text != "Third sentence ends here also." {
		t.Errorf("second mark: got ref=%q text=%q", got[1].Ref, got[1].Text)
	}
}

func TestTitleCase(t *testing.T) {
	cases := []struct{ head, want string }{
		{"ON THE USES OF ART", "On the Uses of Art"},
		{"CHAPTER 2", "Chapter 2"},
		{"Already Mixed Case", "Already Mixed Case"},
		{"THE END", "The End"},
	}
	for _, c := range cases {
		if got := titleCase(c.head); got != c.want {
			t.Errorf("titleCase(%q): got %q, want %q", c.head, got, c.want)
		}
	}
}

func TestPagesOrdering(t *testing.T) {
	scans := []Scan{
		{ID: 3, Result: Result{Pages: []Page{
			{Label: "2", RunningHead: "HEAD", Lines: []string{"line"}, Marks: []Mark{mark(1, 1, "line")}},
		}}},
		{ID: 1, Result: Result{Pages: []Page{
			{Label: "1", RunningHead: "HEAD", Lines: []string{"line"}, Marks: []Mark{mark(1, 1, "line"), mark(1, 1, "line")}},
		}}},
	}
	infos := Pages(scans)
	if len(infos) != 2 {
		t.Fatalf("got %d pages, want 2", len(infos))
	}
	if infos[0].Label != "1" || infos[0].ScanID != 1 {
		t.Errorf("first page: got label=%q scanID=%d, want label=1 scanID=1", infos[0].Label, infos[0].ScanID)
	}
	if infos[0].MarkCount != 2 {
		t.Errorf("first page: got MarkCount %d, want 2", infos[0].MarkCount)
	}
	if infos[1].Label != "2" || infos[1].ScanID != 3 {
		t.Errorf("second page: got label=%q scanID=%d, want label=2 scanID=3", infos[1].Label, infos[1].ScanID)
	}
	if infos[0].RefPrefix != "scan:p1" || infos[1].RefPrefix != "scan:p2" {
		t.Errorf("RefPrefix mismatch: got %q, %q", infos[0].RefPrefix, infos[1].RefPrefix)
	}
}

func TestPagesUnlabelledUsesScanPrefix(t *testing.T) {
	scans := []Scan{
		{ID: 9, Result: Result{Pages: []Page{
			{Label: "", RunningHead: "", Lines: []string{"line"}},
		}}},
	}
	infos := Pages(scans)
	if len(infos) != 1 {
		t.Fatalf("got %d pages, want 1", len(infos))
	}
	if infos[0].Label != "" {
		t.Errorf("Label: got %q, want \"\"", infos[0].Label)
	}
	if infos[0].RefPrefix != "scan:s9.0" {
		t.Errorf("RefPrefix: got %q, want scan:s9.0", infos[0].RefPrefix)
	}
}

func TestAssembleHeadingLinesAreNeverAPassage(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{{
			Label:    "72",
			Headings: []string{"1. Han Feizi on Law"},
			Lines: []string{
				"corruption.",
				"1. Han Feizi on Law",
				"Han Feizi was a thinker. He wrote his ideas",
				"in book form. Han Feizi argued",
			},
			Marks: []Mark{
				// The bracket's top tick beside the heading, reported alone.
				mark(2, 2, "1."),
				// The bracket itself, with the heading counted into it.
				mark(2, 4, "Han Feizi was a thinker. He wrote his ideas in book form."),
			},
		}}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 1 {
		t.Fatalf("got %d passages, want 1 (a heading-only mark is not a passage)", len(got))
	}
	if got[0].Ref != "scan:p72:m1" {
		t.Errorf("Ref: got %q, want scan:p72:m1", got[0].Ref)
	}
	want := "Han Feizi was a thinker. He wrote his ideas in book form."
	if got[0].Text != want {
		t.Errorf("Text: got %q, want %q (heading dropped, body after it starts a sentence)", got[0].Text, want)
	}
}

func TestAssembleHighlightIsExactlyTheMarkedWords(t *testing.T) {
	scans := []Scan{
		{ID: 1, Result: Result{Pages: []Page{{
			Label: "80",
			Lines: []string{
				"fear of harm, and the state should reward people for doing",
				"what's good for the state and ruthlessly punish those who harm",
			},
			Marks: []Mark{{
				FirstLine: 1, LastLine: 2, Kind: "underline",
				Text: "reward people for doing  what's good for the state",
			}},
		}}}},
	}
	got := Assemble(scans, nil)
	if len(got) != 1 {
		t.Fatalf("got %d passages, want 1", len(got))
	}
	want := "reward people for doing what's good for the state"
	if got[0].Text != want {
		t.Errorf("Text: got %q, want %q (an underline is its words, not whole sentences)", got[0].Text, want)
	}
}

// TestAssembleHeadingTickRegression replays a real model response in which
// the bracket's top tick beside "1. Han Feizi on Law" came back as a mark of
// its own. Before heading lines were excluded from a mark's range it was
// imported as a passage reading "1.".
func TestAssembleHeadingTickRegression(t *testing.T) {
	raw, err := os.ReadFile("testdata/regress/IMG_6519_heading_tick.json")
	if err != nil {
		t.Fatal(err)
	}
	result, err := ParseResult(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	got := Assemble([]Scan{{ID: 1, Result: result}}, nil)
	if len(got) != 1 {
		for _, p := range got {
			t.Logf("passage %s: %q", p.Ref, p.Text)
		}
		t.Fatalf("got %d passages, want 1", len(got))
	}
	const prefix = "Han Feizi (ca. 280"
	if len(got[0].Text) < len(prefix) || got[0].Text[:len(prefix)] != prefix {
		t.Errorf("Text: got %q, want it to start with %q", got[0].Text, prefix)
	}
}
