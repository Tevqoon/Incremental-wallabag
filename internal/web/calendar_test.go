package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestCalendarShowsCurrentMonth covers the default view — no ?month means
// today's month, with today's own cell present and marked.
func TestCalendarShowsCurrentMonth(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	response := get(t, server, "/calendar")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	body := response.Body.String()
	if !strings.Contains(body, time.Now().Format("January 2006")) {
		t.Errorf("calendar does not show the current month:\n%s", body)
	}
	if !strings.Contains(body, `is-today`) {
		t.Errorf("calendar does not mark today's cell:\n%s", body)
	}
}

// TestCalendarMonthNavigation checks the prev/next links carry the adjacent
// months, not just relabel the same one.
func TestCalendarMonthNavigation(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	body := get(t, server, "/calendar?month=2026-03").Body.String()
	if !strings.Contains(body, "March 2026") {
		t.Errorf("calendar does not honour the requested month:\n%s", body)
	}
	if !strings.Contains(body, "/calendar?month=2026-02") {
		t.Errorf("calendar is missing the link back to February:\n%s", body)
	}
	if !strings.Contains(body, "/calendar?month=2026-04") {
		t.Errorf("calendar is missing the link forward to April:\n%s", body)
	}
}

// TestCalendarDayShowsGradedActivity exercises the real write path — a grade
// posted through /elements/{id}/grade — and confirms the day it landed on
// shows it, the same way TestDashboardShowsStreakAfterGrading covers the
// dashboard's own reading of the same data.
func TestCalendarDayShowsGradedActivity(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	if response := post(t, server, "/elements/1/grade", url.Values{"grade": {"done"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("grade: status = %d, want 303", response.Code)
	}

	today := time.Now().Format("2006-01-02")
	body := get(t, server, "/calendar/day/"+today).Body.String()
	if !strings.Contains(body, "A test article") {
		t.Errorf("day view does not list the graded article:\n%s", body)
	}
	if !strings.Contains(body, `class="badge">done<`) {
		t.Errorf("day view does not show the grade it landed on:\n%s", body)
	}
}

// TestCalendarDayShowsWordCount covers the day view's word/article summary,
// added alongside the dashboard's own weekly word count.
func TestCalendarDayShowsWordCount(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	post(t, server, "/elements/1/extract", url.Values{
		"start_block": {"0"}, "start_offset": {"4"},
		"end_block": {"0"}, "end_offset": {"15"}, "quote": {"quick brown"},
	})

	today := time.Now().Format("2006-01-02")
	body := get(t, server, "/calendar/day/"+today).Body.String()
	if !strings.Contains(body, "2 words extracted") {
		t.Errorf("day view does not show the extract's word count:\n%s", body)
	}
}

// TestCalendarDayEmptyState covers a day with nothing logged.
func TestCalendarDayEmptyState(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	body := get(t, server, "/calendar/day/2020-01-01").Body.String()
	if !strings.Contains(body, "Nothing logged this day.") {
		t.Errorf("day view does not show its empty state:\n%s", body)
	}
}

// TestCalendarDayRejectsABadDate guards the path-value parse, same as the
// element-id handlers do for a non-numeric {id}.
func TestCalendarDayRejectsABadDate(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	response := get(t, server, "/calendar/day/not-a-date")
	if response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.Code)
	}
}

// TestDashboardHeatmapLinksIntoTheCalendar guards the "click a day" path
// from the dashboard's own heatmap, and the link back to the full calendar.
func TestDashboardHeatmapLinksIntoTheCalendar(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	body := get(t, server, "/").Body.String()
	if !strings.Contains(body, `href="/calendar"`) {
		t.Errorf("dashboard is missing the full-calendar link:\n%s", body)
	}
	today := time.Now().Format("2006-01-02")
	if !strings.Contains(body, "/calendar/day/"+today) {
		t.Errorf("dashboard heatmap does not link into today's day view:\n%s", body)
	}
}
