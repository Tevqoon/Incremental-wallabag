package store

import (
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
	// Bumped by every migration file; the point of the assertion is that
	// re-opening does not re-apply them, not the specific number.
	if version != 2 {
		t.Errorf("user_version = %d, want 2", version)
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
	}}, now)
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
	due, err := db.CountDue(now)
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

	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, now); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	document.Title = "Retitled upstream"
	result, err := db.UpsertDocuments("wallabag", []source.Document{document}, now)
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
	}}, now); err != nil {
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
	}}, now); err != nil {
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
	}, time.Now())
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
	}, time.Now()); err != nil {
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
	}, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	queue, err := db.Queue(now, 10)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(queue) != 1 {
		t.Fatalf("queue holds %d elements, want only the unread article", len(queue))
	}
	if queue[0].Title != "Unread" {
		t.Errorf("queued %q, want the unread article", queue[0].Title)
	}

	due, err := db.CountDue(now)
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
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, now); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	document.IsArchived = true
	document.UpdatedAt = now.Add(time.Hour)
	result, err := db.UpsertDocuments("wallabag", []source.Document{document}, now)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if result.Suspended != 1 {
		t.Errorf("reported %d suspended, want 1", result.Suspended)
	}

	queue, _ := db.Queue(now, 10)
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
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, now); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	if err := db.Unsuspend(1, now, now); err != nil {
		t.Fatalf("Unsuspend: %v", err)
	}

	// Sync again — still archived upstream, but not a fresh transition.
	document.UpdatedAt = now.Add(time.Hour)
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, now); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	queue, _ := db.Queue(now, 10)
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
	}, now); err != nil {
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

	result, err := db.UpsertDocuments("wallabag", []source.Document{document}, now)
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
	// the entire point of importing them.
	queue, _ := db.Queue(now, 10)
	if len(queue) != 2 {
		t.Errorf("queue holds %d elements, want the two extracts", len(queue))
	}

	// Re-syncing must not duplicate them.
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, now); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	extracts, _ = db.ChildrenOf(1)
	if len(extracts) != 2 {
		t.Errorf("got %d extracts after re-sync, want 2", len(extracts))
	}
}

func TestExtractsBrowse(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "Source article", UpdatedAt: now,
		Highlights: []source.Highlight{{ExternalID: "1", Quote: "An imported passage."}},
	}}, now); err != nil {
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

// TestQueueInterleavesArticlesAndExtracts guards the tie-break. Everything
// starts at the same default priority, so ordering by id alone would put every
// article ahead of every extract and you would never reach an extract until the
// entire reading list was done.
func TestQueueInterleavesArticlesAndExtracts(t *testing.T) {
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
	if _, err := db.UpsertDocuments("wallabag", documents, now); err != nil {
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

	queue, err := db.Queue(now, 10)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(queue) != 10 {
		t.Fatalf("got %d queued, want 10", len(queue))
	}

	var articles, extracts int
	for _, item := range queue {
		if item.IsRoot() {
			articles++
		} else {
			extracts++
		}
	}
	if articles == 0 || extracts == 0 {
		t.Errorf("first 10 of the queue are all one kind (%d articles, %d extracts); "+
			"articles and extracts are not interleaved", articles, extracts)
	}

	// Deterministic: the same query must not reshuffle between page loads.
	again, _ := db.Queue(now, 10)
	for i := range queue {
		if queue[i].ID != again[i].ID {
			t.Fatalf("queue order changed between reads at position %d", i)
		}
	}
}
