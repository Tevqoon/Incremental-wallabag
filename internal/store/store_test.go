package store

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/source"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	// t.TempDir is removed automatically when the test finishes, so each test
	// gets a genuinely fresh database with no cleanup code.
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	first.Close()

	// Re-opening must not try to re-apply migration 001, which would fail on
	// "table already exists".
	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()

	var version int
	if err := second.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}

	// Derived from the embedded files rather than hard-coded, so adding a
	// migration does not require editing this test. What is being asserted is
	// that every migration ran exactly once, not any particular number.
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	if version != len(files) {
		t.Errorf("user_version = %d, want %d (one per migration file)", version, len(files))
	}
}

func TestUpsertCreatesDocumentAndRootTopic(t *testing.T) {
	db := testStore(t)
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)

	result, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1",
		Title:      "First article",
		URL:        "https://example.com/1",
		UpdatedAt:  time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC),
	}}, 0, 0, now)
	if err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if result.Created != 1 || result.Updated != 0 {
		t.Errorf("got %d created / %d updated, want 1 / 0", result.Created, result.Updated)
	}

	// Every new document must get exactly one root topic, or it never reaches
	// the reading queue.
	elements, err := db.CountElements("")
	if err != nil {
		t.Fatalf("CountElements: %v", err)
	}
	if elements != 1 {
		t.Errorf("got %d elements, want 1 root topic", elements)
	}

	// The root topic is due today, so a freshly synced article is immediately
	// readable rather than waiting for a cycle.
	due, err := db.CountDue(now, QueueArticles)
	if err != nil {
		t.Fatalf("CountDue: %v", err)
	}
	if due != 1 {
		t.Errorf("got %d due today, want 1", due)
	}
}

func TestUpsertIsIdempotent(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	document := source.Document{
		ExternalID: "1",
		Title:      "First article",
		UpdatedAt:  time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC),
	}

	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	document.Title = "Retitled upstream"
	result, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	if result.Created != 0 || result.Updated != 1 {
		t.Errorf("got %d created / %d updated, want 0 / 1", result.Created, result.Updated)
	}

	documents, err := db.CountDocuments("wallabag")
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	if documents != 1 {
		t.Errorf("got %d documents, want 1", documents)
	}

	// Re-syncing must not spawn a second root topic; that would duplicate the
	// article in the queue and split its reading history.
	elements, err := db.CountElements("")
	if err != nil {
		t.Fatalf("CountElements: %v", err)
	}
	if elements != 1 {
		t.Errorf("got %d elements after re-sync, want 1", elements)
	}
}

// TestMetadataSyncPreservesFetchedContent covers the interaction that makes the
// lazy-body design work: listings arrive without article text, and re-syncing
// one must not wipe a body that was already fetched.
func TestMetadataSyncPreservesFetchedContent(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	updated := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1",
		Title:      "First article",
		UpdatedAt:  updated,
	}}, 0, 0, now); err != nil {
		t.Fatalf("initial metadata sync: %v", err)
	}

	document, err := db.DocumentByID(1)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.HasContent {
		t.Error("a metadata-only sync should not report having content")
	}

	if err := db.SetDocumentContent(1, "<p>The body.</p>"); err != nil {
		t.Fatalf("SetDocumentContent: %v", err)
	}

	// A later metadata-only sync of the same document.
	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1",
		Title:      "First article",
		UpdatedAt:  updated.Add(time.Hour),
	}}, 0, 0, now); err != nil {
		t.Fatalf("second metadata sync: %v", err)
	}

	document, err = db.DocumentByID(1)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.ContentHTML != "<p>The body.</p>" {
		t.Errorf("content = %q, want it preserved across a metadata sync", document.ContentHTML)
	}
	if !document.HasContent {
		t.Error("has_content was cleared by a metadata sync")
	}
}

func TestUpsertReportsNewestWatermark(t *testing.T) {
	db := testStore(t)
	newest := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	result, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", UpdatedAt: time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)},
		{ExternalID: "2", UpdatedAt: newest},
		{ExternalID: "3", UpdatedAt: time.Date(2026, 7, 30, 6, 0, 0, 0, time.UTC)},
	}, 0, 0, time.Now())
	if err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if !result.Watermark.Equal(newest) {
		t.Errorf("watermark = %v, want the newest update time %v", result.Watermark, newest)
	}
}

func TestSyncStateRoundTrip(t *testing.T) {
	db := testStore(t)

	// A source that has never run reports the zero time, which means
	// "fetch everything".
	state, err := db.SyncState("wallabag")
	if err != nil {
		t.Fatalf("SyncState: %v", err)
	}
	if !state.Watermark.IsZero() {
		t.Errorf("watermark = %v, want zero for an unsynced source", state.Watermark)
	}

	watermark := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	if err := db.SaveSyncState(SyncState{
		Source:    "wallabag",
		Watermark: watermark,
		LastRun:   time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SaveSyncState: %v", err)
	}

	state, err = db.SyncState("wallabag")
	if err != nil {
		t.Fatalf("SyncState after save: %v", err)
	}
	if !state.Watermark.Equal(watermark) {
		t.Errorf("watermark = %v, want %v", state.Watermark, watermark)
	}

	// Saving again must update in place rather than inserting a second row.
	if err := db.SaveSyncState(SyncState{Source: "wallabag", Watermark: watermark.Add(time.Hour)}); err != nil {
		t.Fatalf("second SaveSyncState: %v", err)
	}
	var rows int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM sync_state`).Scan(&rows); err != nil {
		t.Fatalf("count sync_state: %v", err)
	}
	if rows != 1 {
		t.Errorf("got %d sync_state rows, want 1", rows)
	}
}

// TestForeignKeysEnforced guards the pragma in Open. SQLite silently ignores
// foreign keys unless they are switched on per connection, which would make
// every ON DELETE CASCADE in the schema a no-op and orphan elements.
func TestForeignKeysEnforced(t *testing.T) {
	db := testStore(t)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Doomed", UpdatedAt: time.Now()},
	}, 0, 0, time.Now()); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if _, err := db.db.Exec(`DELETE FROM documents WHERE id = 1`); err != nil {
		t.Fatalf("delete document: %v", err)
	}

	elements, err := db.CountElements("")
	if err != nil {
		t.Fatalf("CountElements: %v", err)
	}
	if elements != 0 {
		t.Errorf("got %d elements after deleting their document, want 0 (cascade did not fire)", elements)
	}
}

// TestArchivedDocumentsArriveSuspended is the fix for a queue that was 97%
// noise: material wallabag has already filed as read must not compete with what
// is genuinely unread.
func TestArchivedDocumentsArriveSuspended(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Unread", UpdatedAt: now},
		{ExternalID: "2", Title: "Already read", IsArchived: true, UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	queue, err := db.Queue(now, QueueArticles, 10)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(queue) != 1 {
		t.Fatalf("queue holds %d elements, want only the unread article", len(queue))
	}
	if queue[0].Title != "Unread" {
		t.Errorf("queued %q, want the unread article", queue[0].Title)
	}

	due, err := db.CountDue(now, QueueArticles)
	if err != nil {
		t.Fatalf("CountDue: %v", err)
	}
	if due != 1 {
		t.Errorf("%d due, want 1 — suspended material is being counted", due)
	}

	// It is suspended, not deleted: still present and still openable.
	archived, err := db.ElementByID(2)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if archived.Schedule.State != ir.StateSuspended {
		t.Errorf("state = %q, want %q", archived.Schedule.State, ir.StateSuspended)
	}
}

// TestArchivingLaterSuspends covers the transition: archiving in wallabag after
// the fact should take an article out of the queue here too.
func TestArchivingLaterSuspends(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	document := source.Document{ExternalID: "1", Title: "In progress", UpdatedAt: now}
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	document.IsArchived = true
	document.UpdatedAt = now.Add(time.Hour)
	result, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if result.Suspended != 1 {
		t.Errorf("reported %d suspended, want 1", result.Suspended)
	}

	queue, _ := db.Queue(now, QueueArticles, 10)
	if len(queue) != 0 {
		t.Errorf("archived article is still queued")
	}
}

// TestUnsuspendingSurvivesResync is the guard against the app arguing with the
// reader. Pulling something back into the queue is a deliberate act, and a
// later sync of an article that is still archived must not undo it.
func TestUnsuspendingSurvivesResync(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	document := source.Document{
		ExternalID: "1", Title: "Worth re-reading", IsArchived: true, UpdatedAt: now,
	}
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	if err := db.Unsuspend(1, now, now); err != nil {
		t.Fatalf("Unsuspend: %v", err)
	}

	// Sync again — still archived upstream, but not a fresh transition.
	document.UpdatedAt = now.Add(time.Hour)
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	queue, _ := db.Queue(now, QueueArticles, 10)
	if len(queue) != 1 {
		t.Errorf("a re-sync re-suspended an article the reader had queued")
	}
}

// TestSuspendPreservesProgress distinguishes suspension from the terminal
// grades: it is a pause, so the interval and repetition count must survive.
func TestSuspendPreservesProgress(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Long read", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if err := db.SaveSchedule(1, ir.Schedule{
		State: ir.StateReading, IntervalDays: 8, AFactor: 2.4, Reps: 3, Priority: 0.3,
		DueOn: now.AddDate(0, 0, 8),
	}, now); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	if err := db.SetReadBlock(1, 12); err != nil {
		t.Fatalf("SetReadBlock: %v", err)
	}

	if err := db.Suspend(1, now); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	element, err := db.ElementByID(1)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if element.Schedule.IntervalDays != 8 || element.Schedule.Reps != 3 {
		t.Errorf("suspending reset progress: interval %.0f, reps %d",
			element.Schedule.IntervalDays, element.Schedule.Reps)
	}
	if element.ReadBlock != 12 {
		t.Errorf("read point = %d, want 12", element.ReadBlock)
	}
	if !element.Schedule.DueOn.IsZero() {
		t.Errorf("suspended element kept a due date: %v", element.Schedule.DueOn)
	}
}

// TestHighlightsImportDuringSync is the path that rescues annotations on
// archived articles: they arrive with the listing rather than waiting for an
// article to be opened, which for archived material never happens.
func TestHighlightsImportDuringSync(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	document := source.Document{
		ExternalID: "1",
		Title:      "Archived but annotated",
		IsArchived: true,
		UpdatedAt:  now,
		Highlights: []source.Highlight{
			{ExternalID: "97418", Quote: "A passage worth keeping."},
			{ExternalID: "97419", Quote: "Another one."},
		},
	}

	result, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now)
	if err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	if result.Highlights != 2 {
		t.Errorf("imported %d highlights, want 2", result.Highlights)
	}

	extracts, err := db.ChildrenOf(1)
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if len(extracts) != 2 {
		t.Fatalf("got %d extracts, want 2", len(extracts))
	}
	for _, extract := range extracts {
		if extract.Origin != OriginImport {
			t.Errorf("origin = %q, want %q", extract.Origin, OriginImport)
		}
		// The listing carries no article text, so nothing can be located yet.
		if extract.HasRange {
			t.Error("an extract imported from a listing has a position it cannot have")
		}
	}

	// The article stays out of the queue, but its extracts are in it — which is
	// the entire point of importing them. The two now being separate queues,
	// that means absent from one and present in the other.
	queue, _ := db.Queue(now, QueueExtracts, 10)
	if len(queue) != 2 {
		t.Errorf("extract queue holds %d elements, want the two extracts", len(queue))
	}
	if articles, _ := db.Queue(now, QueueArticles, 10); len(articles) != 0 {
		t.Errorf("article queue holds %d elements, want none — the article is archived", len(articles))
	}

	// Re-syncing must not duplicate them.
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	extracts, _ = db.ChildrenOf(1)
	if len(extracts) != 2 {
		t.Errorf("got %d extracts after re-sync, want 2", len(extracts))
	}
}

// TestResyncBackfillsRangesOntoAnExistingHighlight covers the highlights
// already imported before the ranges column existed: re-importing the same
// external_ref never reaches the INSERT that would otherwise set it, so
// without a backfill on the existing-row path, those highlights would carry
// a NULL ranges forever and never get the one chance
// anchorHighlights' recovery fallback needs to reach them. A listing sync
// has annotation.Ranges even though it lacks the article body itself, so
// this can happen here rather than waiting for the article to be opened.
func TestResyncBackfillsRangesOntoAnExistingHighlight(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	document := source.Document{
		ExternalID: "1", Title: "Predates ranges tracking", UpdatedAt: now,
		Highlights: []source.Highlight{
			{ExternalID: "500", Quote: "A passage worth keeping."},
		},
	}
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	before, err := db.ChildrenOf(1)
	if err != nil || len(before) != 1 {
		t.Fatalf("ChildrenOf: got %d, err %v", len(before), err)
	}
	if before[0].Ranges != "" {
		t.Fatalf("test premise is wrong: ranges = %q, want empty", before[0].Ranges)
	}

	// The same highlight, same external_ref, seen again — but this time
	// carrying a ranges payload, exactly as a real re-sync against a wallabag
	// server would once ResolveRange's caller ships.
	document.Highlights[0].Ranges = []byte(`["stub-range"]`)
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("re-sync: %v", err)
	}

	after, err := db.ChildrenOf(1)
	if err != nil || len(after) != 1 {
		t.Fatalf("ChildrenOf after re-sync: got %d, err %v", len(after), err)
	}
	if after[0].ID != before[0].ID {
		t.Errorf("re-sync created a new row instead of updating %d: got %d", before[0].ID, after[0].ID)
	}
	if after[0].Ranges != `["stub-range"]` {
		t.Errorf("ranges = %q, want the backfilled payload", after[0].Ranges)
	}
	// Nothing else about the row should have moved.
	if after[0].Quote != before[0].Quote || after[0].HasRange != before[0].HasRange {
		t.Errorf("backfilling ranges touched something else: before %+v, after %+v", before[0], after[0])
	}

	// A third sync, ranges already set, must leave it alone rather than
	// erroring or churning a write every time.
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	final, _ := db.ChildrenOf(1)
	if len(final) != 1 || final[0].Ranges != `["stub-range"]` {
		t.Errorf("ranges did not stay stable across a third sync: %+v", final)
	}
}

// TestHighlightUnderANewRefAdoptsInsteadOfDuplicating guards the actual bug
// report: UpdateHighlightLocation replaces an annotation by creating a new
// one and best-effort deleting the old, and if that delete has not gone
// through by the next full listing, the same quote is reported twice —
// once under the ref already imported, once under a new one nothing local
// matches yet. Checking external_ref alone would treat the second as a
// brand new highlight and duplicate it.
func TestHighlightUnderANewRefAdoptsInsteadOfDuplicating(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	document := source.Document{
		ExternalID: "1", Title: "An article", UpdatedAt: now,
		Highlights: []source.Highlight{{ExternalID: "100", Quote: "A passage worth keeping."}},
	}
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("first import: %v", err)
	}

	extracts, _ := db.ChildrenOf(1)
	if len(extracts) != 1 {
		t.Fatalf("got %d extracts after first import, want 1", len(extracts))
	}
	firstID := extracts[0].ID

	// A full listing now reports the annotation replaced: same quote, a
	// newer ref (the old one, "100", is not in this listing at all — its
	// delete just hasn't completed).
	document.Highlights = []source.Highlight{{ExternalID: "200", Quote: "A passage worth keeping."}}
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("second import: %v", err)
	}

	extracts, _ = db.ChildrenOf(1)
	if len(extracts) != 1 {
		t.Fatalf("got %d extracts after the ref changed, want still 1 (adopted, not duplicated): %+v", len(extracts), extracts)
	}
	if extracts[0].ID != firstID {
		t.Errorf("a new row was created (id %d) instead of adopting the ref onto the original (id %d)",
			extracts[0].ID, firstID)
	}
	if extracts[0].ExternalRef != "200" {
		t.Errorf("external ref = %q, want the new one, 200", extracts[0].ExternalRef)
	}

	// Flag it missing (as ReconcileMissingHighlights would once the old ref
	// is confirmed gone), then have it show up again under yet another ref —
	// adopting must also clear the flag, since it demonstrably is not
	// missing if a current listing just reported it.
	if _, _, err := db.ReconcileMissingHighlights("wallabag", nil); err != nil {
		t.Fatalf("ReconcileMissingHighlights: %v", err)
	}
	if flagged, _ := db.ElementByID(firstID); !flagged.MissingUpstream {
		t.Fatal("fixture element was not flagged missing — test setup is wrong")
	}

	document.Highlights = []source.Highlight{{ExternalID: "300", Quote: "A passage worth keeping."}}
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("third import: %v", err)
	}
	extracts, _ = db.ChildrenOf(1)
	if len(extracts) != 1 {
		t.Fatalf("got %d extracts after the third ref change, want still 1", len(extracts))
	}
	if extracts[0].MissingUpstream {
		t.Error("adopting a new ref left the element flagged missing upstream")
	}
}

func TestExtractsBrowse(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "Source article", UpdatedAt: now,
		Highlights: []source.Highlight{{ExternalID: "1", Quote: "An imported passage."}},
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if _, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "A passage I took myself.",
		ContentHTML: "<p>A passage I took myself.</p>", Origin: OriginManual,
	}, now); err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	all, err := db.Extracts(ExtractFilter{})
	if err != nil {
		t.Fatalf("Extracts: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d extracts, want 2", len(all))
	}
	if all[0].DocumentTitle != "Source article" {
		t.Errorf("extract does not carry its article title")
	}

	imported, _ := db.Extracts(ExtractFilter{Origin: OriginImport})
	if len(imported) != 1 || imported[0].Quote != "An imported passage." {
		t.Errorf("origin filter returned %+v", imported)
	}

	found, _ := db.Extracts(ExtractFilter{Query: "took myself"})
	if len(found) != 1 {
		t.Errorf("text search returned %d, want 1", len(found))
	}

	// Root topics are not extracts and must never appear here.
	for _, extract := range all {
		if extract.IsRoot() {
			t.Error("a whole article was listed as an extract")
		}
	}
}

func TestExtractsSort(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "Source article", UpdatedAt: now,
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	// Created in this order: soon (due first, mid priority), far (due last,
	// most important), mid (due in between, least important) — so each sort
	// picks a different row first and the test can't pass by accident.
	soon, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "soon", ContentHTML: "<p>soon</p>",
		Priority: 0.5, DelayDays: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract(soon): %v", err)
	}
	far, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "far", ContentHTML: "<p>far</p>",
		Priority: 0.1, DelayDays: 30,
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract(far): %v", err)
	}
	mid, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "mid", ContentHTML: "<p>mid</p>",
		Priority: 0.9, DelayDays: 10,
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract(mid): %v", err)
	}

	wantIDs := func(rows []ExtractRow) []int64 {
		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		return ids
	}
	assertOrder := func(t *testing.T, sort string, want []int64) {
		t.Helper()
		got, err := db.Extracts(ExtractFilter{Sort: sort})
		if err != nil {
			t.Fatalf("Extracts(sort=%q): %v", sort, err)
		}
		gotIDs := wantIDs(got)
		if len(gotIDs) != len(want) {
			t.Fatalf("Extracts(sort=%q) = %v, want %v", sort, gotIDs, want)
		}
		for i := range want {
			if gotIDs[i] != want[i] {
				t.Errorf("Extracts(sort=%q) = %v, want %v", sort, gotIDs, want)
				return
			}
		}
	}

	assertOrder(t, "", []int64{mid, far, soon})
	assertOrder(t, "due", []int64{soon, mid, far})
	assertOrder(t, "priority", []int64{far, soon, mid})
	assertOrder(t, "oldest", []int64{soon, far, mid})
}

// TestDocumentImageCacheRoundTrip covers both outcomes an image fetch can
// have — see DocumentImage.OK — and that re-saving the same (document, url)
// pair updates in place rather than accumulating duplicate rows, since a
// re-fetched or corrected image should replace what was cached before.
func TestDocumentImageCacheRoundTrip(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "Has pictures", UpdatedAt: now,
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if _, found, err := db.CachedImage(1, "https://example.com/cat.png"); err != nil {
		t.Fatalf("CachedImage before any save: %v", err)
	} else if found {
		t.Fatalf("CachedImage before any save: found = true, want false")
	}

	id, err := db.SaveDocumentImage(1, "https://example.com/cat.png", "image/png", []byte("bytes"), true, 800, 600, now)
	if err != nil {
		t.Fatalf("SaveDocumentImage: %v", err)
	}

	byURL, found, err := db.CachedImage(1, "https://example.com/cat.png")
	if err != nil || !found {
		t.Fatalf("CachedImage after save: found=%v err=%v", found, err)
	}
	if byURL.ID != id || byURL.ContentType != "image/png" || string(byURL.Data) != "bytes" || !byURL.OK {
		t.Errorf("CachedImage after save = %+v", byURL)
	}
	if byURL.Width != 800 || byURL.Height != 600 {
		t.Errorf("CachedImage after save: dimensions = %dx%d, want 800x600", byURL.Width, byURL.Height)
	}

	byID, err := db.DocumentImageByID(id)
	if err != nil {
		t.Fatalf("DocumentImageByID: %v", err)
	}
	if byID.ID != byURL.ID || byID.URL != byURL.URL || byID.ContentType != byURL.ContentType ||
		string(byID.Data) != string(byURL.Data) || byID.OK != byURL.OK ||
		byID.Width != byURL.Width || byID.Height != byURL.Height {
		t.Errorf("DocumentImageByID = %+v, want %+v", byID, byURL)
	}

	// A failed fetch is cached too, with no data, so it is not retried on
	// every render — see resolveImages in the web package. It has nothing to
	// measure either, so its dimensions are the same "unknown" 0 as anything
	// else never measured.
	failedID, err := db.SaveDocumentImage(1, "https://example.com/broken.png", "", nil, false, 0, 0, now)
	if err != nil {
		t.Fatalf("SaveDocumentImage (failure): %v", err)
	}
	failed, found, err := db.CachedImage(1, "https://example.com/broken.png")
	if err != nil || !found {
		t.Fatalf("CachedImage (failure): found=%v err=%v", found, err)
	}
	if failed.OK || failed.ID != failedID {
		t.Errorf("CachedImage (failure) = %+v, want OK=false, ID=%d", failed, failedID)
	}
	if failed.Width != 0 || failed.Height != 0 {
		t.Errorf("CachedImage (failure): dimensions = %dx%d, want 0x0", failed.Width, failed.Height)
	}

	// Re-saving the same (document, url) updates the existing row rather
	// than inserting a second one for it — dimensions included, as a
	// re-fetch of a previously-unmeasurable image should be able to correct
	// them from 0 to a real size.
	updatedID, err := db.SaveDocumentImage(1, "https://example.com/cat.png", "image/webp", []byte("new-bytes"), true, 400, 300, now)
	if err != nil {
		t.Fatalf("SaveDocumentImage (update): %v", err)
	}
	if updatedID != id {
		t.Errorf("re-saving the same URL got a new id %d, want the original %d", updatedID, id)
	}
	updated, _, err := db.CachedImage(1, "https://example.com/cat.png")
	if err != nil {
		t.Fatalf("CachedImage after update: %v", err)
	}
	if updated.ContentType != "image/webp" || string(updated.Data) != "new-bytes" {
		t.Errorf("CachedImage after update = %+v, want the new content", updated)
	}
	if updated.Width != 400 || updated.Height != 300 {
		t.Errorf("CachedImage after update: dimensions = %dx%d, want 400x300", updated.Width, updated.Height)
	}

	if _, err := db.DocumentImageByID(999); !errors.Is(err, ErrNotFound) {
		t.Errorf("DocumentImageByID(999) = %v, want ErrNotFound", err)
	}
}

// TestDocumentImageDimensionsDefaultToUnknown covers migrations/011_image_dimensions.sql's
// central promise: a row written before that migration existed has no
// measurement to give, and reading it back must report that as 0, the same
// "unknown" value the rest of the stack already treats specially (see
// DocumentImage.Width) — not fail, and not synthesize a fake size. This
// inserts a row the way pre-011 code would have — leaving width and height
// out entirely — so the migration's column defaults are what supply the
// zeros, exactly as they would for a database upgraded from before this
// migration existed.
func TestDocumentImageDimensionsDefaultToUnknown(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "Predates dimension tracking", UpdatedAt: now,
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if _, err := db.db.Exec(
		`INSERT INTO document_images (document_id, url, content_type, data, ok, fetched_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		1, "https://example.com/old.png", "image/png", []byte("bytes"), 1, formatTime(now),
	); err != nil {
		t.Fatalf("insert pre-migration-style row: %v", err)
	}

	image, found, err := db.CachedImage(1, "https://example.com/old.png")
	if err != nil {
		t.Fatalf("CachedImage: %v", err)
	}
	if !found {
		t.Fatal("CachedImage: found = false, want true")
	}
	if image.Width != 0 || image.Height != 0 {
		t.Errorf("dimensions of a row with none recorded = %dx%d, want 0x0 (unknown)", image.Width, image.Height)
	}
}

// TestSetDocumentImageDimensionsPreservesEverythingElse guards the reason
// SetDocumentImageDimensions exists as its own statement instead of a call
// back through SaveDocumentImage: a later measurement of already-cached
// bytes is not a new fetch, so it must touch width and height only, leaving
// content_type, data and — most importantly — fetched_at exactly as they
// were. Resetting fetched_at here would make a plain measurement look like
// a brand new fetch that never actually happened.
func TestSetDocumentImageDimensionsPreservesEverythingElse(t *testing.T) {
	db := testStore(t)
	fetchedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "Has a legacy image", UpdatedAt: fetchedAt,
	}}, 0, 0, fetchedAt); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	id, err := db.SaveDocumentImage(1, "https://example.com/old.png", "image/png",
		[]byte("bytes"), true, 0, 0, fetchedAt)
	if err != nil {
		t.Fatalf("SaveDocumentImage: %v", err)
	}

	if err := db.SetDocumentImageDimensions(id, 20, 10); err != nil {
		t.Fatalf("SetDocumentImageDimensions: %v", err)
	}

	image, found, err := db.CachedImage(1, "https://example.com/old.png")
	if err != nil || !found {
		t.Fatalf("CachedImage: found=%v err=%v", found, err)
	}
	if image.Width != 20 || image.Height != 10 {
		t.Errorf("dimensions = %dx%d, want 20x10", image.Width, image.Height)
	}
	if string(image.Data) != "bytes" || image.ContentType != "image/png" || !image.OK {
		t.Errorf("SetDocumentImageDimensions touched something other than the dimensions: %+v", image)
	}

	var storedFetchedAt string
	if err := db.db.QueryRow(`SELECT fetched_at FROM document_images WHERE id = ?`, id).Scan(&storedFetchedAt); err != nil {
		t.Fatalf("read fetched_at: %v", err)
	}
	if want := fetchedAt.UTC().Format(timeFormat); storedFetchedAt != want {
		t.Errorf("fetched_at = %q, want unchanged at %q", storedFetchedAt, want)
	}
}

// TestQueueSeparatesArticlesFromExtracts is the split itself: each queue
// returns its own half of the elements table and nothing of the other's.
//
// This replaces the interleave the queue used to perform, which blended the
// two in proportion to how many of each were due. What it guards now is the
// property that made that machinery unnecessary — a reader asking for articles
// gets articles, so no ranking column has to hold the two populations apart.
func TestQueueSeparatesArticlesFromExtracts(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	// Articles first, as a sync would insert them, then extracts — so ids
	// group by kind exactly the way the real database does.
	documents := make([]source.Document, 0, 10)
	for i := 1; i <= 10; i++ {
		documents = append(documents, source.Document{
			ExternalID: strconv.Itoa(i),
			Title:      "Article " + strconv.Itoa(i),
			UpdatedAt:  now,
		})
	}
	if _, err := db.UpsertDocuments("wallabag", documents, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	for i := 1; i <= 10; i++ {
		if _, err := db.CreateExtract(NewExtract{
			ParentID: int64(i), DocumentID: int64(i),
			Quote: "Extract " + strconv.Itoa(i), Priority: 0.5,
		}, now); err != nil {
			t.Fatalf("CreateExtract: %v", err)
		}
	}

	articles, err := db.Queue(now, QueueArticles, 50)
	if err != nil {
		t.Fatalf("Queue(articles): %v", err)
	}
	if len(articles) != 10 {
		t.Fatalf("article queue holds %d, want 10", len(articles))
	}
	for _, item := range articles {
		if !item.IsRoot() {
			t.Errorf("extract %d is in the article queue", item.ID)
		}
	}

	extracts, err := db.Queue(now, QueueExtracts, 50)
	if err != nil {
		t.Fatalf("Queue(extracts): %v", err)
	}
	if len(extracts) != 10 {
		t.Fatalf("extract queue holds %d, want 10", len(extracts))
	}
	for _, item := range extracts {
		if item.IsRoot() {
			t.Errorf("article %d is in the extract queue", item.ID)
		}
	}

	// Deterministic: the same query must not reshuffle between page loads.
	again, _ := db.Queue(now, QueueArticles, 50)
	for i := range articles {
		if articles[i].ID != again[i].ID {
			t.Fatalf("queue order changed between reads at position %d", i)
		}
	}
}

// TestQueueRejectsAnUnknownKind: the two kinds partition the table, so a third
// value is a caller bug rather than a request for everything. Answering it
// with the whole table would silently undo the split.
func TestQueueRejectsAnUnknownKind(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if _, err := db.Queue(now, QueueKind("everything"), 10); err == nil {
		t.Error("Queue accepted an unknown kind")
	}
	if _, err := db.CountDue(now, QueueKind("")); err == nil {
		t.Error("CountDue accepted an empty kind — the store takes a resolved kind, not a request value")
	}
}

// TestExtractQueueHoldsBookAnnotations: a book's own root topic is always
// suspended, because there is no body to read, so a book reaches the reader
// only through the extract queue. Its passages arrive suspended too, awaiting
// triage — and the moment a triage pass keeps one, it has to show up there.
//
// This is the case the split is most easily got wrong for: scoping the extract
// queue to "extracts of articles", or joining through a parent that has to be
// in circulation itself, would drop every book passage on the floor and leave
// the queue looking empty for the material there is most of.
func TestExtractQueueHoldsBookAnnotations(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	work := source.Document{
		ExternalID: "book-1",
		Title:      "A book",
		Author:     "An author",
		UpdatedAt:  now,
		Highlights: []source.Highlight{
			{ExternalID: "b1", Quote: "A passage from chapter one.", Chapter: "One", Ordinal: 1},
			{ExternalID: "b2", Quote: "A passage from chapter two.", Chapter: "Two", Ordinal: 2},
		},
	}
	// Triage: how an upload of a whole book arrives — parked, not queued.
	result, err := db.ImportAnnotations(work, ImportOptions{Triage: true}, now)
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}

	// Nothing is in either queue yet: the root has no body, the passages are
	// parked until triaged.
	if queue, _ := db.Queue(now, QueueArticles, 10); len(queue) != 0 {
		t.Errorf("article queue holds %d, want none — a book has no readable root", len(queue))
	}
	if queue, _ := db.Queue(now, QueueExtracts, 10); len(queue) != 0 {
		t.Errorf("extract queue holds %d, want none before triage", len(queue))
	}

	annotations, err := db.DocumentAnnotations(result.DocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if len(annotations) != 2 {
		t.Fatalf("got %d annotations, want 2", len(annotations))
	}

	// A triage pass keeps one, through the same call the triage page makes.
	// State back to new is what ends the suspension an untriaged import starts
	// in — see triageSchedule, which this mirrors.
	kept := annotations[0]
	schedule := ir.Backlog(kept.Schedule, 0, now)
	schedule.State = ir.StateNew
	if err := db.KeepTriaged(kept.ID, schedule, now); err != nil {
		t.Fatalf("KeepTriaged: %v", err)
	}

	queue, err := db.Queue(now, QueueExtracts, 10)
	if err != nil {
		t.Fatalf("Queue(extracts): %v", err)
	}
	if len(queue) != 1 {
		t.Fatalf("extract queue holds %d, want the kept book passage", len(queue))
	}
	if queue[0].ID != kept.ID {
		t.Errorf("queued element %d, want the kept book annotation %d", queue[0].ID, kept.ID)
	}
	if queue[0].DocumentTitle != "A book" {
		t.Errorf("queued passage reports document %q, want %q", queue[0].DocumentTitle, "A book")
	}
}

// TestQueueOrderIsStableAcrossRemovals guards the property that retired
// queue_rank. Ordering used to be computed against the rest of the due
// population — a fraction whose denominator shrank every time anything was
// graded away — so removing one element could jump two untouched ones past
// each other. Every ordering term is now a value on the row itself, and this
// is the test that says so: grade the head of the queue away, and everything
// behind it must keep its exact relative order.
func TestQueueOrderIsStableAcrossRemovals(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	highlights := make([]source.Highlight, 0, 7)
	for i := 1; i <= 7; i++ {
		highlights = append(highlights, source.Highlight{
			ExternalID: "h" + strconv.Itoa(i),
			Quote:      "Highlight " + strconv.Itoa(i),
			UpdatedAt:  now,
		})
	}
	documents := []source.Document{
		{ExternalID: "1", Title: "Article 1", UpdatedAt: now},
		{ExternalID: "2", Title: "Article 2", UpdatedAt: now},
		{ExternalID: "3", Title: "Annotated article", UpdatedAt: now, Highlights: highlights, IsArchived: true},
	}
	if _, err := db.UpsertDocuments("wallabag", documents, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	before, err := db.Queue(now, QueueExtracts, 30)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(before) != 7 {
		t.Fatalf("got %d queued, want the 7 highlights", len(before))
	}

	// Grade away the head of the queue, exactly like pressing "Next" on the
	// top item.
	head := before[0].Element
	graded := ir.Next(head.Schedule, ir.GradeNext, now, head.ID)
	if err := db.SaveSchedule(head.ID, graded, now); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}

	after, err := db.Queue(now, QueueExtracts, 30)
	if err != nil {
		t.Fatalf("Queue (after grading): %v", err)
	}
	if len(after) != 6 {
		t.Fatalf("got %d queued after grading, want 6", len(after))
	}

	for i, item := range after {
		if item.ID != before[i+1].ID {
			t.Fatalf("position %d holds element %d, want %d — grading the head "+
				"reordered what was behind it", i, item.ID, before[i+1].ID)
		}
	}
}

// TestBuryIsPerQueue: "later today" moves an element to the back of its own
// queue and must leave the other one alone. The bury terms live inside the
// kind-filtered read, so skipping through a pile of extracts cannot disturb
// the order a reading session was working through.
func TestBuryIsPerQueue(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	highlights := make([]source.Highlight, 0, 3)
	for i := 1; i <= 3; i++ {
		highlights = append(highlights, source.Highlight{
			ExternalID: "h" + strconv.Itoa(i),
			Quote:      "Highlight " + strconv.Itoa(i),
			UpdatedAt:  now,
		})
	}
	documents := []source.Document{
		{ExternalID: "1", Title: "Article 1", UpdatedAt: now},
		{ExternalID: "2", Title: "Article 2", UpdatedAt: now},
		{ExternalID: "3", Title: "Annotated", UpdatedAt: now, Highlights: highlights, IsArchived: true},
	}
	if _, err := db.UpsertDocuments("wallabag", documents, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	ids := func(items []QueueItem) []int64 {
		out := make([]int64, len(items))
		for i, item := range items {
			out[i] = item.ID
		}
		return out
	}

	articlesBefore, _ := db.Queue(now, QueueArticles, 10)
	extractsBefore, _ := db.Queue(now, QueueExtracts, 10)
	if len(articlesBefore) != 2 || len(extractsBefore) != 3 {
		t.Fatalf("got %d articles and %d extracts, want 2 and 3",
			len(articlesBefore), len(extractsBefore))
	}
	wantArticles := ids(articlesBefore)

	// Skip the head of the extract queue.
	buried := extractsBefore[0].ID
	if err := db.Bury(buried, now.Add(time.Second)); err != nil {
		t.Fatalf("Bury: %v", err)
	}

	extractsAfter, _ := db.Queue(now, QueueExtracts, 10)
	if len(extractsAfter) != 3 {
		t.Fatalf("burying dropped an extract from today: %d remain", len(extractsAfter))
	}
	if extractsAfter[len(extractsAfter)-1].ID != buried {
		t.Errorf("buried extract sits at position %d of its queue, want last",
			indexOfElement(extractsAfter, buried))
	}

	// The reading queue never saw it.
	articlesAfter, _ := db.Queue(now, QueueArticles, 10)
	got := ids(articlesAfter)
	if len(got) != len(wantArticles) {
		t.Fatalf("article queue changed size after burying an extract: %v, want %v",
			got, wantArticles)
	}
	for i := range wantArticles {
		if got[i] != wantArticles[i] {
			t.Fatalf("article queue reordered after burying an extract: %v, want %v",
				got, wantArticles)
		}
	}
}

// TestWriteIsQueuedWithTheLocalChange is the guarantee the outbox exists for:
// the local state and the intent to publish it commit together, so they cannot
// disagree.
func TestWriteIsQueuedWithTheLocalChange(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if err := db.SetArchived(1, "wallabag", "77", true, now); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}

	document, _ := db.DocumentByID(1)
	if !document.IsArchived {
		t.Error("the local column was not updated")
	}

	writes, err := db.PendingWrites("wallabag", 10)
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if len(writes) != 1 {
		t.Fatalf("got %d queued writes, want 1", len(writes))
	}
	if writes[0].Operation != OpArchive || writes[0].ExternalID != "77" {
		t.Errorf("queued %+v, want an archive write for entry 77", writes[0])
	}
	if !PayloadBool(writes[0].Payload) {
		t.Error("payload does not say archived")
	}
}

// TestManualExtractQueuesHighlightPush is the counterpart to an imported
// highlight already carrying its external_ref: a passage taken manually
// inside increader should reach wallabag too, not only the reverse.
func TestManualExtractQueuesHighlightPush(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	id, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "A passage I took myself.",
		ContentHTML: "<p>A passage I took myself.</p>", Origin: OriginManual,
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	writes, err := db.PendingWrites("wallabag", 10)
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if len(writes) != 1 {
		t.Fatalf("got %d queued writes, want 1", len(writes))
	}
	write := writes[0]
	if write.Operation != OpHighlightCreate || write.ExternalID != "77" {
		t.Errorf("queued %+v, want a highlight_create write for entry 77", write)
	}
	if write.Payload != "A passage I took myself." {
		t.Errorf("payload = %q, want the extract's quote", write.Payload)
	}
	if !write.ElementID.Valid || write.ElementID.Int64 != id {
		t.Errorf("ElementID = %+v, want it to point at the new extract %d", write.ElementID, id)
	}
}

// TestExtractsExcludedFromHighlightPush covers the two ways an extract must
// not queue a highlight_create: a cloze, which has no wallabag equivalent at
// all, and one that arrived already carrying an external_ref — an imported
// highlight, which already exists upstream and would otherwise be recreated.
func TestExtractsExcludedFromHighlightPush(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	extractID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Parent passage.",
		ContentHTML: "<p>Parent passage.</p>", Origin: OriginManual,
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract (parent): %v", err)
	}
	if _, err := db.CreateExtract(NewExtract{
		ParentID: extractID, DocumentID: 1, Kind: KindItem, Quote: "cloze text",
		ContentHTML: "<p>cloze text</p>", Origin: OriginManual,
	}, now); err != nil {
		t.Fatalf("CreateExtract (cloze): %v", err)
	}
	if _, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Already in wallabag.",
		ContentHTML: "<p>Already in wallabag.</p>",
		Origin:      OriginImport, ExternalRef: "999",
	}, now); err != nil {
		t.Fatalf("CreateExtract (imported): %v", err)
	}

	writes, err := db.PendingWrites("wallabag", 10)
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	// Exactly the one write from the manual parent extract above; neither the
	// cloze nor the already-imported extract should have queued anything.
	if len(writes) != 1 || writes[0].Payload != "Parent passage." {
		t.Errorf("queued writes = %+v, want only the manual topic extract's push", writes)
	}
}

// TestArchivingLocallyDoesNotRetriggerTheSyncTransition is the interaction that
// would otherwise demote a finished article: writing the archive flag locally
// means the next sync sees no change, so M6's transition does not fire.
func TestArchivingLocallyDoesNotRetriggerTheSyncTransition(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	document := source.Document{ExternalID: "77", Title: "An article", UpdatedAt: now}
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	// Mark it done, as the reader would.
	if err := db.SaveSchedule(1, ir.Schedule{State: ir.StateDone}, now); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	if err := db.SetArchived(1, "wallabag", "77", true, now); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}

	// wallabag now reports it archived, because increader archived it.
	document.IsArchived = true
	document.UpdatedAt = now.Add(time.Hour)
	result, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if result.Suspended != 0 {
		t.Errorf("the sync treated increader's own write as a new transition")
	}

	element, _ := db.ElementByID(1)
	if element.Schedule.State != ir.StateDone {
		t.Errorf("state = %q, want %q — a finished article was demoted",
			element.Schedule.State, ir.StateDone)
	}
}

// TestQueuedWritesSupersede keeps the outbox from replaying states the reader
// never asked to be in: archiving, unarchiving and archiving again is one final
// state, not three requests.
func TestQueuedWritesSupersede(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	for _, archived := range []bool{true, false, true} {
		if err := db.SetArchived(1, "wallabag", "77", archived, now); err != nil {
			t.Fatalf("SetArchived(%v): %v", archived, err)
		}
	}

	writes, _ := db.PendingWrites("wallabag", 10)
	if len(writes) != 1 {
		t.Fatalf("got %d queued writes, want 1 superseding the rest", len(writes))
	}
	if !PayloadBool(writes[0].Payload) {
		t.Error("the surviving write is not the final state")
	}

	// A different operation on the same entry is independent.
	if err := db.SetStarred(1, "wallabag", "77", true, now); err != nil {
		t.Fatalf("SetStarred: %v", err)
	}
	writes, _ = db.PendingWrites("wallabag", 10)
	if len(writes) != 2 {
		t.Errorf("starring superseded the archive write; got %d", len(writes))
	}
}

// TestTagWritesDoNotSupersedeDifferentTags guards the other direction: two tag
// additions are two distinct facts, unlike two archive flags.
func TestTagWritesDoNotSupersedeDifferentTags(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	for _, label := range []string{"philosophy", "to-reread"} {
		if err := db.AttachTag(1, "wallabag", "77", label); err != nil {
			t.Fatalf("AttachTag(%q): %v", label, err)
		}
	}

	writes, _ := db.PendingWrites("wallabag", 10)
	if len(writes) != 2 {
		t.Errorf("got %d queued tag writes, want 2", len(writes))
	}

	tags, err := db.TagsOf(1)
	if err != nil {
		t.Fatalf("TagsOf: %v", err)
	}
	if len(tags) != 2 {
		t.Errorf("got %v locally, want both tags", tags)
	}

	// Adding the same tag twice queues one write, not two.
	if err := db.AttachTag(1, "wallabag", "77", "philosophy"); err != nil {
		t.Fatalf("AttachTag repeat: %v", err)
	}
	writes, _ = db.PendingWrites("wallabag", 10)
	if len(writes) != 2 {
		t.Errorf("re-adding a tag queued a duplicate write; got %d", len(writes))
	}
}

func TestFailedWritesRetryThenStop(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	if err := db.SetArchived(1, "wallabag", "77", true, now); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}

	writes, _ := db.PendingWrites("wallabag", 10)
	id := writes[0].ID

	for attempt := 1; attempt <= maxWriteAttempts; attempt++ {
		if err := db.FailWrite(id, errors.New("wallabag unreachable")); err != nil {
			t.Fatalf("FailWrite: %v", err)
		}
	}

	// Exhausted, so it stops being retried...
	writes, _ = db.PendingWrites("wallabag", 10)
	if len(writes) != 0 {
		t.Errorf("an exhausted write is still being retried")
	}

	// ...but is not silently discarded.
	queued, abandoned, err := db.CountPendingWrites("wallabag")
	if err != nil {
		t.Fatalf("CountPendingWrites: %v", err)
	}
	if queued != 0 || abandoned != 1 {
		t.Errorf("got %d queued / %d abandoned, want 0 / 1", queued, abandoned)
	}
}

func TestTagsSyncFromProvider(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	document := source.Document{
		ExternalID: "77", Title: "Tagged", UpdatedAt: now,
		Tags: []string{"philosophy", "long-read"}, ReadingTime: 21,
	}
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	tags, _ := db.TagsOf(1)
	if len(tags) != 2 {
		t.Fatalf("got %v, want both tags", tags)
	}

	stored, _ := db.DocumentByID(1)
	if stored.ReadingTime != 21 {
		t.Errorf("reading time = %d, want 21", stored.ReadingTime)
	}

	// A tag removed upstream must disappear here. The listing is authoritative,
	// so merging rather than replacing would strand it forever.
	document.Tags = []string{"philosophy"}
	document.UpdatedAt = now.Add(time.Hour)
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, 0, now); err != nil {
		t.Fatalf("re-sync: %v", err)
	}

	tags, _ = db.TagsOf(1)
	if len(tags) != 1 || tags[0] != "philosophy" {
		t.Errorf("got %v, want just philosophy after the upstream removal", tags)
	}
}

func TestLibraryFilters(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Unread one", UpdatedAt: now, Tags: []string{"philosophy"}},
		{ExternalID: "2", Title: "Starred one", UpdatedAt: now, IsStarred: true},
		{ExternalID: "3", Title: "Archived one", UpdatedAt: now, IsArchived: true},
		{ExternalID: "4", Title: "Annotated one", UpdatedAt: now, IsArchived: true,
			Highlights: []source.Highlight{{ExternalID: "9", Quote: "A passage."}}},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	tests := []struct {
		filter LibraryFilter
		want   int
	}{
		{LibraryFilter{}, 4},
		{LibraryFilter{State: "unread"}, 2},
		{LibraryFilter{State: "starred"}, 1},
		{LibraryFilter{State: "archived"}, 2},
		{LibraryFilter{State: "annotated"}, 1},
		{LibraryFilter{Tag: "philosophy"}, 1},
		{LibraryFilter{Query: "Starred"}, 1},
		{LibraryFilter{State: "archived", Query: "Annotated"}, 1},
	}
	for _, test := range tests {
		got, err := db.SearchDocuments(test.filter, now)
		if err != nil {
			t.Fatalf("SearchDocuments(%+v): %v", test.filter, err)
		}
		if len(got) != test.want {
			t.Errorf("filter %+v returned %d, want %d", test.filter, len(got), test.want)
		}
	}

	counts, err := db.CountByState("wallabag", now)
	if err != nil {
		t.Fatalf("CountByState: %v", err)
	}
	for key, want := range map[string]int{
		"all": 4, "unread": 2, "starred": 1, "archived": 2, "annotated": 1,
	} {
		if counts[key] != want {
			t.Errorf("count %q = %d, want %d", key, counts[key], want)
		}
	}
}

// TestBurySinksWithinTodayAndClearsItself covers the skip case: an element
// pushed aside must come back at the bottom of the same day's queue, not
// disappear until tomorrow.
func TestBurySinksWithinTodayAndClearsItself(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	documents := []source.Document{}
	for i := 1; i <= 4; i++ {
		documents = append(documents, source.Document{
			ExternalID: strconv.Itoa(i),
			Title:      "Article " + strconv.Itoa(i),
			UpdatedAt:  now,
		})
	}
	if _, err := db.UpsertDocuments("wallabag", documents, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	first, _ := db.Queue(now, QueueArticles, 10)
	buried := first[0].ID

	if err := db.Bury(buried, now); err != nil {
		t.Fatalf("Bury: %v", err)
	}

	after, _ := db.Queue(now, QueueArticles, 10)
	if len(after) != 4 {
		t.Fatalf("burying removed the element from today: %d remain", len(after))
	}
	if after[len(after)-1].ID != buried {
		t.Errorf("buried element is at position %d, want last",
			indexOfElement(after, buried))
	}

	// Tomorrow it is ordinary again — the date stops matching, so nothing has
	// to be cleared and nothing can stay buried by accident.
	tomorrow, _ := db.Queue(now.AddDate(0, 0, 1), QueueArticles, 10)
	if tomorrow[len(tomorrow)-1].ID == buried && len(tomorrow) > 1 {
		if first[0].ID == buried {
			t.Error("the element is still sorted last a day after being buried")
		}
	}
}

// TestRepeatedBuryIsARoundRobinNotAFixedCycle guards the actual bug: once
// every due element had been buried once, the buried bucket's internal order
// collapsed back to plain queue_rank, so every further "skip" replayed the
// exact same sequence from the top forever — pressing Later could never
// actually get anywhere new. buried_at fixes that by ordering the bucket on
// when each element was (most recently) buried, not queue_rank.
func TestRepeatedBuryIsARoundRobinNotAFixedCycle(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	documents := []source.Document{}
	for i := 1; i <= 4; i++ {
		documents = append(documents, source.Document{
			ExternalID: strconv.Itoa(i), Title: "Article " + strconv.Itoa(i), UpdatedAt: now,
		})
	}
	if _, err := db.UpsertDocuments("wallabag", documents, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	first, err := db.Queue(now, QueueArticles, 10)
	if err != nil || len(first) != 4 {
		t.Fatalf("Queue: got %d elements, err %v", len(first), err)
	}
	e0, e1, e2, e3 := first[0].ID, first[1].ID, first[2].ID, first[3].ID

	// Bury the first two, in order, each at a distinct later moment.
	if err := db.Bury(e0, now.Add(1*time.Second)); err != nil {
		t.Fatalf("Bury e0: %v", err)
	}
	if err := db.Bury(e1, now.Add(2*time.Second)); err != nil {
		t.Fatalf("Bury e1: %v", err)
	}

	assertOrder := func(t *testing.T, want []int64) {
		t.Helper()
		got, err := db.Queue(now, QueueArticles, 10)
		if err != nil {
			t.Fatalf("Queue: %v", err)
		}
		gotIDs := make([]int64, len(got))
		for i, item := range got {
			gotIDs[i] = item.ID
		}
		same := len(gotIDs) == len(want)
		for i := range want {
			if same && gotIDs[i] != want[i] {
				same = false
			}
		}
		if !same {
			t.Fatalf("queue order = %v, want %v", gotIDs, want)
		}
	}
	assertOrder(t, []int64{e2, e3, e0, e1})

	// Bury e0 again — it is already in today's buried bucket, so a naive
	// implementation (comparing only the date) would leave it exactly where
	// it was. It must instead jump past e1, which has not been touched
	// since: a second skip has to actually go somewhere.
	if err := db.Bury(e0, now.Add(3*time.Second)); err != nil {
		t.Fatalf("Bury e0 again: %v", err)
	}
	assertOrder(t, []int64{e2, e3, e1, e0})
}

func indexOfElement(items []QueueItem, id int64) int {
	for i, item := range items {
		if item.ID == id {
			return i
		}
	}
	return -1
}

// TestExtractsAreDueLater is the change of mind about M3's "due immediately":
// the value of an extract is re-reading it once the article has faded.
func TestExtractsAreDueLater(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	id, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "A passage.", DelayDays: 10,
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	extract, _ := db.ElementByID(id)
	// Fuzzed a little rather than exactly 10 days out — see
	// ir.FuzzedFirstDueDays — so the expectation is computed the same way
	// CreateExtract itself computes it, from the same seed.
	fuzzedDelay := ir.FuzzedFirstDueDays(extractSeed(1, "A passage."), 10)
	want := ir.Day(now.AddDate(0, 0, fuzzedDelay))
	if !ir.Day(extract.Schedule.DueOn).Equal(want) {
		t.Errorf("due %v, want %v", extract.Schedule.DueOn, want)
	}

	// It is therefore not in today's queue — only the article is.
	queue, _ := db.Queue(now, QueueArticles, 10)
	if len(queue) != 1 {
		t.Errorf("queue holds %d elements, want only the article", len(queue))
	}
}

// dueDayCounts groups every imported extract's due_on by date, for the two
// spread tests below.
func dueDayCounts(t *testing.T, db *Store) (days, largest int) {
	t.Helper()
	rows, err := db.db.Query(`
		SELECT due_on, COUNT(*) FROM elements
		WHERE origin = 'import' GROUP BY due_on ORDER BY due_on`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var date string
		var count int
		if err := rows.Scan(&date, &count); err != nil {
			t.Fatalf("scan: %v", err)
		}
		days++
		if count > largest {
			largest = count
		}
	}
	return days, largest
}

// TestImportedHighlightsSpreadAcrossTheWindow: a library's import is hundreds
// of highlights at once, and putting them all on one date moves the pile
// rather than clearing it. One highlight per document, across many documents —
// see TestImportedHighlightsFromOneDocumentSpread for the same property
// within a single document, which is the case a per-document seed used to
// get wrong.
func TestImportedHighlightsSpreadAcrossTheWindow(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	documents := make([]source.Document, 0, 40)
	for i := 1; i <= 40; i++ {
		documents = append(documents, source.Document{
			ExternalID: strconv.Itoa(i),
			Title:      "Article " + strconv.Itoa(i),
			UpdatedAt:  now,
			Highlights: []source.Highlight{
				{ExternalID: "h" + strconv.Itoa(i), Quote: "A passage worth keeping."},
			},
		})
	}
	const floor, spread = 10, 30
	if _, err := db.UpsertDocuments("wallabag", documents, floor, spread, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	days, largest := dueDayCounts(t, db)
	if days < 5 {
		t.Errorf("40 highlights landed on %d distinct days; they are not spread", days)
	}
	if largest > 15 {
		t.Errorf("the busiest day holds %d of 40 highlights, want them spread", largest)
	}

	// And none of them are due today, which is the point of the floor.
	due, _ := db.CountDue(now, QueueArticles)
	if due != 40 {
		t.Errorf("%d due today, want just the 40 articles", due)
	}
}

// TestImportedHighlightsFromOneDocumentSpread guards the actual bug report:
// spreadOffset used to be seeded on documentID alone, which is the same value
// for every highlight in one document's import — so a single heavily
// annotated article, or a single book, landed every one of its passages on
// the identical due date no matter how wide the configured window was. The
// seed is per highlight now (document plus its own external ref), so this is
// the case TestImportedHighlightsSpreadAcrossTheWindow's one-highlight-per-
// document setup could never have caught.
func TestImportedHighlightsFromOneDocumentSpread(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	highlights := make([]source.Highlight, 0, 50)
	for i := 1; i <= 50; i++ {
		highlights = append(highlights, source.Highlight{
			ExternalID: "h" + strconv.Itoa(i),
			Quote:      "Passage " + strconv.Itoa(i),
		})
	}
	documents := []source.Document{
		{ExternalID: "book", Title: "One heavily annotated piece", UpdatedAt: now,
			Highlights: highlights, IsArchived: true},
	}
	const floor, spread = 30, 60
	if _, err := db.UpsertDocuments("wallabag", documents, floor, spread, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	days, largest := dueDayCounts(t, db)
	if days < 10 {
		t.Errorf("50 highlights from one document landed on %d distinct days; want a real spread", days)
	}
	if largest > 10 {
		t.Errorf("the busiest day holds %d of 50 highlights from one document, want them spread", largest)
	}
}

// TestImportedHighlightsRespectTheFloor: nothing imported shows up before
// annotation_delay_days, no matter how the spread happens to land.
func TestImportedHighlightsRespectTheFloor(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	highlights := make([]source.Highlight, 0, 30)
	for i := 1; i <= 30; i++ {
		highlights = append(highlights, source.Highlight{
			ExternalID: "h" + strconv.Itoa(i), Quote: "Passage " + strconv.Itoa(i),
		})
	}
	documents := []source.Document{
		{ExternalID: "book", Title: "A book", UpdatedAt: now, Highlights: highlights, IsArchived: true},
	}
	const floor, spread = 30, 60
	if _, err := db.UpsertDocuments("wallabag", documents, floor, spread, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	rows, err := db.db.Query(`SELECT due_on FROM elements WHERE origin = 'import'`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	floorDate := ir.Day(now).AddDate(0, 0, floor)
	ceilDate := ir.Day(now).AddDate(0, 0, floor+spread)
	count := 0
	for rows.Next() {
		var date string
		if err := rows.Scan(&date); err != nil {
			t.Fatalf("scan: %v", err)
		}
		due, err := time.ParseInLocation(dateFormat, date, time.Local)
		if err != nil {
			t.Fatalf("parse due_on %q: %v", date, err)
		}
		if due.Before(floorDate) {
			t.Errorf("highlight due %v, before the floor %v", due, floorDate)
		}
		if !due.Before(ceilDate) {
			t.Errorf("highlight due %v, at or past floor+spread %v", due, ceilDate)
		}
		count++
	}
	if count != 30 {
		t.Fatalf("checked %d rows, want 30", count)
	}
}

// TestDeleteExtractCascades: an extract's own children, clozes and export
// ledger rows must all go with it — none of it should orphan on delete.
func TestDeleteExtractCascades(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	extractID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "A passage worth keeping.",
		ContentHTML: "<p>A passage worth keeping.</p>",
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}
	if _, err := db.AddCloze(extractID, 2, 9, ""); err != nil {
		t.Fatalf("AddCloze: %v", err)
	}

	if err := db.DeleteExtract(extractID); err != nil {
		t.Fatalf("DeleteExtract: %v", err)
	}

	if _, err := db.ElementByID(extractID); !errors.Is(err, ErrNotFound) {
		t.Errorf("ElementByID after delete: %v, want ErrNotFound", err)
	}
	clozes, err := db.ClozesOf(extractID)
	if err != nil {
		t.Fatalf("ClozesOf: %v", err)
	}
	if len(clozes) != 0 {
		t.Errorf("got %d clozes surviving the deleted extract, want 0", len(clozes))
	}
}

// TestDeleteClozeRemovesOnlyThatOne guards the ordinal-addressed deletion
// against the obvious way to get it wrong: removing cloze 2 out of {1, 2, 3}
// must leave 1 and 3 both present and unrenumbered — Anki has no trouble
// with a gap in ordinals, but silently shifting 3 down to 2 would rewrite a
// deletion nobody asked to change.
func TestDeleteClozeRemovesOnlyThatOne(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	extractID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "one two three four five six",
		ContentHTML: "<p>one two three four five six</p>",
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}
	for _, span := range [][2]int{{0, 3}, {4, 7}, {8, 13}} {
		if _, err := db.AddCloze(extractID, span[0], span[1], ""); err != nil {
			t.Fatalf("AddCloze: %v", err)
		}
	}

	if err := db.DeleteCloze(extractID, 2); err != nil {
		t.Fatalf("DeleteCloze: %v", err)
	}

	clozes, err := db.ClozesOf(extractID)
	if err != nil {
		t.Fatalf("ClozesOf: %v", err)
	}
	if len(clozes) != 2 || clozes[0].Ordinal != 1 || clozes[1].Ordinal != 3 {
		t.Errorf("clozes = %+v, want ordinals 1 and 3 surviving, unrenumbered", clozes)
	}
}

func TestDeleteClozeMissing(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	extractID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "A passage.",
		ContentHTML: "<p>A passage.</p>",
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	if err := db.DeleteCloze(extractID, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteCloze on an extract with no clozes = %v, want ErrNotFound", err)
	}
}

// TestDeleteExtractQueuesHighlightRemoval is the pairing that keeps a deletion
// from silently undoing itself: without the queued upstream removal, the next
// sync would re-import the very highlight just deleted.
func TestDeleteExtractQueuesHighlightRemoval(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "An article", UpdatedAt: now,
		Highlights: []source.Highlight{{ExternalID: "97418", Quote: "A passage."}},
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	extracts, _ := db.ChildrenOf(1)
	if len(extracts) != 1 {
		t.Fatalf("got %d extracts, want 1", len(extracts))
	}

	if err := db.DeleteExtract(extracts[0].ID); err != nil {
		t.Fatalf("DeleteExtract: %v", err)
	}

	writes, err := db.PendingWrites("wallabag", 10)
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if len(writes) != 1 {
		t.Fatalf("got %d queued writes, want 1", len(writes))
	}
	if writes[0].Operation != OpHighlightDelete || writes[0].ExternalID != "97418" {
		t.Errorf("queued %+v, want a highlight_delete for annotation 97418", writes[0])
	}
}

// TestDeleteExtractOnManualOriginQueuesNothing: a hand-made extract has no
// upstream counterpart, so nothing should be queued for it.
func TestDeleteExtractOnManualOriginQueuesNothing(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	extractID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Mine.", ContentHTML: "<p>Mine.</p>",
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	if err := db.DeleteExtract(extractID); err != nil {
		t.Fatalf("DeleteExtract: %v", err)
	}

	writes, _ := db.PendingWrites("wallabag", 10)
	if len(writes) != 0 {
		t.Errorf("got %d queued writes for a manual extract, want 0", len(writes))
	}
}

func TestDeleteExtractRejectsRootTopic(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if err := db.DeleteExtract(1); err == nil {
		t.Fatal("expected an error deleting a root topic, got nil")
	}

	if _, err := db.ElementByID(1); err != nil {
		t.Errorf("the root topic was removed despite the rejection: %v", err)
	}
}

func TestDeleteExtractMissing(t *testing.T) {
	db := testStore(t)
	if err := db.DeleteExtract(999); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteExtract(999) = %v, want ErrNotFound", err)
	}
}

// TestReconcileMissingFlagsAbsentDocuments is the mechanism behind the
// "missing upstream" badge: a full listing that no longer includes a
// document's external_id is the only signal increader ever gets that
// something was deleted at the provider, since an incremental sync cannot
// distinguish "deleted" from "unchanged" — neither produces an event.
func TestReconcileMissingFlagsAbsentDocuments(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Still there", UpdatedAt: now},
		{ExternalID: "2", Title: "Deleted at wallabag", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	// A fresh full listing that only reports entry 1: entry 2 has vanished.
	marked, cleared, err := db.ReconcileMissing("wallabag", []string{"1"})
	if err != nil {
		t.Fatalf("ReconcileMissing: %v", err)
	}
	if marked != 1 || cleared != 0 {
		t.Errorf("marked=%d cleared=%d, want marked=1 cleared=0", marked, cleared)
	}

	present, _ := db.DocumentByID(1)
	if present.MissingUpstream {
		t.Error("a document that is still listed was flagged missing")
	}
	absent, _ := db.DocumentByID(2)
	if !absent.MissingUpstream {
		t.Error("a document absent from the listing was not flagged missing")
	}

	// It reappears in a later listing — restored, or the earlier check was a
	// transient fluke. Either way the flag must clear, not stay stuck.
	marked, cleared, err = db.ReconcileMissing("wallabag", []string{"1", "2"})
	if err != nil {
		t.Fatalf("ReconcileMissing (restored): %v", err)
	}
	if marked != 0 || cleared != 1 {
		t.Errorf("marked=%d cleared=%d, want marked=0 cleared=1", marked, cleared)
	}
	restored, _ := db.DocumentByID(2)
	if restored.MissingUpstream {
		t.Error("a document present again in the listing stayed flagged missing")
	}
}

// TestDeleteDocumentCascades checks a whole-document delete takes its
// extracts with it — the local-only cleanup for something ReconcileMissing
// has already flagged gone upstream.
func TestDeleteDocumentCascades(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	extractID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "A passage.",
		ContentHTML: "<p>A passage.</p>",
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	if err := db.DeleteDocument(1); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}

	if _, err := db.DocumentByID(1); !errors.Is(err, ErrNotFound) {
		t.Errorf("DocumentByID after delete: %v, want ErrNotFound", err)
	}
	if _, err := db.ElementByID(1); !errors.Is(err, ErrNotFound) {
		t.Errorf("root topic survived DeleteDocument: %v, want ErrNotFound", err)
	}
	if _, err := db.ElementByID(extractID); !errors.Is(err, ErrNotFound) {
		t.Errorf("extract survived DeleteDocument: %v, want ErrNotFound", err)
	}
}

func TestDeleteDocumentMissing(t *testing.T) {
	db := testStore(t)
	if err := db.DeleteDocument(999); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteDocument(999) = %v, want ErrNotFound", err)
	}
}

// TestReconcileMissingHighlightsFlagsDeletedAnnotations is the extract-level
// counterpart to TestReconcileMissingFlagsAbsentDocuments: increader is meant
// to hold a superset of wallabag's data, so a highlight deleted upstream must
// stay here, flagged rather than removed — covering both an imported
// highlight and a manual extract that was successfully pushed, since from
// wallabag's side those are indistinguishable once both carry a real
// annotation id.
func TestReconcileMissingHighlightsFlagsDeletedAnnotations(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "An article", UpdatedAt: now, Highlights: []source.Highlight{
			{ExternalID: "h1", Quote: "Stays annotated."},
			{ExternalID: "h2", Quote: "Deleted at wallabag."},
		}},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	// A manual extract already successfully pushed upstream — indistinguishable
	// from an import at this point, since it now carries a real external_ref.
	pushedID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Manual, but pushed, then deleted.",
		ContentHTML: "<p>Manual, but pushed, then deleted.</p>",
		Origin:      OriginManual, ExternalRef: "h3",
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}
	// CreateExtract only queues a push when ExternalRef is empty; setting it
	// directly above stands in for "the push already succeeded".

	// A plain manual extract with no upstream identity at all — must never be
	// touched by reconciliation just because "h-something" is not in the list.
	plainID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Never left increader.",
		ContentHTML: "<p>Never left increader.</p>", Origin: OriginManual,
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract (plain): %v", err)
	}

	// The fresh listing reports h1 and h3 still annotated; h2 has vanished.
	marked, cleared, err := db.ReconcileMissingHighlights("wallabag", []string{"h1", "h3"})
	if err != nil {
		t.Fatalf("ReconcileMissingHighlights: %v", err)
	}
	if marked != 1 || cleared != 0 {
		t.Errorf("marked=%d cleared=%d, want marked=1 cleared=0", marked, cleared)
	}

	imports, _ := db.Extracts(ExtractFilter{Origin: OriginImport})
	var stays, gone Element
	for _, extract := range imports {
		switch extract.ExternalRef {
		case "h1":
			stays = extract.Element
		case "h2":
			gone = extract.Element
		}
	}
	if stays.MissingUpstream {
		t.Error("an annotation still in the listing was flagged missing")
	}
	if !gone.MissingUpstream {
		t.Error("an annotation absent from the listing was not flagged missing")
	}

	pushed, _ := db.ElementByID(pushedID)
	if pushed.MissingUpstream {
		t.Error("a pushed extract still in the listing was flagged missing")
	}
	plain, _ := db.ElementByID(plainID)
	if plain.MissingUpstream {
		t.Error("a plain manual extract with no external_ref was incorrectly flagged")
	}

	// h2 reappears — restored, or the earlier check was a fluke either way
	// the flag must clear.
	marked, cleared, err = db.ReconcileMissingHighlights("wallabag", []string{"h1", "h2", "h3"})
	if err != nil {
		t.Fatalf("ReconcileMissingHighlights (restored): %v", err)
	}
	if marked != 0 || cleared != 1 {
		t.Errorf("marked=%d cleared=%d, want marked=0 cleared=1", marked, cleared)
	}
}

// TestQueueLocationUpdatesFindsExtractsNeedingOne covers the gap left by
// shipping ranges after highlights already existed upstream: every extract
// pushed before that point has a real annotation with nothing for wallabag's
// own reader to draw it at, and nothing about creating it originally would
// ever revisit that.
func TestQueueLocationUpdatesFindsExtractsNeedingOne(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	// Pushed before ranges existed: a real annotation, nothing to draw it at.
	locationlessID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Pushed before ranges existed.",
		ContentHTML: "<p>Pushed before ranges existed.</p>",
		Origin:      OriginManual, ExternalRef: "h1",
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract (locationless): %v", err)
	}

	// Already has one — must not be queued again just because it is manual.
	// Not a candidate at all, since "h2" is never passed to
	// QueueLocationUpdates below; already proven by len(writes) == 1.
	if _, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Already has a location.",
		ContentHTML: "<p>Already has a location.</p>",
		Origin:      OriginManual, ExternalRef: "h2",
	}, now); err != nil {
		t.Fatalf("CreateExtract (located): %v", err)
	}

	queued, err := db.QueueLocationUpdates("wallabag", []string{"h1"})
	if err != nil {
		t.Fatalf("QueueLocationUpdates: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued %d, want 1", queued)
	}

	writes, err := db.PendingWrites("wallabag", 10)
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if len(writes) != 1 {
		t.Fatalf("got %d pending writes, want 1", len(writes))
	}
	write := writes[0]
	if write.Operation != OpHighlightUpdateLocation {
		t.Errorf("operation = %q, want %q", write.Operation, OpHighlightUpdateLocation)
	}
	if write.ExternalID != "h1" {
		t.Errorf("external_id = %q, want the old annotation's own id, the same convention OpHighlightDelete uses", write.ExternalID)
	}
	if write.Payload != "Pushed before ranges existed." {
		t.Errorf("payload = %q, want the extract's quote", write.Payload)
	}
	if !write.ElementID.Valid || write.ElementID.Int64 != locationlessID {
		t.Errorf("ElementID = %+v, want it to point at the locationless extract", write.ElementID)
	}

	// Idempotent: a second call with the same input queues nothing further.
	queued, err = db.QueueLocationUpdates("wallabag", []string{"h1"})
	if err != nil {
		t.Fatalf("QueueLocationUpdates (second run): %v", err)
	}
	if queued != 0 {
		t.Errorf("second run queued %d, want 0", queued)
	}
}

// TestBackfillHighlightPushesQueuesMissedExtracts covers the gap CreateExtract
// alone cannot: an extract made before the push-back feature existed, or one
// whose write was somehow lost, has no external_ref and nothing queued for
// it, and CreateExtract itself only ever runs once, at creation. Backfill is
// what gives such an extract a second chance.
func TestBackfillHighlightPushesQueuesMissedExtracts(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	// Simulates an extract made before the feature existed: a plain INSERT
	// with no queued write, which is exactly what CreateExtract itself
	// produced prior to that change.
	missedID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Made before the feature existed.",
		ContentHTML: "<p>Made before the feature existed.</p>", Origin: OriginManual,
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract (missed): %v", err)
	}
	if _, err := db.db.Exec(`DELETE FROM pending_writes WHERE element_id = ?`, missedID); err != nil {
		t.Fatalf("clear the write CreateExtract queued, to simulate one predating the feature: %v", err)
	}

	// A normal extract made after the feature existed, still with its
	// original queued write intact — must not be queued a second time.
	freshID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Made after, already queued.",
		ContentHTML: "<p>Made after, already queued.</p>", Origin: OriginManual,
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract (fresh): %v", err)
	}

	// Already pushed — must not be re-queued despite having no pending write.
	pushedID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Already on wallabag.",
		ContentHTML: "<p>Already on wallabag.</p>",
		Origin:      OriginManual, ExternalRef: "h1",
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract (pushed): %v", err)
	}

	queued, err := db.BackfillHighlightPushes("wallabag")
	if err != nil {
		t.Fatalf("BackfillHighlightPushes: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued %d, want 1", queued)
	}

	writes, err := db.PendingWrites("wallabag", 10)
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if len(writes) != 2 {
		t.Fatalf("got %d pending writes, want 2 (the fresh extract's original plus the backfilled one)", len(writes))
	}

	var sawMissed, sawFresh, sawPushed bool
	for _, w := range writes {
		if !w.ElementID.Valid {
			t.Errorf("write %+v has no element_id", w)
			continue
		}
		switch w.ElementID.Int64 {
		case missedID:
			sawMissed = true
		case freshID:
			sawFresh = true
		case pushedID:
			sawPushed = true
		}
	}
	if !sawMissed {
		t.Error("the extract with no queued write was not backfilled")
	}
	if !sawFresh {
		t.Error("the extract's original write disappeared")
	}
	if sawPushed {
		t.Error("an extract already carrying an external_ref was queued again")
	}

	// Idempotent: running it again queues nothing further for the same
	// extract, since it now has a pending write of its own.
	queued, err = db.BackfillHighlightPushes("wallabag")
	if err != nil {
		t.Fatalf("BackfillHighlightPushes (second run): %v", err)
	}
	if queued != 0 {
		t.Errorf("second run queued %d, want 0", queued)
	}
}

// TestBackfillHighlightPushesResetsAbandonedWrites covers the other way an
// extract gets stuck: not never queued, but queued and then failed until it
// hit maxWriteAttempts — exactly what happened to real extracts here before
// the wallabag quote-length limit was discovered and fixed. PendingWrites
// filters those out of every future drain, so without a reset they would sit
// abandoned forever even though the bug that exhausted them no longer exists.
func TestBackfillHighlightPushesResetsAbandonedWrites(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", Title: "An article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	extractID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Exhausted its retries before the fix.",
		ContentHTML: "<p>Exhausted its retries before the fix.</p>", Origin: OriginManual,
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	writes, err := db.PendingWrites("wallabag", 10)
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if len(writes) != 1 {
		t.Fatalf("got %d pending writes, want 1", len(writes))
	}
	// Drive it past maxWriteAttempts, the same way a real repeated failure
	// would — FailWrite is the exact call drainWrites itself makes.
	for i := 0; i < maxWriteAttempts; i++ {
		if err := db.FailWrite(writes[0].ID, errors.New("simulated failure")); err != nil {
			t.Fatalf("FailWrite: %v", err)
		}
	}

	// Exhausted: the normal drain path no longer sees it.
	remaining, err := db.PendingWrites("wallabag", 10)
	if err != nil {
		t.Fatalf("PendingWrites (after exhausting): %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("got %d writes still visible to the drain path, want 0 (exhausted)", len(remaining))
	}

	queued, err := db.BackfillHighlightPushes("wallabag")
	if err != nil {
		t.Fatalf("BackfillHighlightPushes: %v", err)
	}
	if queued != 1 {
		t.Errorf("queued/reset %d, want 1", queued)
	}

	revived, err := db.PendingWrites("wallabag", 10)
	if err != nil {
		t.Fatalf("PendingWrites (after backfill): %v", err)
	}
	if len(revived) != 1 || !revived[0].ElementID.Valid || revived[0].ElementID.Int64 != extractID {
		t.Fatalf("PendingWrites after backfill = %+v, want the same extract's write, reset and visible again", revived)
	}
	if revived[0].Attempts != 0 {
		t.Errorf("attempts = %d, want reset to 0", revived[0].Attempts)
	}
}

// TestDocumentByExternalIDFindsMatch pins the (source, external_id) lookup
// against the same identity UpsertDocuments itself upserts on.
func TestDocumentByExternalIDFindsMatch(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "42", Title: "Found by provider id", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	document, err := db.DocumentByExternalID("wallabag", "42")
	if err != nil {
		t.Fatalf("DocumentByExternalID: %v", err)
	}
	if document.Title != "Found by provider id" {
		t.Errorf("title = %q, want the synced document", document.Title)
	}
}

// TestDocumentByExternalIDMissingIsNotFound covers the case ingest's repair
// pass relies on being ordinary rather than an error: a wallabag entry that
// was just created has no local row yet, because that row is only made by
// the next sync's UpsertDocuments, not by the create itself.
func TestDocumentByExternalIDMissingIsNotFound(t *testing.T) {
	db := testStore(t)

	_, err := db.DocumentByExternalID("wallabag", "no-such-id")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestClearDocumentContent covers the repair-pass call that forgets a
// document's cached body after its content has been replaced upstream, so
// the reading path re-fetches the real thing instead of serving the stale
// copy it already has — and that it touches nothing else about the document.
func TestClearDocumentContent(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "A preview", Author: "Some Author",
		ContentHTML: "<p>Only the preview.</p>", UpdatedAt: now,
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if err := db.ClearDocumentContent(1); err != nil {
		t.Fatalf("ClearDocumentContent: %v", err)
	}

	document, err := db.DocumentByID(1)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.ContentHTML != "" {
		t.Errorf("content_html = %q, want cleared", document.ContentHTML)
	}
	if document.HasContent {
		t.Error("has_content is still true after ClearDocumentContent")
	}
	if document.Title != "A preview" || document.Author != "Some Author" {
		t.Errorf("ClearDocumentContent touched fields beyond content: title=%q author=%q",
			document.Title, document.Author)
	}
}

// TestRemapExternalRefPreservesScheduling is the test the whole design turns
// on: re-anchoring a wallabag highlight always changes its annotation id
// (UpdateHighlightLocation is create-then-delete, since wallabag cannot
// relocate an annotation in place), and RemapExternalRef exists so that id
// change carries no cost to the reader — everything already decided about
// this passage, on this row, must survive untouched.
func TestRemapExternalRefPreservesScheduling(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "An article", UpdatedAt: now,
		Highlights: []source.Highlight{{ExternalID: "100", Quote: "A passage worth keeping."}},
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	extracts, err := db.ChildrenOf(1)
	if err != nil || len(extracts) != 1 {
		t.Fatalf("ChildrenOf: %v (extracts=%+v)", err, extracts)
	}
	extractID := extracts[0].ID

	// Give it a real reading history, the way actually reviewing it several
	// times would: this is what must not move.
	if err := db.SaveSchedule(extractID, ir.Schedule{
		State: ir.StateReading, IntervalDays: 21, AFactor: 2.6, Reps: 4,
		Priority: 0.35, DueOn: now.AddDate(0, 0, 21),
	}, now); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}

	if err := db.RemapExternalRef(1, "100", "200"); err != nil {
		t.Fatalf("RemapExternalRef: %v", err)
	}

	element, err := db.ElementByID(extractID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if element.ExternalRef != "200" {
		t.Errorf("external ref = %q, want the new one, 200", element.ExternalRef)
	}
	if element.Quote != "A passage worth keeping." {
		t.Errorf("quote = %q, want untouched", element.Quote)
	}
	if element.Schedule.IntervalDays != 21 || element.Schedule.AFactor != 2.6 ||
		element.Schedule.Reps != 4 || element.Schedule.Priority != 0.35 {
		t.Errorf("RemapExternalRef disturbed scheduling: %+v", element.Schedule)
	}
	if element.Schedule.DueOn.Format("2006-01-02") != now.AddDate(0, 0, 21).Format("2006-01-02") {
		t.Errorf("due_on = %v, want unchanged", element.Schedule.DueOn)
	}
}

// TestRemapExternalRefToleratesDuplicateCollision covers the retry case the
// partial unique index on (document_id, external_ref) creates: newRef is
// already claimed by some other row under the same document — most likely
// insertHighlights' own adopt-by-quote path having gotten there first on an
// intervening sync — so the UPDATE hits the unique constraint. That must be
// treated as "already done", not surfaced as a failure, and it must not
// disturb either row.
func TestRemapExternalRefToleratesDuplicateCollision(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "An article", UpdatedAt: now,
		Highlights: []source.Highlight{
			{ExternalID: "100", Quote: "The first passage."},
			{ExternalID: "999", Quote: "A completely different passage."},
		},
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	// "999" is already in use by the second highlight, so remapping "100"
	// onto it must collide rather than silently steal it.
	if err := db.RemapExternalRef(1, "100", "999"); err != nil {
		t.Fatalf("RemapExternalRef: want the collision swallowed, got error: %v", err)
	}

	extracts, err := db.ChildrenOf(1)
	if err != nil || len(extracts) != 2 {
		t.Fatalf("ChildrenOf: %v (extracts=%+v)", err, extracts)
	}
	refs := map[string]bool{}
	for _, e := range extracts {
		refs[e.ExternalRef] = true
	}
	if !refs["100"] || !refs["999"] {
		t.Errorf("refs after a collided remap = %v, want both rows unchanged (100 and 999)", refs)
	}
}

// TestRemapExternalRefNoMatchIsNotAnError covers the other idempotence case:
// oldRef matching nothing local, either because this exact remap already
// completed on an earlier, interrupted run, or because the row was never
// synced down in the first place.
func TestRemapExternalRefNoMatchIsNotAnError(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "An article", UpdatedAt: now,
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if err := db.RemapExternalRef(1, "no-such-old-ref", "new-ref"); err != nil {
		t.Errorf("RemapExternalRef with nothing to match: %v, want nil", err)
	}
}

// TestClearExtractAnchorsPreservesSchedulingAndPassage is ClearExtractAnchors'
// own load-bearing test: it must forget *only* the position an extract was
// last located at, leaving its passage, its rendered HTML and everything
// about its reading schedule exactly as they were — the position is what went
// stale when the parent's body changed underneath it, nothing else did.
func TestClearExtractAnchorsPreservesSchedulingAndPassage(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "A preview, soon to be replaced", UpdatedAt: now,
	}}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	extractID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Origin: OriginManual,
		Quote:       "A passage anchored against the old preview body.",
		ContentHTML: "<p>A passage anchored against the old preview body.</p>",
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	if err := db.AnchorExtract(extractID, ir.Range{StartBlock: 3, StartOffset: 10, EndBlock: 3, EndOffset: 40},
		"A passage anchored against the old preview body.",
		"<p>A passage anchored against the old preview body.</p>", now); err != nil {
		t.Fatalf("AnchorExtract: %v", err)
	}

	if err := db.SaveSchedule(extractID, ir.Schedule{
		State: ir.StateReading, IntervalDays: 12, AFactor: 2.2, Reps: 2,
		Priority: 0.4, DueOn: now.AddDate(0, 0, 12),
	}, now); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}

	cleared, err := db.ClearExtractAnchors(1)
	if err != nil {
		t.Fatalf("ClearExtractAnchors: %v", err)
	}
	if cleared != 1 {
		t.Errorf("cleared = %d, want 1", cleared)
	}

	element, err := db.ElementByID(extractID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if element.HasRange {
		t.Error("HasRange is still true after ClearExtractAnchors")
	}
	if element.Quote != "A passage anchored against the old preview body." {
		t.Errorf("quote = %q, want untouched", element.Quote)
	}
	if element.ContentHTML != "<p>A passage anchored against the old preview body.</p>" {
		t.Errorf("content_html = %q, want untouched — a failed re-location must degrade to the old text, not a blank", element.ContentHTML)
	}
	if element.Schedule.IntervalDays != 12 || element.Schedule.AFactor != 2.2 ||
		element.Schedule.Reps != 2 || element.Schedule.Priority != 0.4 {
		t.Errorf("ClearExtractAnchors disturbed scheduling: %+v", element.Schedule)
	}
	if element.Schedule.DueOn.Format("2006-01-02") != now.AddDate(0, 0, 12).Format("2006-01-02") {
		t.Errorf("due_on = %v, want unchanged", element.Schedule.DueOn)
	}

	// A second pass over the same document has nothing left to clear — the
	// root topic has no anchor to begin with, and the extract's is already
	// gone.
	clearedAgain, err := db.ClearExtractAnchors(1)
	if err != nil {
		t.Fatalf("second ClearExtractAnchors: %v", err)
	}
	if clearedAgain != 0 {
		t.Errorf("second pass cleared = %d, want 0", clearedAgain)
	}
}
