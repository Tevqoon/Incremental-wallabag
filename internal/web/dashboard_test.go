package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tevqoon/increader/internal/store"
)

// TestDashboardEmptyState covers a fresh install: no documents synced yet,
// so every section should render its empty/zero state rather than error.
func TestDashboardEmptyState(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	server, err := New(Options{
		Store:  db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	response := get(t, server, "/")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	body := response.Body.String()
	if !strings.Contains(body, "No articles are due today.") {
		t.Errorf("dashboard does not show the empty queue-preview state:\n%s", body)
	}
	if !strings.Contains(body, `class="tile-value">0<`) {
		t.Errorf("dashboard does not show a zero streak tile:\n%s", body)
	}
}

// TestDashboardShowsBacklogComposition checks that the backlog breakdown
// reflects CountByState via a real request, not just that the query works in
// isolation.
func TestDashboardShowsBacklogComposition(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	body := get(t, server, "/").Body.String()
	if !strings.Contains(body, "/library?state=unread") {
		t.Errorf("dashboard is missing the unread backlog row:\n%s", body)
	}
	if !strings.Contains(body, `class="breakdown-count">1<`) {
		t.Errorf("dashboard does not show the seeded article in the backlog breakdown:\n%s", body)
	}
}

// TestDashboardBacklogCountsUploadedBooks guards against the dashboard
// naming "wallabag" explicitly when counting backlog state — CountByState
// itself supports every source via an empty sourceName (see its own doc
// comment), and a book imported by upload must be as visible here as an
// article synced from wallabag, not silently dropped from the breakdown.
func TestDashboardBacklogCountsUploadedBooks(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	importedDocumentID(t, server, "triage")

	body := get(t, server, "/").Body.String()
	// The seeded wallabag article plus the freshly imported book: both
	// default to unread (neither is archived), so this must read 2 — 1
	// would mean the book was left out.
	if !strings.Contains(body, `class="breakdown-count">2<`) {
		t.Errorf("dashboard backlog does not count the uploaded book alongside the wallabag article:\n%s", body)
	}
}

// TestDashboardShowsStreakAfterGrading exercises the real write path — a
// grade posted through /elements/{id}/grade, the same route the reader's
// buttons use — and confirms the dashboard reflects it, so the
// applyGrade -> SaveScheduleReviewed -> activity_log wiring is covered
// end-to-end rather than only at the store layer.
func TestDashboardShowsStreakAfterGrading(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	if response := post(t, server, "/elements/1/grade", url.Values{"grade": {"done"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("grade: status = %d, want 303", response.Code)
	}

	body := get(t, server, "/").Body.String()
	if !strings.Contains(body, `class="tile-value">1<`) {
		t.Errorf("dashboard does not show a 1-day streak after grading:\n%s", body)
	}
}

// TestDashboardShowsWeeklyArticlesAndWords exercises the write path for the
// week's headline tiles: finishing an article shows up as read today and
// read this week, and the extract taken alongside it shows up in the
// extracts drawer's own word count.
func TestDashboardShowsWeeklyArticlesAndWords(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"4"},
		"end_block": {"0"}, "end_offset": {"15"}, "quote": {"quick brown"},
	})
	if response := post(t, server, "/elements/1/grade", url.Values{"grade": {"done"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("grade: status = %d, want 303", response.Code)
	}

	body := get(t, server, "/").Body.String()
	if !strings.Contains(body, "read today") {
		t.Errorf("dashboard is missing the read-today tile:\n%s", body)
	}
	if !strings.Contains(body, "read this week") {
		t.Errorf("dashboard is missing the read-this-week tile:\n%s", body)
	}
	if !strings.Contains(body, `class="tile-value">1<`) {
		t.Errorf("dashboard does not show 1 article read:\n%s", body)
	}
	if !strings.Contains(body, "2 words") {
		t.Errorf("dashboard does not show 2 words extracted (\"quick brown\"):\n%s", body)
	}
}

// TestDashboardNavLinksPresent guards the split between the dashboard (home)
// and the queues (their own page now) — all must be reachable from the nav,
// the extract queue included: it is the one a reader never lands in by
// accident, so losing its link would lose the queue.
func TestDashboardNavLinksPresent(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	body := get(t, server, "/").Body.String()
	for _, link := range []string{"/queue?kind=articles", "/queue?kind=extracts"} {
		if !strings.Contains(body, `href="`+link+`"`) {
			t.Errorf("nav is missing %s:\n%s", link, body)
		}
	}
}
