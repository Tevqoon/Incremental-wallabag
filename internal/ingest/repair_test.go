package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/store"
)

func testRepairStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestRepairRemapsRefsClearsContentAndAnchorsButPreservesScheduling is
// Repair's own load-bearing test: after a highlight has been re-anchored at
// wallabag (its id necessarily changing — see Remap's own comment) and its
// entry's content replaced, the local row must end up pointing at the new
// id, forget its stale body and position — but the reading schedule already
// built up on that row is the entire reason this design exists, and it must
// not move.
func TestRepairRemapsRefsClearsContentAndAnchorsButPreservesScheduling(t *testing.T) {
	db := testRepairStore(t)
	now := time.Now()

	// Seed a document the way a sync would: a paywall preview, with one
	// highlight already imported under its original wallabag annotation id.
	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "555", Title: "A preview", UpdatedAt: now,
		ContentHTML: "<p>Subscribe to keep reading…</p>",
		Highlights:  []source.Highlight{{ExternalID: "100", Quote: "A passage worth keeping."}},
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	extracts, err := db.ChildrenOf(1)
	if err != nil || len(extracts) != 1 {
		t.Fatalf("ChildrenOf: %v (extracts=%+v)", err, extracts)
	}
	extractID := extracts[0].ID

	// Give the highlight a real reading history — this is exactly what must
	// survive its wallabag id changing underneath it.
	if err := db.SaveSchedule(extractID, ir.Schedule{
		State: ir.StateReading, IntervalDays: 15, AFactor: 2.3, Reps: 3,
		Priority: 0.42, DueOn: now.AddDate(0, 0, 15),
	}, now); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}

	// Anchor it, the way opening the (old, preview) article and locating the
	// passage in it would have — this position is what went stale the
	// moment the preview was replaced with the full article.
	if err := db.AnchorExtract(extractID, ir.Range{StartBlock: 0, StartOffset: 0, EndBlock: 0, EndOffset: 10},
		"A passage worth keeping.", "<p>A passage worth keeping.</p>", now); err != nil {
		t.Fatalf("AnchorExtract: %v", err)
	}

	applied := Applied{
		Remaps: map[int][]Remap{
			555: {{Old: "100", New: "200"}},
		},
	}

	logger := discardLogger()
	result, err := Repair(context.Background(), db, applied, now, logger)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if result.Repaired != 1 {
		t.Errorf("Repaired = %d, want 1", result.Repaired)
	}
	if len(result.Errors) != 0 {
		t.Errorf("Errors = %v, want none", result.Errors)
	}

	document, err := db.DocumentByID(1)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.HasContent {
		t.Error("HasContent is still true after Repair; content should have been cleared for re-fetch")
	}
	if document.ContentHTML != "" {
		t.Errorf("ContentHTML = %q, want cleared", document.ContentHTML)
	}
	if document.MissingUpstream {
		t.Error("document marked missing upstream, want untouched")
	}

	element, err := db.ElementByID(extractID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if element.ExternalRef != "200" {
		t.Errorf("ExternalRef = %q, want the remapped id, 200", element.ExternalRef)
	}
	if element.HasRange {
		t.Error("HasRange is still true after Repair; the stale position should have been cleared")
	}
	if element.MissingUpstream {
		t.Error("element marked missing upstream, want cleared by the remap")
	}
	if element.ContentHTML != "<p>A passage worth keeping.</p>" {
		t.Errorf("ContentHTML = %q, want untouched by ClearExtractAnchors", element.ContentHTML)
	}
	if element.Quote != "A passage worth keeping." {
		t.Errorf("Quote = %q, want untouched", element.Quote)
	}

	// The whole point: scheduling survived the id change and the anchor
	// clear untouched.
	if element.Schedule.IntervalDays != 15 || element.Schedule.AFactor != 2.3 ||
		element.Schedule.Reps != 3 || element.Schedule.Priority != 0.42 {
		t.Errorf("scheduling was disturbed by Repair: %+v", element.Schedule)
	}
	if element.Schedule.DueOn.Format("2006-01-02") != now.AddDate(0, 0, 15).Format("2006-01-02") {
		t.Errorf("due_on = %v, want unchanged", element.Schedule.DueOn)
	}
}

// TestRepairSkipsEntriesWithNoLocalDocumentYet covers the ordinary case of a
// brand-new wallabag entry this same run just created: it has no local row
// at all, since that row is only made by the next sync, not by the create
// itself. store.ErrNotFound must be tolerated as Skipped, not surfaced as an
// error.
func TestRepairSkipsEntriesWithNoLocalDocumentYet(t *testing.T) {
	db := testRepairStore(t)

	applied := Applied{Remaps: map[int][]Remap{777: nil}}

	result, err := Repair(context.Background(), db, applied, time.Now(), discardLogger())
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", result.Skipped)
	}
	if result.Repaired != 0 {
		t.Errorf("Repaired = %d, want 0", result.Repaired)
	}
	if len(result.Errors) != 0 {
		t.Errorf("Errors = %v, want none — a missing local document is not a failure", result.Errors)
	}
}

// TestRepairIsIdempotent covers the safety property the whole design relies
// on: running Repair twice with the same Applied value — the shape a retry
// after a crash between the two calls would produce — must not fail or
// double-apply anything the second time.
func TestRepairIsIdempotent(t *testing.T) {
	db := testRepairStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "555", Title: "A preview", UpdatedAt: now,
		ContentHTML: "<p>Subscribe to keep reading…</p>",
		Highlights:  []source.Highlight{{ExternalID: "100", Quote: "A passage worth keeping."}},
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	applied := Applied{Remaps: map[int][]Remap{555: {{Old: "100", New: "200"}}}}

	first, err := Repair(context.Background(), db, applied, now, discardLogger())
	if err != nil {
		t.Fatalf("first Repair: %v", err)
	}
	if first.Repaired != 1 || len(first.Errors) != 0 {
		t.Fatalf("first Repair = %+v, want Repaired=1 and no errors", first)
	}

	// Re-run with the identical Applied value, as a retry after a crash
	// right after the first call would. The ref is already remapped
	// (RemapExternalRef's zero-match tolerance), and content/anchors are
	// already cleared (both idempotent updates) — nothing here should error.
	second, err := Repair(context.Background(), db, applied, now, discardLogger())
	if err != nil {
		t.Fatalf("second Repair: %v", err)
	}
	if len(second.Errors) != 0 {
		t.Errorf("second Repair.Errors = %v, want none", second.Errors)
	}
	if second.Repaired != 1 {
		t.Errorf("second Repair.Repaired = %d, want 1 (the document was still found and re-cleared, harmlessly)", second.Repaired)
	}

	extracts, err := db.ChildrenOf(1)
	if err != nil || len(extracts) != 1 {
		t.Fatalf("ChildrenOf after two repairs: %v (extracts=%+v)", err, extracts)
	}
	if extracts[0].ExternalRef != "200" {
		t.Errorf("ExternalRef after two repairs = %q, want still 200, not duplicated or reverted", extracts[0].ExternalRef)
	}
}

// TestRepairRequeuesOnlyTheGrownDocument is the requirement in one test: two
// documents go through the same Repair call, one flagged as grown and one
// not, and only the grown one comes back into the reading queue due today.
// The other — a free post that was already complete and which the reader
// had already marked done — must be left exactly as it was, since that is
// the other, equally important half of the rule: growth is what earns a
// document back into the queue, and nothing else does.
func TestRepairRequeuesOnlyTheGrownDocument(t *testing.T) {
	db := testRepairStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "555", Title: "Grew: a preview finally backfilled", UpdatedAt: now},
		{ExternalID: "556", Title: "Did not grow: already complete", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	// Document 1 (external id 555) was read and marked done; document 2
	// (external id 556) was dismissed unread. Neither's prior state should
	// matter to how Repair treats it — only Applied.Grew does.
	if err := db.SaveSchedule(1, ir.Schedule{State: ir.StateDone}, now); err != nil {
		t.Fatalf("SaveSchedule(555): %v", err)
	}
	if err := db.SaveSchedule(2, ir.Schedule{State: ir.StateDismissed}, now); err != nil {
		t.Fatalf("SaveSchedule(556): %v", err)
	}

	requeueDay := now.AddDate(0, 0, 2)
	applied := Applied{
		Remaps: map[int][]Remap{555: nil, 556: nil},
		Grew:   map[int]bool{555: true, 556: false},
	}

	result, err := Repair(context.Background(), db, applied, requeueDay, discardLogger())
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if result.Repaired != 2 {
		t.Errorf("Repaired = %d, want 2 (both documents' content/anchors are cleared regardless of growth)", result.Repaired)
	}
	if result.Requeued != 1 {
		t.Errorf("Requeued = %d, want 1 (only the grown document)", result.Requeued)
	}

	grown, err := db.ElementByID(1)
	if err != nil {
		t.Fatalf("ElementByID(1): %v", err)
	}
	if grown.Schedule.State != ir.StateNew {
		t.Errorf("grown document's state = %q, want %q — it must return to the queue", grown.Schedule.State, ir.StateNew)
	}
	if grown.Schedule.DueOn.Format("2006-01-02") != requeueDay.Format("2006-01-02") {
		t.Errorf("grown document's due_on = %v, want today (%v)", grown.Schedule.DueOn, requeueDay)
	}

	ungrown, err := db.ElementByID(2)
	if err != nil {
		t.Fatalf("ElementByID(2): %v", err)
	}
	if ungrown.Schedule.State != ir.StateDismissed {
		t.Errorf("ungrown document's state = %q, want untouched (%q) — an already-complete post must not be requeued",
			ungrown.Schedule.State, ir.StateDismissed)
	}
	if !ungrown.Schedule.DueOn.IsZero() {
		t.Errorf("ungrown document's due_on = %v, want still unset", ungrown.Schedule.DueOn)
	}
}
