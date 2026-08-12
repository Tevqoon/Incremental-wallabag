package ingest

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Tevqoon/increader/internal/source"
)

// TestWriteReportSaysTheFirstOccurrenceWins pins the one thing the design
// insists must be stated in words: ambiguity is reported, never resolved,
// and re-anchoring always takes the first occurrence.
func TestWriteReportSaysTheFirstOccurrenceWins(t *testing.T) {
	plan := Plan{Items: []Item{{
		Post:    source.Document{URL: "https://example.substack.com/p/a-post"},
		EntryID: 1,
		Action:  ActionAnnotationsOnly,
		Annotations: []AnnotationPlan{
			{AnnotationID: 500, Quote: "an ambiguous quote", Verdict: VerdictAmbiguous, Occurrences: 3},
		},
	}}}

	var buf bytes.Buffer
	if err := WriteReport(&buf, plan, nil); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	report := buf.String()
	if !strings.Contains(strings.ToLower(report), "first occurrence") {
		t.Errorf("report does not mention that the first occurrence wins:\n%s", report)
	}
}

// TestWriteReportContainsNoSecrets is a straightforward audit: neither a dry
// run nor a completed run's report may contain anything resembling a
// wallabag credential or the Substack session cookie. Nothing passed to
// WriteReport actually carries either — see its own doc comment — so this
// exists to catch a future field added to Item, Post or Applied that
// forgets that constraint, not because there is redaction logic here today
// that could regress.
func TestWriteReportContainsNoSecrets(t *testing.T) {
	plan := Plan{Items: []Item{
		{
			Post:    source.Document{URL: "https://example.substack.com/p/a-post", Title: "A Post", Author: "An Author"},
			EntryID: 1, Action: ActionUpdate,
		},
		{
			Post:   source.Document{URL: "https://example.substack.com/p/conflicted"},
			Action: ActionConflict,
			Notes:  []string{"entry 1 carries 2 annotation(s)", "entry 2 carries 5 annotation(s)"},
		},
	}, Conflicts: 1}

	applied := &Applied{
		Created: 1, Updated: 1, Reanchored: 2, AnnotationFailures: 1,
		Errors: []error{errString("simulated network failure")},
	}

	var buf bytes.Buffer
	if err := WriteReport(&buf, plan, applied); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	report := strings.ToLower(buf.String())
	for _, forbidden := range []string{"substack.sid", "session", "cookie", "client_secret", "password", "bearer"} {
		if strings.Contains(report, forbidden) {
			t.Errorf("report contains %q, want no trace of any credential-shaped word:\n%s", forbidden, buf.String())
		}
	}
}

// TestWriteReportDryRunOmitsApplied covers the applied == nil case: a dry
// run has nothing to report about what was actually written, since nothing
// was.
func TestWriteReportDryRunOmitsApplied(t *testing.T) {
	plan := Plan{Items: []Item{
		{Post: source.Document{URL: "https://example.substack.com/p/a-post"}, Action: ActionCreate},
	}}

	var buf bytes.Buffer
	if err := WriteReport(&buf, plan, nil); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if strings.Contains(buf.String(), "Applied:") {
		t.Errorf("dry-run report mentions Applied section, want none:\n%s", buf.String())
	}
}

// TestWriteReportNotesWhenAMatchDependedOnTrimming covers report.go's own
// half of the truncation-marker fix: an annotation whose classification only
// succeeded because wallabag.TrimTruncationMarker was applied first
// (AnnotationPlan.TrimmedMatch) must say so in its own line, so the operator
// can see a match depended on undoing wallabag's own truncation rather than
// that being invisible in the numbers.
func TestWriteReportNotesWhenAMatchDependedOnTrimming(t *testing.T) {
	plan := Plan{Items: []Item{{
		Post:    source.Document{URL: "https://example.substack.com/p/a-post"},
		EntryID: 1,
		Action:  ActionAnnotationsOnly,
		Annotations: []AnnotationPlan{
			{AnnotationID: 500, Quote: "a passage worth keeping…", Verdict: VerdictUnique, Occurrences: 1, TrimmedMatch: true},
		},
	}}}

	var buf bytes.Buffer
	if err := WriteReport(&buf, plan, nil); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	report := strings.ToLower(buf.String())
	if !strings.Contains(report, "truncation") {
		t.Errorf("report does not mention that this match depended on undoing wallabag's truncation:\n%s", buf.String())
	}
}

// errString is a trivial error for report tests that need one without
// pulling in errors.New at every call site.
type errString string

func (e errString) Error() string { return string(e) }

// TestWriteReportShowsContentGrowthProminently pins the coordinator's own
// stated priority: this is the operator's main reason for running the
// importer, so the report must headline how many posts are returning to the
// queue and show the actual old -> new byte counts behind that decision,
// not bury it inside the generic per-item annotation detail.
func TestWriteReportShowsContentGrowthProminently(t *testing.T) {
	plan := Plan{Items: []Item{
		{
			Post: source.Document{URL: "https://example.substack.com/p/grew"}, EntryID: 1,
			Action: ActionUpdate, OldBytes: 200, NewBytes: 4000, ContentGrew: true,
		},
		{
			Post: source.Document{URL: "https://example.substack.com/p/tweaked"}, EntryID: 2,
			Action: ActionUpdate, OldBytes: 4000, NewBytes: 4100, ContentGrew: false,
		},
	}}

	var buf bytes.Buffer
	if err := WriteReport(&buf, plan, nil); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	report := buf.String()

	if !strings.Contains(report, "Returning to the reading queue") {
		t.Errorf("report does not headline the requeue count:\n%s", report)
	}
	if !strings.Contains(report, "200 -> 4000") {
		t.Errorf("report is missing the grown entry's old -> new byte counts:\n%s", report)
	}
	if !strings.Contains(report, "4000 -> 4100") {
		t.Errorf("report is missing the ungrown entry's old -> new byte counts:\n%s", report)
	}

	// The requeue count must appear before, not after, the same section's
	// own byte-count detail — "prominently" means headlined, not buried.
	headline := strings.Index(report, "Returning to the reading queue")
	detail := strings.Index(report, "200 -> 4000")
	if headline == -1 || detail == -1 || headline > detail {
		t.Errorf("requeue headline (index %d) does not precede the byte-count detail (index %d)", headline, detail)
	}
}
