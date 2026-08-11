package web

import (
	"net/http"
	"sort"
	"time"

	"github.com/Tevqoon/increader/internal/store"
)

// dashboardPreviewLimit caps how many due items the dashboard shows before
// pointing to the full queue — a taste of what's next, not a second queue.
const dashboardPreviewLimit = 5

// dashboardHeatmapDays covers twelve weeks, long enough to see a pattern in
// the activity heatmap without the grid growing unwieldy.
const dashboardHeatmapDays = 84

// dashboardTagLimit caps the tag breakdown to what's worth a glance; the
// library's own tag filter nav is where the rest live.
const dashboardTagLimit = 8

// weekBar is one bar in the weekly review chart: a week's worth of daily
// activity-heatmap counts, rolled up.
type weekBar struct {
	From, To time.Time
	Reviews  int
	Articles int
	Words    int
	Percent  int
}

// dashboardData is what the dashboard page renders.
type dashboardData struct {
	Title string
	Today time.Time

	// Queue & backlog health. Preview is the reading queue, which is where a
	// session starts; ExtractsDue carries the other queue's size so that a
	// growing extract backlog is visible from here without opening it — the
	// one thing separate queues cost that the old interleave gave away for
	// free, by putting extracts in front of you whether you asked or not.
	Due         int
	ExtractsDue int
	Total       int
	Preview     []store.QueueItem
	Counts      map[string]int

	// Reading composition
	Tags           []store.Tag
	ManualExtracts int
	ImportExtracts int
	ExtractsTotal  int
	Missing        int

	// Reading activity & streaks
	Streak       int
	WeekReviews  int
	WeekArticles int
	WeekWords    int
	Heatmap      []store.DayCount
	Weeks        []weekBar

	// Sync — a minor line, not a headline section
	PendingWrites   int
	AbandonedWrites int
}

// handleDashboard is the app's home page: a status snapshot — queue/backlog
// health, reading composition, and now reading activity over time — plus a
// preview of what's next. The queue itself lives at its own page (see
// handleQueue); this only shows the first few due items.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	today := s.today()

	preview, err := s.store.Queue(today, store.QueueArticles, dashboardPreviewLimit)
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
	heatmap, err := s.store.ActivityHeatmap(today.AddDate(0, 0, -(dashboardHeatmapDays-1)), today)
	if err != nil {
		s.fail(w, err)
		return
	}
	weeks := weeklyBars(heatmap)
	var weekReviews, weekArticles, weekWords int
	if len(weeks) > 0 {
		last := weeks[len(weeks)-1]
		weekReviews, weekArticles, weekWords = last.Reviews, last.Articles, last.Words
	}

	queued, abandoned, err := s.store.CountPendingWrites("wallabag")
	if err != nil {
		s.fail(w, err)
		return
	}

	s.render(w, "dashboard.html", dashboardData{
		Title:           "Dashboard",
		Today:           today,
		Due:             due,
		ExtractsDue:     extractsDue,
		Total:           total,
		Preview:         preview,
		Counts:          counts,
		Tags:            tags,
		ManualExtracts:  manual,
		ImportExtracts:  imported,
		ExtractsTotal:   manual + imported,
		Missing:         missing,
		Streak:          streak,
		WeekReviews:     weekReviews,
		WeekArticles:    weekArticles,
		WeekWords:       weekWords,
		Heatmap:         heatmap,
		Weeks:           weeks,
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

// weeklyBars rolls daily heatmap counts into 7-day buckets, oldest first, so
// the chart reads left to right the same direction time does.
// dashboardHeatmapDays is a multiple of 7, so every bucket is a full week and
// the last one is always the current week, ending today.
func weeklyBars(days []store.DayCount) []weekBar {
	if len(days) == 0 {
		return nil
	}

	var weeks []weekBar
	max := 0
	for i := 0; i < len(days); i += 7 {
		end := i + 7
		if end > len(days) {
			end = len(days)
		}
		bucket := days[i:end]
		// Articles sums each day's distinct-document count across the week,
		// so an article reviewed on two different days counts twice — once
		// per day it was actually touched, which is what "articles this
		// week" means here, as opposed to "distinct articles this week".
		var reviews, articles, words int
		for _, d := range bucket {
			reviews += d.Reviews
			articles += d.Articles
			words += d.Words
		}
		if reviews > max {
			max = reviews
		}
		weeks = append(weeks, weekBar{
			From:     bucket[0].Date,
			To:       bucket[len(bucket)-1].Date,
			Reviews:  reviews,
			Articles: articles,
			Words:    words,
		})
	}
	if max == 0 {
		max = 1
	}
	for i := range weeks {
		weeks[i].Percent = weeks[i].Reviews * 100 / max
	}
	return weeks
}
