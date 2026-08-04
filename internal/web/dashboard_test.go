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
	if !strings.Contains(body, "Nothing is due today.") {
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

// TestDashboardNavLinksPresent guards the split between the dashboard (home)
// and the queue (its own page now) — both must be reachable from the nav.
func TestDashboardNavLinksPresent(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	body := get(t, server, "/").Body.String()
	if !strings.Contains(body, `href="/queue"`) {
		t.Errorf("nav is missing the Queue link:\n%s", body)
	}
}
