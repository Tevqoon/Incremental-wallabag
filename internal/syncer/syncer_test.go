package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/store"
)

// writingSource is a provider that records what was published to it and can be
// told to fail.
type writingSource struct {
	archived map[string]bool
	tags     map[string][]string
	failWith error
	calls    int
}

func newWritingSource() *writingSource {
	return &writingSource{archived: map[string]bool{}, tags: map[string][]string{}}
}

func (w *writingSource) Name() string { return "wallabag" }

func (w *writingSource) Fetch(context.Context, time.Time) ([]source.Document, error) {
	return nil, nil
}
func (w *writingSource) Content(context.Context, string) (string, error) { return "", nil }

func (w *writingSource) SetArchived(_ context.Context, id string, archived bool) error {
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
	}, time.Now()); err != nil {
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
