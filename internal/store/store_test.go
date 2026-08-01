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
	}}, 0, now)
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

	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	document.Title = "Retitled upstream"
	result, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now)
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
	}}, 0, now); err != nil {
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
	}}, 0, now); err != nil {
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
	}, 0, time.Now())
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
	}, 0, time.Now()); err != nil {
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
	}, 0, now); err != nil {
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
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	document.IsArchived = true
	document.UpdatedAt = now.Add(time.Hour)
	result, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now)
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
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	if err := db.Unsuspend(1, now, now); err != nil {
		t.Fatalf("Unsuspend: %v", err)
	}

	// Sync again — still archived upstream, but not a fresh transition.
	document.UpdatedAt = now.Add(time.Hour)
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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

	result, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now)
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
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now); err != nil {
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
	}}, 0, now); err != nil {
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
	if _, err := db.UpsertDocuments("wallabag", documents, 0, now); err != nil {
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

// TestQueueInterleavesProportionallyByCount guards the fair-interleave itself,
// not just the tie-break: a handful of due articles (priority 0.5) against a
// pile of due imported highlights (priority 0.6, importedPriority) must not
// sort into two blocks just because their priorities differ. They should
// spread through the queue in proportion to how many of each are due — three
// articles among twenty-seven highlights land roughly every tenth slot, not
// all three up front.
func TestQueueInterleavesProportionallyByCount(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	highlights := make([]source.Highlight, 0, 27)
	for i := 1; i <= 27; i++ {
		highlights = append(highlights, source.Highlight{
			ExternalID: "h" + strconv.Itoa(i),
			Quote:      "Highlight " + strconv.Itoa(i),
			UpdatedAt:  now,
		})
	}

	documents := []source.Document{
		{ExternalID: "1", Title: "Article 1", UpdatedAt: now},
		{ExternalID: "2", Title: "Article 2", UpdatedAt: now},
		{ExternalID: "3", Title: "Article 3", UpdatedAt: now},
		// Archived so its own root topic is suspended and does not itself
		// enter the queue — only its highlights should, keeping the count at
		// exactly 3 due articles against 27 due highlights.
		{ExternalID: "4", Title: "Annotated article", UpdatedAt: now, Highlights: highlights, IsArchived: true},
	}
	// delayDays 0 puts every highlight due today, alongside the articles —
	// this test is about same-day ordering, not the multi-day spread that
	// ExtractDelayDays already covers.
	if _, err := db.UpsertDocuments("wallabag", documents, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	queue, err := db.Queue(now, 30)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(queue) != 30 {
		t.Fatalf("got %d queued, want 30", len(queue))
	}

	var articleIndexes []int
	for i, item := range queue {
		if item.IsRoot() {
			articleIndexes = append(articleIndexes, i)
		}
	}
	if len(articleIndexes) != 3 {
		t.Fatalf("got %d articles in queue, want 3", len(articleIndexes))
	}

	// Fair interleave by rank puts the k-th of 3 articles at index
	// floor((k-0.5)/3 * 30) among 30 due items: 4, 14, 24.
	want := []int{4, 14, 24}
	for i, index := range articleIndexes {
		if index != want[i] {
			t.Errorf("article %d landed at index %d, want %d (articles: %v); "+
				"not interleaved in proportion to how many of each are due",
				i, index, want[i], articleIndexes)
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
	}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now); err != nil {
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
	result, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now)
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
	}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now); err != nil {
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
	if _, err := db.UpsertDocuments("wallabag", []source.Document{document}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
		got, err := db.SearchDocuments(test.filter)
		if err != nil {
			t.Fatalf("SearchDocuments(%+v): %v", test.filter, err)
		}
		if len(got) != test.want {
			t.Errorf("filter %+v returned %d, want %d", test.filter, len(got), test.want)
		}
	}

	counts, err := db.CountByState("wallabag")
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
	if _, err := db.UpsertDocuments("wallabag", documents, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	first, _ := db.Queue(now, 10)
	buried := first[0].ID

	if err := db.Bury(buried, now); err != nil {
		t.Fatalf("Bury: %v", err)
	}

	after, _ := db.Queue(now, 10)
	if len(after) != 4 {
		t.Fatalf("burying removed the element from today: %d remain", len(after))
	}
	if after[len(after)-1].ID != buried {
		t.Errorf("buried element is at position %d, want last",
			indexOfElement(after, buried))
	}

	// Tomorrow it is ordinary again — the date stops matching, so nothing has
	// to be cleared and nothing can stay buried by accident.
	tomorrow, _ := db.Queue(now.AddDate(0, 0, 1), 10)
	if tomorrow[len(tomorrow)-1].ID == buried && len(tomorrow) > 1 {
		if first[0].ID == buried {
			t.Error("the element is still sorted last a day after being buried")
		}
	}
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
	}, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	id, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "A passage.", DelayDays: 10,
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	extract, _ := db.ElementByID(id)
	want := ir.Day(now.AddDate(0, 0, 10))
	if !ir.Day(extract.Schedule.DueOn).Equal(want) {
		t.Errorf("due %v, want %v", extract.Schedule.DueOn, want)
	}

	// It is therefore not in today's queue — only the article is.
	queue, _ := db.Queue(now, 10)
	if len(queue) != 1 {
		t.Errorf("queue holds %d elements, want only the article", len(queue))
	}
}

// TestImportedHighlightsSpreadAcrossTheWindow: a library's import is hundreds
// of highlights at once, and putting them all on one date moves the pile
// rather than clearing it.
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
	if _, err := db.UpsertDocuments("wallabag", documents, 10, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	rows, err := db.db.Query(`
		SELECT due_on, COUNT(*) FROM elements
		WHERE origin = 'import' GROUP BY due_on ORDER BY due_on`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	days, largest := 0, 0
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

	if days < 5 {
		t.Errorf("40 highlights landed on %d distinct days; they are not spread", days)
	}
	if largest > 15 {
		t.Errorf("the busiest day holds %d of 40 highlights, want them spread", largest)
	}

	// And none of them are due today, which is the point of the delay.
	due, _ := db.CountDue(now)
	if due != 40 {
		t.Errorf("%d due today, want just the 40 articles", due)
	}
}

// TestDeleteExtractCascades: an extract's own children, clozes and export
// ledger rows must all go with it — none of it should orphan on delete.
func TestDeleteExtractCascades(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "An article", UpdatedAt: now},
	}, 0, now); err != nil {
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

// TestDeleteExtractQueuesHighlightRemoval is the pairing that keeps a deletion
// from silently undoing itself: without the queued upstream removal, the next
// sync would re-import the very highlight just deleted.
func TestDeleteExtractQueuesHighlightRemoval(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{{
		ExternalID: "1", Title: "An article", UpdatedAt: now,
		Highlights: []source.Highlight{{ExternalID: "97418", Quote: "A passage."}},
	}}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
	}, 0, now); err != nil {
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
