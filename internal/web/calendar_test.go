package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/store"
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

// TestCalendarShowsArticlesReadInTheGrid is the core promise of the redesign:
// the number in today's cell is articles read, not raw activity-log rows.
func TestCalendarShowsArticlesReadInTheGrid(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	if response := post(t, server, "/elements/1/grade", url.Values{"grade": {"next"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("grade: status = %d, want 303", response.Code)
	}

	body := get(t, server, "/calendar").Body.String()
	today := time.Now().Format("2006-01-02")
	if !strings.Contains(body, `href="/calendar/day/`+today+`"`) {
		t.Fatalf("calendar is missing today's cell:\n%s", body)
	}
	if !strings.Contains(body, `1 article`) {
		t.Errorf("today's cell does not report 1 article read:\n%s", body)
	}
	if !strings.Contains(body, `class="tile-value">1<`) {
		t.Errorf("month summary does not show 1 article read:\n%s", body)
	}
}

// TestCalendarMonthStripLinksAdjacentMonths covers the 12-month strip: it
// carries real links into the months it summarises, including the one
// currently on screen.
func TestCalendarMonthStripLinksAdjacentMonths(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	body := get(t, server, "/calendar?month=2026-03").Body.String()
	if !strings.Contains(body, `href="/calendar?month=2025-04"`) {
		t.Errorf("month strip is missing a link a year back:\n%s", body)
	}
	if !strings.Contains(body, `href="/calendar?month=2026-03"`) {
		t.Errorf("month strip is missing a link to the month on screen:\n%s", body)
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
	if !strings.Contains(body, `class="badge badge-done">done<`) {
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
	if !strings.Contains(body, "2 words") {
		t.Errorf("day view does not show the extract's word count:\n%s", body)
	}
}

// TestCalendarDayGroupsMultipleGradesIntoOneRow covers the fold that makes
// the day view about articles rather than about button presses: grading the
// same article twice in a sitting must appear as one row, not two, with the
// second grade's state the one that shows.
func TestCalendarDayGroupsMultipleGradesIntoOneRow(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	if response := post(t, server, "/elements/1/grade", url.Values{"grade": {"next"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("grade: status = %d, want 303", response.Code)
	}
	if response := post(t, server, "/elements/1/grade", url.Values{"grade": {"done"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("grade: status = %d, want 303", response.Code)
	}

	today := time.Now().Format("2006-01-02")
	body := get(t, server, "/calendar/day/"+today).Body.String()
	if strings.Count(body, "A test article") != 1 {
		t.Errorf("day view shows the article %d times, want 1:\n%s",
			strings.Count(body, "A test article"), body)
	}
	if !strings.Contains(body, `class="badge badge-done">done<`) {
		t.Errorf("day view does not show the later grade winning:\n%s", body)
	}
	if !strings.Contains(body, "2 sessions") {
		t.Errorf("day view does not report 2 reading sessions:\n%s", body)
	}
}

// TestCalendarDaySeparatesExtractReviewFromArticleReads covers the split a
// day's own page exists to make: reviewing an extract is real work, but it is
// not reading the article, and must not inflate the articles-read count or
// appear in the articles list.
func TestCalendarDaySeparatesExtractReviewFromArticleReads(t *testing.T) {
	server, db, _ := newTestServer(t, true)

	extractID, err := db.CreateExtract(store.NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "a passage", ContentHTML: "<p>a passage</p>",
		Origin: store.OriginManual,
	}, time.Now())
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}
	if response := post(t, server, fmt.Sprintf("/elements/%d/grade", extractID),
		url.Values{"grade": {"done"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("grade: status = %d, want 303", response.Code)
	}

	today := time.Now().Format("2006-01-02")
	body := get(t, server, "/calendar/day/"+today).Body.String()
	if !strings.Contains(body, "No articles read this day.") {
		t.Errorf("day view counts an extract review as an article read:\n%s", body)
	}
	if !strings.Contains(body, "1 extract") && !strings.Contains(body, "1 revisited") {
		t.Errorf("day view does not surface the extract review anywhere:\n%s", body)
	}
}

// TestCalendarDayEmptyState covers a day with nothing logged.
func TestCalendarDayEmptyState(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	body := get(t, server, "/calendar/day/2020-01-01").Body.String()
	if !strings.Contains(body, "No articles read this day.") {
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

// TestDashboardLinksIntoTheCalendar guards the "click through to today" path
// from the dashboard's own read-today tile, and the link into the full
// calendar.
func TestDashboardLinksIntoTheCalendar(t *testing.T) {
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
