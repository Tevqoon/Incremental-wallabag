package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Tevqoon/increader/internal/store"
)

// calendarDay is one cell in the month grid.
type calendarDay struct {
	Date     time.Time
	InMonth  bool
	IsToday  bool
	Reviews  int
	Extracts int
	Articles int
	Words    int
	Total    int
}

// calendarData is the month view: a grid of complete weeks covering the
// requested month, padded with the trailing days of the month before and
// the leading days of the one after so every row is a full week.
type calendarData struct {
	Title      string
	MonthLabel string
	Days       []calendarDay
	PrevMonth  string
	NextMonth  string
}

// calendarDayData is one day's own page: everything logged that day, in the
// order it happened.
type calendarDayData struct {
	Title    string
	Day      time.Time
	Entries  []store.ActivityEntry
	Articles int
	Words    int
	PrevDay  string
	NextDay  string
	Month    string
}

// handleCalendar shows a month of activity, prev/next navigable, each day
// linking to its own page. Reuses ActivityHeatmap rather than a second
// query — the dashboard's 12-week strip and this are the same data at two
// different grains.
func (s *Server) handleCalendar(w http.ResponseWriter, r *http.Request) {
	month, err := parseMonth(r.URL.Query().Get("month"), s.today())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	firstOfMonth := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, month.Location())
	lastOfMonth := firstOfMonth.AddDate(0, 1, -1)

	// The grid starts on the Monday on or before the 1st and ends on the
	// Sunday on or after the last day, so every row is a complete week —
	// time.Weekday is Sunday=0..Saturday=6, so +6 mod 7 turns that into a
	// Monday-first offset.
	start := firstOfMonth.AddDate(0, 0, -((int(firstOfMonth.Weekday()) + 6) % 7))
	end := lastOfMonth.AddDate(0, 0, (7-(int(lastOfMonth.Weekday())+6)%7-1)%7)

	heatmap, err := s.store.ActivityHeatmap(start, end)
	if err != nil {
		s.fail(w, err)
		return
	}

	today := s.today()
	days := make([]calendarDay, len(heatmap))
	for i, d := range heatmap {
		days[i] = calendarDay{
			Date:     d.Date,
			InMonth:  d.Date.Month() == month.Month(),
			IsToday:  d.Date.Equal(today),
			Reviews:  d.Reviews,
			Extracts: d.Extracts,
			Articles: d.Articles,
			Words:    d.Words,
			Total:    d.Reviews + d.Extracts,
		}
	}

	s.render(w, "calendar.html", calendarData{
		Title:      "Calendar",
		MonthLabel: firstOfMonth.Format("January 2006"),
		Days:       days,
		PrevMonth:  firstOfMonth.AddDate(0, -1, 0).Format("2006-01"),
		NextMonth:  firstOfMonth.AddDate(0, 1, 0).Format("2006-01"),
	})
}

// parseMonth reads a "2006-01" month parameter, falling back to the month
// today is in when none was given.
func parseMonth(value string, today time.Time) (time.Time, error) {
	if value == "" {
		return today, nil
	}
	parsed, err := time.ParseInLocation("2006-01", value, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad month %q", value)
	}
	return parsed, nil
}

// handleCalendarDay shows everything logged on one day.
func (s *Server) handleCalendarDay(w http.ResponseWriter, r *http.Request) {
	day, err := time.ParseInLocation(dateLayout, r.PathValue("date"), time.Local)
	if err != nil {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}

	entries, err := s.store.ActivityOn(day)
	if err != nil {
		s.fail(w, err)
		return
	}

	articles := map[int64]bool{}
	words := 0
	for _, entry := range entries {
		if entry.Kind == store.ActivityReview {
			articles[entry.DocumentID] = true
		}
		words += entry.Words
	}

	s.render(w, "calendar_day.html", calendarDayData{
		Title:    "Calendar · " + day.Format("2 Jan 2006"),
		Day:      day,
		Entries:  entries,
		Articles: len(articles),
		Words:    words,
		PrevDay:  day.AddDate(0, 0, -1).Format(dateLayout),
		NextDay:  day.AddDate(0, 0, 1).Format(dateLayout),
		Month:    day.Format("2006-01"),
	})
}

// dateLayout is the calendar's own date-in-a-URL format — the same
// YYYY-MM-DD shape due_on and activity_log.occurred_on are stored in,
// spelled out here since neither is exported from the store package.
const dateLayout = "2006-01-02"
