package store

import (
	"path/filepath"
	"testing"
	"time"

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
	if version != 1 {
		t.Errorf("user_version = %d, want 1", version)
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
