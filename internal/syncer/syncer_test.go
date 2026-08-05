package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/store"
)

// writingSource is a provider that records what was published to it and can be
// told to fail.
type writingSource struct {
	archived            map[string]bool
	tags                map[string][]string
	deletedHighlights   []string
	createdHighlights   []string // "entryID:quote", in call order
	relocatedHighlights []string // "oldID->newID:entryID:quote", in call order

	// nextHighlightID is handed out as the id of each created highlight,
	// standing in for wallabag assigning a real one.
	nextHighlightID int

	// listing is what Fetch returns, standing in for whatever a full or
	// incremental listing would currently report upstream.
	listing []source.Document

	failWith error
	calls    int

	// delay, if set, is slept through on each write — used to widen the
	// window in which two concurrent drains could otherwise both claim the
	// same pending row.
	delay time.Duration
}

func newWritingSource() *writingSource {
	return &writingSource{archived: map[string]bool{}, tags: map[string][]string{}}
}

func (w *writingSource) Name() string { return "wallabag" }

func (w *writingSource) Fetch(context.Context, time.Time) ([]source.Document, error) {
	return w.listing, w.failWith
}
func (w *writingSource) Content(context.Context, string) (string, error) { return "", nil }

func (w *writingSource) SetArchived(_ context.Context, id string, archived bool) error {
	if w.delay > 0 {
		time.Sleep(w.delay)
	}
	w.calls++
	if w.failWith != nil {
		return w.failWith
	}
	w.archived[id] = archived
	return nil
}

func (w *writingSource) SetStarred(context.Context, string, bool) error {
	w.calls++
	return w.failWith
}

func (w *writingSource) AddTags(_ context.Context, id string, labels []string) error {
	w.calls++
	if w.failWith != nil {
		return w.failWith
	}
	w.tags[id] = append(w.tags[id], labels...)
	return nil
}

func (w *writingSource) RemoveTag(context.Context, string, string) error {
	w.calls++
	return w.failWith
}

func (w *writingSource) DeleteHighlight(_ context.Context, id string) error {
	w.calls++
	if w.failWith != nil {
		return w.failWith
	}
	w.deletedHighlights = append(w.deletedHighlights, id)
	return nil
}

func (w *writingSource) CreateHighlight(_ context.Context, entryID, quote string) (string, error) {
	w.calls++
	if w.failWith != nil {
		return "", w.failWith
	}
	w.createdHighlights = append(w.createdHighlights, entryID+":"+quote)
	w.nextHighlightID++
	return strconv.Itoa(w.nextHighlightID), nil
}

func (w *writingSource) UpdateHighlightLocation(_ context.Context, oldID, entryID, quote string) (string, error) {
	w.calls++
	if w.failWith != nil {
		return "", w.failWith
	}
	w.nextHighlightID++
	newID := strconv.Itoa(w.nextHighlightID)
	w.relocatedHighlights = append(w.relocatedHighlights, oldID+"->"+newID+":"+entryID+":"+quote)
	return newID, nil
}

// readOnlySource cannot write, which must be handled rather than assumed away.
type readOnlySource struct{}

func (readOnlySource) Name() string { return "koreader" }
func (readOnlySource) Fetch(context.Context, time.Time) ([]source.Document, error) {
	return nil, nil
}
func (readOnlySource) Content(context.Context, string) (string, error) { return "", nil }

func testSetup(t *testing.T) (*store.Store, *slog.Logger) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, slog.New(slog.NewTextHandler(io.Discard, nil))
}

func seed(t *testing.T, db *store.Store) {
	t.Helper()
	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", Title: "An article", UpdatedAt: time.Now()},
	}, 0, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestDrainPublishesQueuedWrites(t *testing.T) {
	db, logger := testSetup(t)
	seed(t, db)

	if err := db.SetArchived(1, "wallabag", "77", true, time.Now()); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	if err := db.AttachTag(1, "wallabag", "77", "philosophy"); err != nil {
		t.Fatalf("AttachTag: %v", err)
	}

	provider := newWritingSource()
	published := New(db, logger, provider).drainWrites(context.Background(), provider)

	if published != 2 {
		t.Errorf("published %d writes, want 2", published)
	}
	if !provider.archived["77"] {
		t.Error("the archive write did not reach the provider")
	}
	if len(provider.tags["77"]) != 1 || provider.tags["77"][0] != "philosophy" {
		t.Errorf("tags reaching the provider = %v", provider.tags["77"])
	}

	// Published writes are cleared, so a second drain sends nothing.
	remaining, _ := db.PendingWrites("wallabag", 10)
	if len(remaining) != 0 {
		t.Errorf("%d writes still queued after publishing", len(remaining))
	}
}

// TestDrainPushesNewHighlightsAndRecordsTheirID is the round trip a manual
// extract needs: draining sends it upstream as a new annotation, and the id
// wallabag hands back for it is written onto the local element, not
// discarded — without that, deleting the extract later would have nothing to
// remove upstream.
func TestDrainPushesNewHighlightsAndRecordsTheirID(t *testing.T) {
	db, logger := testSetup(t)
	seed(t, db)

	extractID, err := db.CreateExtract(store.NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "A passage worth keeping.",
		ContentHTML: "<p>A passage worth keeping.</p>", Origin: store.OriginManual,
	}, time.Now())
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	provider := newWritingSource()
	published := New(db, logger, provider).drainWrites(context.Background(), provider)

	if published != 1 {
		t.Fatalf("published %d writes, want 1", published)
	}
	if len(provider.createdHighlights) != 1 || provider.createdHighlights[0] != "77:A passage worth keeping." {
		t.Errorf("created highlights = %v, want one for entry 77", provider.createdHighlights)
	}

	element, err := db.ElementByID(extractID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if element.ExternalRef != "1" {
		t.Errorf("ExternalRef = %q, want the id wallabag handed back for the new annotation", element.ExternalRef)
	}

	// Published writes are cleared, same as any other operation.
	remaining, _ := db.PendingWrites("wallabag", 10)
	if len(remaining) != 0 {
		t.Errorf("%d writes still queued after publishing", len(remaining))
	}
}

// TestReconcileFlagsDeletedEntries is the end-to-end path behind the
// library's "missing upstream" badge: Reconcile does a full listing, and
// anything previously known locally but absent from it gets flagged.
func TestReconcileFlagsDeletedEntries(t *testing.T) {
	db, logger := testSetup(t)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", Title: "Stays", UpdatedAt: time.Now()},
		{ExternalID: "78", Title: "Gets deleted at wallabag", UpdatedAt: time.Now()},
	}, 0, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	provider := newWritingSource()
	// The full listing Reconcile fetches now only reports entry 77 — 78 has
	// vanished from wallabag's own side since the seed above.
	provider.listing = []source.Document{
		{ExternalID: "77", Title: "Stays", UpdatedAt: time.Now()},
	}

	if err := New(db, logger, provider).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	stays, _ := db.DocumentByID(1)
	if stays.MissingUpstream {
		t.Error("a document still in the listing was flagged missing")
	}
	gone, _ := db.DocumentByID(2)
	if !gone.MissingUpstream {
		t.Error("a document absent from the listing was not flagged missing")
	}
}

// TestReconcileFlagsDeletedHighlights is TestReconcileFlagsDeletedEntries one
// level down: an annotation deleted upstream, on a document that is still
// there, must be flagged rather than silently forgotten — increader keeps a
// superset of wallabag's data, so the extract itself must survive.
func TestReconcileFlagsDeletedHighlights(t *testing.T) {
	db, logger := testSetup(t)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", Title: "An article", UpdatedAt: time.Now(), Highlights: []source.Highlight{
			{ExternalID: "h1", Quote: "Stays annotated."},
			{ExternalID: "h2", Quote: "Deleted at wallabag."},
		}},
	}, 0, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	provider := newWritingSource()
	// The article itself is unchanged, but its annotations list has shrunk
	// to just h1 — h2 was deleted at wallabag.
	provider.listing = []source.Document{
		{ExternalID: "77", Title: "An article", UpdatedAt: time.Now(), Highlights: []source.Highlight{
			{ExternalID: "h1", Quote: "Stays annotated."},
		}},
	}

	if err := New(db, logger, provider).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	extracts, err := db.Extracts(store.ExtractFilter{Origin: store.OriginImport})
	if err != nil {
		t.Fatalf("Extracts: %v", err)
	}
	if len(extracts) != 2 {
		t.Fatalf("got %d imported extracts, want 2 — a deletion upstream must not remove increader's own copy", len(extracts))
	}
	for _, extract := range extracts {
		want := extract.ExternalRef == "h2"
		if extract.MissingUpstream != want {
			t.Errorf("extract %s: MissingUpstream = %v, want %v", extract.ExternalRef, extract.MissingUpstream, want)
		}
	}
}

// TestReconcileRelocatesAndDrainsLocationlessHighlights covers the gap
// shipping ranges after highlights already existed left behind: Reconcile
// must notice a highlight the provider reports with no location
// (Highlight.HasLocation false), queue a replacement, and push it out
// immediately — same as backfilling a missed push, so whoever triggers a
// manual sync actually sees the result instead of just queuing it.
func TestReconcileRelocatesAndDrainsLocationlessHighlights(t *testing.T) {
	db, logger := testSetup(t)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "77", Title: "An article", UpdatedAt: time.Now(), Highlights: []source.Highlight{
			{ExternalID: "h1", Quote: "Pushed before ranges existed.", HasLocation: false},
		}},
	}, 0, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	provider := newWritingSource()
	provider.listing = []source.Document{
		{ExternalID: "77", Title: "An article", UpdatedAt: time.Now(), Highlights: []source.Highlight{
			{ExternalID: "h1", Quote: "Pushed before ranges existed.", HasLocation: false},
		}},
	}

	if err := New(db, logger, provider).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(provider.relocatedHighlights) != 1 {
		t.Fatalf("relocated highlights = %v, want exactly one pushed immediately", provider.relocatedHighlights)
	}
	if provider.relocatedHighlights[0] != "h1->1:77:Pushed before ranges existed." {
		t.Errorf("relocated = %q, want the old id replaced against entry 77 with the same quote", provider.relocatedHighlights[0])
	}

	extracts, err := db.Extracts(store.ExtractFilter{Origin: store.OriginImport})
	if err != nil {
		t.Fatalf("Extracts: %v", err)
	}
	if len(extracts) != 1 {
		t.Fatalf("got %d imported extracts, want 1", len(extracts))
	}
	if extracts[0].ExternalRef != "1" {
		t.Errorf("ExternalRef = %q, want the new id from the replacement", extracts[0].ExternalRef)
	}
}

// TestReconcileBackfillsAndDrainsMissedPushes is the fix for extracts that
// were made before push-back existed, or whose write was somehow lost: they
// have no external_ref and nothing queued, so nothing would ever retry them
// on its own. Reconcile must not just queue the backfilled write but also
// push it out immediately — the whole point of running this from a manual
// sync is seeing the result, not queueing something for later.
func TestReconcileBackfillsAndDrainsMissedPushes(t *testing.T) {
	db, logger := testSetup(t)
	seed(t, db)

	missedID, err := db.CreateExtract(store.NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "Missed its original push.",
		ContentHTML: "<p>Missed its original push.</p>", Origin: store.OriginManual,
	}, time.Now())
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}
	// Simulate an extract that predates the feature, or a lost write: no
	// queued push for it despite having no external_ref. CompleteWrite is
	// the same call drainWrites itself makes once a write has gone out —
	// here it stands in for "this write is gone", regardless of why.
	writes, err := db.PendingWrites("wallabag", 10)
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	for _, w := range writes {
		if w.ElementID.Valid && w.ElementID.Int64 == missedID {
			if err := db.CompleteWrite(w.ID); err != nil {
				t.Fatalf("CompleteWrite: %v", err)
			}
		}
	}

	provider := newWritingSource()
	provider.listing = []source.Document{{ExternalID: "77", Title: "An article", UpdatedAt: time.Now()}}

	if err := New(db, logger, provider).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(provider.createdHighlights) != 1 {
		t.Fatalf("created highlights = %v, want exactly the backfilled one pushed immediately", provider.createdHighlights)
	}

	element, err := db.ElementByID(missedID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if element.ExternalRef == "" {
		t.Error("the backfilled extract still has no external_ref — it was queued but not drained")
	}
}

// TestFailedWritesSurviveForRetry is the reason the outbox exists: a wallabag
// outage is exactly when a lost write would go unnoticed, because nothing looks
// wrong locally.
func TestFailedWritesSurviveForRetry(t *testing.T) {
	db, logger := testSetup(t)
	seed(t, db)

	if err := db.SetArchived(1, "wallabag", "77", true, time.Now()); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}

	provider := newWritingSource()
	provider.failWith = errors.New("connection refused")

	syncer := New(db, logger, provider)
	if published := syncer.drainWrites(context.Background(), provider); published != 0 {
		t.Errorf("published %d writes despite failure", published)
	}

	writes, _ := db.PendingWrites("wallabag", 10)
	if len(writes) != 1 {
		t.Fatalf("the failed write was lost")
	}
	if writes[0].Attempts != 1 {
		t.Errorf("attempts = %d, want 1", writes[0].Attempts)
	}
	if writes[0].LastError == "" {
		t.Error("the failure was not recorded")
	}

	// It goes through once the provider recovers.
	provider.failWith = nil
	if published := syncer.drainWrites(context.Background(), provider); published != 1 {
		t.Errorf("published %d on retry, want 1", published)
	}
	if !provider.archived["77"] {
		t.Error("the retried write did not reach the provider")
	}
}

// TestWritesForDeletedEntriesAreDropped keeps the queue from filling with work
// that can never succeed.
func TestWritesForDeletedEntriesAreDropped(t *testing.T) {
	db, logger := testSetup(t)
	seed(t, db)

	if err := db.SetArchived(1, "wallabag", "77", true, time.Now()); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}

	provider := newWritingSource()
	provider.failWith = source.ErrGone

	New(db, logger, provider).drainWrites(context.Background(), provider)

	writes, _ := db.PendingWrites("wallabag", 10)
	queued, abandoned, _ := db.CountPendingWrites("wallabag")
	if len(writes) != 0 || queued != 0 || abandoned != 0 {
		t.Errorf("a write for a deleted entry was kept: %d queued, %d abandoned",
			queued, abandoned)
	}
}

// TestReadOnlySourcesAreSkipped covers the optional-interface seam: a provider
// that cannot write must not be asked to.
func TestReadOnlySourcesAreSkipped(t *testing.T) {
	db, logger := testSetup(t)

	provider := readOnlySource{}
	if published := New(db, logger, provider).drainWrites(context.Background(), provider); published != 0 {
		t.Errorf("published %d writes to a read-only source", published)
	}
}

// TestConcurrentDrainsDoNotDoublePublish covers the outbox drain race: a
// scheduled tick and a manual "sync now" request run drainWrites from
// different goroutines, and without serialization both can read the same
// pending row before either clears it, publishing it upstream twice.
func TestConcurrentDrainsDoNotDoublePublish(t *testing.T) {
	db, logger := testSetup(t)
	seed(t, db)

	if err := db.SetArchived(1, "wallabag", "77", true, time.Now()); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}

	provider := newWritingSource()
	provider.delay = 20 * time.Millisecond
	syncer := New(db, logger, provider)

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			syncer.drainWrites(context.Background(), provider)
		}()
	}
	wg.Wait()

	if provider.calls != 1 {
		t.Errorf("provider was called %d times for one queued write, want exactly 1", provider.calls)
	}

	remaining, _ := db.PendingWrites("wallabag", 10)
	if len(remaining) != 0 {
		t.Errorf("%d writes still queued after two concurrent drains", len(remaining))
	}
}

func TestPublishNudgeDoesNotBlock(t *testing.T) {
	db, logger := testSetup(t)
	syncer := New(db, logger, newWritingSource())

	// Called more often than the buffer holds; extra signals are dropped
	// rather than blocking a request handler.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			syncer.Publish()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked; a request handler would have stalled on it")
	}
}
