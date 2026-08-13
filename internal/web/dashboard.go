package web

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/Tevqoon/increader/internal/store"
)

// dashboardPreviewLimit caps how many due items each queue's preview shows
// before pointing to the queue itself — a taste of what's next, not a second
// queue.
const dashboardPreviewLimit = 5

// dashboardWeeks is how far the weekly chart looks back: twelve weeks, long
// enough for a habit to be visible and short enough to still be about now.
const dashboardWeeks = 12

// dashboardTagLimit caps the tag breakdown to what's worth a glance; the
// library's own tag filter nav is where the rest live.
const dashboardTagLimit = 8

// weekBar is one bar in the weekly reading chart: articles read in a 7-day
// window, counted distinct within it — an article read on Monday and again on
// Thursday was one article that week.
type weekBar struct {
	From, To  time.Time
	Articles  int
	Percent   int
	IsCurrent bool
}

// dashboardData is what the dashboard page renders.
//
// The page is organised the way a session is: what to do now, how the reading
// has been going, then the two populations it draws on — articles, and the
// extracts taken out of them — with the second folded away, since a reader
// opening the dashboard is nearly always about to read rather than to
// audit the extract backlog.
type dashboardData struct {
	Title      string
	Today      time.Time
	TodayParam string

	// What's due, and a look at the head of each queue.
	Due            int
	ExtractsDue    int
	Preview        []store.QueueItem
	ExtractPreview []store.QueueItem
	Total          int

	// How the reading is going. ReadToday is articles, not reviews: the
	// question the number answers is "have I read anything today".
	ReadToday    int
	Streak       int
	WeekArticles int
	Trend        string
	Weeks        []weekBar
	WeeksTotal   int

	// The article backlog.
	Counts map[string]int
	Tags   []store.Tag

	// The extract side, behind its own disclosure.
	ExtractsToday  int
	WeekExtracts   int
	WeekWords      int
	ManualExtracts int
	ImportExtracts int
	ExtractsTotal  int
	Missing        int

	// Sync — a minor line, not a headline section.
	PendingWrites   int
	AbandonedWrites int
}

// handleDashboard is the app's home page: what is due now, how much has been
// read lately, and the shape of the backlog behind both. The queues
// themselves live at their own pages (see handleQueue); this shows the first
// few of each.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	today := s.today()

	preview, err := s.store.Queue(today, store.QueueArticles, dashboardPreviewLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	extractPreview, err := s.store.Queue(today, store.QueueExtracts, dashboardPreviewLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	due, extractsDue, err := s.dueCounts(today)
	if err != nil {
		s.fail(w, err)
		return
	}
	total, err := s.store.CountElements("")
	if err != nil {
		s.fail(w, err)
		return
	}
	// Empty, not "wallabag": once uploaded books exist too, naming one
	// source here would silently drop them from the backlog breakdown — see
	// CountByState's own doc comment for why "" means every source.
	counts, err := s.store.CountByState("", today)
	if err != nil {
		s.fail(w, err)
		return
	}

	tags, err := s.store.AllTags()
	if err != nil {
		s.fail(w, err)
		return
	}
	tags = topTags(tags, dashboardTagLimit)

	manual, err := s.store.CountExtracts(store.OriginManual)
	if err != nil {
		s.fail(w, err)
		return
	}
	imported, err := s.store.CountExtracts(store.OriginImport)
	if err != nil {
		s.fail(w, err)
		return
	}
	missing, err := s.store.CountMissingHighlights()
	if err != nil {
		s.fail(w, err)
		return
	}

	streak, err := s.store.CurrentStreak(today)
	if err != nil {
		s.fail(w, err)
		return
	}
	todayCount, err := s.store.ActivityBetween(today, today)
	if err != nil {
		s.fail(w, err)
		return
	}
	week, err := s.store.ActivityBetween(today.AddDate(0, 0, -6), today)
	if err != nil {
		s.fail(w, err)
		return
	}

	// One pass over the (day, article) pairs feeds every window on the page,
	// so the bar for this week and the "this week" figure beside it can never
	// disagree — they are the same count of the same rows.
	reads, err := s.store.ArticlesReadBetween(today.AddDate(0, 0, -(dashboardWeeks*7-1)), today)
	if err != nil {
		s.fail(w, err)
		return
	}
	weeks := weeklyArticleBars(reads, today, dashboardWeeks)
	weeksTotal := distinctArticles(reads, time.Time{}, today)
	lastWeek := distinctArticles(reads, today.AddDate(0, 0, -13), today.AddDate(0, 0, -7))

	queued, abandoned, err := s.store.CountPendingWrites("wallabag")
	if err != nil {
		s.fail(w, err)
		return
	}

	s.render(w, "dashboard.html", dashboardData{
		Title:      "Dashboard",
		Today:      today,
		TodayParam: today.Format(dateLayout),

		Due:            due,
		ExtractsDue:    extractsDue,
		Preview:        preview,
		ExtractPreview: extractPreview,
		Total:          total,

		ReadToday:    todayCount.Articles,
		Streak:       streak,
		WeekArticles: week.Articles,
		Trend:        trend(week.Articles, lastWeek),
		Weeks:        weeks,
		WeeksTotal:   weeksTotal,

		Counts: counts,
		Tags:   tags,

		ExtractsToday:  todayCount.Extracts,
		WeekExtracts:   week.Extracts,
		WeekWords:      week.Words,
		ManualExtracts: manual,
		ImportExtracts: imported,
		ExtractsTotal:  manual + imported,
		Missing:        missing,

		PendingWrites:   queued,
		AbandonedWrites: abandoned,
	})
}

// topTags returns the n tags with the most documents, most-used first —
// AllTags itself is alphabetical, right for the library's filter nav but not
// for a breakdown meant to lead with what matters most.
func topTags(tags []store.Tag, n int) []store.Tag {
	sorted := make([]store.Tag, len(tags))
	copy(sorted, tags)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Documents > sorted[j].Documents })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

// weeklyArticleBars buckets (day, article) pairs into 7-day windows ending
// today, oldest first, so the chart reads left to right the same direction
// time does. The last bar is the current week, which is normally still in
// progress — hence IsCurrent, so the template can say so rather than let a
// short bar read as a bad week.
func weeklyArticleBars(reads []store.ArticleRead, today time.Time, weeks int) []weekBar {
	if weeks < 1 {
		return nil
	}

	start := today.AddDate(0, 0, -(weeks*7 - 1))
	seen := make([]map[int64]bool, weeks)
	for i := range seen {
		seen[i] = map[int64]bool{}
	}
	for _, read := range reads {
		bucket := daysBetween(start, read.Day) / 7
		if bucket < 0 || bucket >= weeks {
			continue
		}
		seen[bucket][read.DocumentID] = true
	}

	bars := make([]weekBar, weeks)
	max := 0
	for i := range bars {
		from := start.AddDate(0, 0, i*7)
		bars[i] = weekBar{
			From:      from,
			To:        from.AddDate(0, 0, 6),
			Articles:  len(seen[i]),
			IsCurrent: i == weeks-1,
		}
		if bars[i].Articles > max {
			max = bars[i].Articles
		}
	}
	if max == 0 {
		max = 1
	}
	for i := range bars {
		bars[i].Percent = bars[i].Articles * 100 / max
	}
	return bars
}

// distinctArticles counts the articles read in [from, to], each once however
// many days it was read on. A zero from means "from the beginning of what was
// loaded", which is how the whole-chart total is taken.
func distinctArticles(reads []store.ArticleRead, from, to time.Time) int {
	seen := map[int64]bool{}
	for _, read := range reads {
		if !from.IsZero() && read.Day.Before(from) {
			continue
		}
		if read.Day.After(to) {
			continue
		}
		seen[read.DocumentID] = true
	}
	return len(seen)
}

// trend phrases this week against last week. Deliberately a sentence rather
// than a signed number: "3 more than last week" needs no legend, and a lone
// "+3" beside a count invites reading it as part of the count.
func trend(thisWeek, lastWeek int) string {
	switch {
	case lastWeek == 0 && thisWeek == 0:
		return "nothing last week either"
	case lastWeek == 0:
		return "nothing last week"
	case thisWeek == lastWeek:
		return "same as last week"
	case thisWeek > lastWeek:
		return fmt.Sprintf("%d more than last week", thisWeek-lastWeek)
	default:
		return fmt.Sprintf("%d fewer than last week", lastWeek-thisWeek)
	}
}

// daysBetween counts whole days from one midnight to another. Rounding to the
// nearest day rather than truncating is what makes it survive a daylight
// saving change inside the range, where the elapsed time between two local
// midnights is 23 or 25 hours and a truncating divide would lose a day.
func daysBetween(from, to time.Time) int {
	return int(to.Sub(from).Round(24*time.Hour) / (24 * time.Hour))
}
