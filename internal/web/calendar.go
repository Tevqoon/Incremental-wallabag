package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/store"
)

// calendarStripMonths is how many months the bar strip under the grid covers,
// ending with the month on screen — a year of reading, so a month reads as
// busy or quiet against the ones around it rather than in isolation.
const calendarStripMonths = 12

// calendarDay is one cell in the month grid. The tally is embedded rather
// than copied field by field so the cell cannot drift out of step with what
// the store counted.
type calendarDay struct {
	store.DayCount

	InMonth bool
	IsToday bool

	// IsFuture dims days that have not happened yet: an empty cell before
	// today means "nothing read", an empty cell after it means nothing at all.
	IsFuture bool

	// Level is the shade, 0..4 — see dayLevel.
	Level int
}

// monthBar is one column of the twelve-month strip.
type monthBar struct {
	Label    string // "Sep"
	Title    string // "September 2026: 31 articles"
	Param    string // "2026-09", for /calendar?month=
	Articles int
	Percent  int
	IsShown  bool // the month currently on screen
}

// calendarData is the month view: a grid of complete weeks covering the
// requested month, padded with the trailing days of the month before and
// the leading days of the one after so every row is a full week.
type calendarData struct {
	Title      string
	MonthLabel string

	Days      []calendarDay
	PrevMonth string
	NextMonth string

	// IsThisMonth suppresses the way-back link when it would go nowhere.
	IsThisMonth bool

	// Month is this month's own reading, and Busiest the day that carried
	// most of it — the two figures the grid itself cannot state outright.
	Month   store.Rollup
	Busiest calendarDay

	// Days in the month, for reading ActiveDays as a proportion.
	MonthDays int

	Months     []monthBar
	StripTotal int
}

// dayArticle is one article read on a day, with that day's grading passes
// folded into a single row.
//
// The activity log records one row per grade, so an article picked up three
// times in a sitting appears three times; a list that showed each of them
// would say more about the grading buttons than about the reading. What the
// day is actually a record of is which articles were read, so that is the row.
type dayArticle struct {
	ElementID  int64
	DocumentID int64
	Title      string

	Reviews int
	// Grade is the state the last review of the day landed on, which is the
	// one that stuck.
	Grade    string
	Finished bool

	// Extracts is how many passages were harvested from this article that
	// day — the visible product of having read it.
	Extracts int
}

// calendarDayData is one day's own page: what was read, and separately what
// was pulled out of it or revisited.
type calendarDayData struct {
	Title   string
	Day     time.Time
	IsToday bool

	Articles  []dayArticle
	Harvested []store.ActivityEntry
	Revisited []store.ActivityEntry

	Reviews  int
	Finished int
	Words    int

	PrevDay string
	NextDay string
	Month   string
}

// handleCalendar shows a month of reading, prev/next navigable, each day
// linking to its own page.
//
// The number in a cell is articles read that day, and the shading follows it:
// the calendar answers "how much did I read, and when", so the one figure it
// puts in front of you should be the one it is about. Everything else the day
// carried — extracts taken, passages revisited — is behind the cell, on the
// day's own page.
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
	var busiest calendarDay
	for i, count := range heatmap {
		day := calendarDay{
			DayCount: count,
			InMonth:  count.Date.Month() == month.Month() && count.Date.Year() == month.Year(),
			IsToday:  count.Date.Equal(today),
			IsFuture: count.Date.After(today),
			Level:    dayLevel(count),
		}
		days[i] = day
		if day.InMonth && day.Articles > busiest.Articles {
			busiest = day
		}
	}

	summary, err := s.store.ActivityBetween(firstOfMonth, lastOfMonth)
	if err != nil {
		s.fail(w, err)
		return
	}

	stripFrom := firstOfMonth.AddDate(0, -(calendarStripMonths - 1), 0)
	months, err := s.store.ActivityByMonth(stripFrom, lastOfMonth)
	if err != nil {
		s.fail(w, err)
		return
	}
	bars, stripTotal := monthBars(months, firstOfMonth)

	s.render(w, "calendar.html", calendarData{
		Title:       "Calendar",
		MonthLabel:  firstOfMonth.Format("January 2006"),
		Days:        days,
		PrevMonth:   firstOfMonth.AddDate(0, -1, 0).Format(monthLayout),
		NextMonth:   firstOfMonth.AddDate(0, 1, 0).Format(monthLayout),
		IsThisMonth: firstOfMonth.Year() == today.Year() && firstOfMonth.Month() == today.Month(),
		Month:       summary,
		Busiest:     busiest,
		MonthDays:   lastOfMonth.Day(),
		Months:      bars,
		StripTotal:  stripTotal,
	})
}

// dayLevel buckets a day into the four shades the grid paints.
//
// Articles decide the shade, because that is what the grid is counting. A day
// spent entirely on extracts — reviewing passages, harvesting new ones —
// still gets the faintest shade rather than reading as a blank: it was a day
// of work, just not of articles.
func dayLevel(day store.DayCount) int {
	switch {
	case day.Articles >= 5:
		return 4
	case day.Articles >= 3:
		return 3
	case day.Articles >= 1:
		return 2
	case day.Extracts+day.ExtractReviews > 0:
		return 1
	default:
		return 0
	}
}

// monthBars turns monthly rollups into the strip's columns, scaled to the
// busiest month in view, and reports the total articles across them.
func monthBars(months []store.MonthCount, shown time.Time) ([]monthBar, int) {
	max, total := 0, 0
	for _, month := range months {
		if month.Articles > max {
			max = month.Articles
		}
		total += month.Articles
	}
	if max == 0 {
		max = 1
	}

	bars := make([]monthBar, 0, len(months))
	for _, month := range months {
		bars = append(bars, monthBar{
			Label: month.Month.Format("Jan"),
			Title: fmt.Sprintf("%s: %d article%s read",
				month.Month.Format("January 2006"), month.Articles, plural(month.Articles)),
			Param:    month.Month.Format(monthLayout),
			Articles: month.Articles,
			Percent:  month.Articles * 100 / max,
			IsShown: month.Month.Year() == shown.Year() &&
				month.Month.Month() == shown.Month(),
		})
	}
	return bars, total
}

// parseMonth reads a "2006-01" month parameter, falling back to the month
// today is in when none was given.
func parseMonth(value string, today time.Time) (time.Time, error) {
	if value == "" {
		return today, nil
	}
	parsed, err := time.ParseInLocation(monthLayout, value, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad month %q", value)
	}
	return parsed, nil
}

// handleCalendarDay shows one day: the articles read, and behind a disclosure
// the extract work — passages harvested and passages revisited — so the page
// leads with reading and keeps the rest a click away.
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

	data := calendarDayData{
		Title:   "Calendar · " + day.Format("2 Jan 2006"),
		Day:     day,
		IsToday: day.Equal(s.today()),
		PrevDay: day.AddDate(0, 0, -1).Format(dateLayout),
		NextDay: day.AddDate(0, 0, 1).Format(dateLayout),
		Month:   day.Format(monthLayout),
	}

	// byDocument keeps each article's row addressable as further entries for
	// it arrive, since the log is ordered by time rather than by article.
	byDocument := map[int64]*dayArticle{}
	var articles []*dayArticle
	for _, entry := range entries {
		switch {
		case entry.Kind == store.ActivityReview && entry.IsRoot():
			article, seen := byDocument[entry.DocumentID]
			if !seen {
				article = &dayArticle{
					ElementID:  entry.ID,
					DocumentID: entry.DocumentID,
					Title:      entry.DocumentTitle,
				}
				byDocument[entry.DocumentID] = article
				articles = append(articles, article)
			}
			article.Reviews++
			article.Grade = entry.Grade
			article.Finished = entry.Grade == string(ir.StateDone)
			data.Reviews++
		case entry.Kind == store.ActivityReview:
			data.Revisited = append(data.Revisited, entry)
		default:
			data.Harvested = append(data.Harvested, entry)
			data.Words += entry.Words
		}
	}

	// Extracts are attributed to the article they came out of, whether or not
	// that article was itself read today — a passage harvested from an
	// article read yesterday still belongs beside it, and one whose article
	// has no row simply does not get counted here.
	for _, entry := range data.Harvested {
		if article, ok := byDocument[entry.DocumentID]; ok {
			article.Extracts++
		}
	}

	for _, article := range articles {
		if article.Finished {
			data.Finished++
		}
		data.Articles = append(data.Articles, *article)
	}

	s.render(w, "calendar_day.html", data)
}

// dateLayout is the calendar's own date-in-a-URL format — the same
// YYYY-MM-DD shape due_on and activity_log.occurred_on are stored in,
// spelled out here since neither is exported from the store package.
const dateLayout = "2006-01-02"

// monthLayout is the same thing a month at a time, for ?month= and for the
// strip's links.
const monthLayout = "2006-01"

// plural is the "s" on a count, for the summary sentences assembled in Go
// rather than in a template.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
