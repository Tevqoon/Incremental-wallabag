package store

import (
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/source"
)

func countActivity(t *testing.T, db *Store, kind string) int {
	t.Helper()
	var n int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM activity_log WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count activity_log: %v", err)
	}
	return n
}

// TestSaveScheduleReviewedLogsActivity is the core write-hook: grading an
// element through the reviewed path leaves a record of what happened and
// when, not just the schedule it landed on.
func TestSaveScheduleReviewedLogsActivity(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if err := db.SaveScheduleReviewed(1, ir.Schedule{State: ir.StateDone}, now); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}

	if n := countActivity(t, db, ActivityReview); n != 1 {
		t.Errorf("got %d review activity rows, want 1", n)
	}

	var grade, occurredOn string
	if err := db.DB().QueryRow(`SELECT grade, occurred_on FROM activity_log WHERE kind = ?`, ActivityReview).
		Scan(&grade, &occurredOn); err != nil {
		t.Fatalf("read logged row: %v", err)
	}
	if grade != string(ir.StateDone) {
		t.Errorf("grade = %q, want %q", grade, ir.StateDone)
	}
	if want := now.Format(dateFormat); occurredOn != want {
		t.Errorf("occurred_on = %q, want %q", occurredOn, want)
	}
}

// TestBacklogDoesNotLogActivity is the regression guard for the distinction
// between an actual grading decision and a backlog reschedule — Backlog's own
// doc comment calls the latter "not a grade", and the plain SaveSchedule path
// it uses must not be mistaken for review activity.
func TestBacklogDoesNotLogActivity(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	rescheduled := ir.Backlog(ir.Schedule{State: ir.StateNew}, 3, now)
	if err := db.SaveSchedule(1, rescheduled, now); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}

	if n := countActivity(t, db, ActivityReview); n != 0 {
		t.Errorf("a backlog reschedule logged %d review rows, want 0", n)
	}
}

// TestCreateExtractLogsManualNotImported: only a passage the reader pulled
// out themselves is "activity" — a highlight arriving from a bulk sync is
// not something that happened today, whatever day the sync ran on.
func TestCreateExtractLogsManualNotImported(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if _, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "mine", ContentHTML: "<p>mine</p>",
		Origin: OriginManual,
	}, now); err != nil {
		t.Fatalf("CreateExtract (manual): %v", err)
	}
	if _, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "imported", ContentHTML: "<p>imported</p>",
		Origin: OriginImport, ExternalRef: "97418",
	}, now); err != nil {
		t.Fatalf("CreateExtract (import): %v", err)
	}

	if n := countActivity(t, db, ActivityExtract); n != 1 {
		t.Errorf("got %d extract activity rows, want 1 (manual only)", n)
	}
}

// TestCurrentStreak covers the walk-back logic, including the edge case that
// matters most in practice: today has no review yet, which must not read as
// a broken streak before the reader has had a chance to open the queue.
func TestCurrentStreak(t *testing.T) {
	db := testStore(t)
	today := ir.Day(time.Now())

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Article", UpdatedAt: time.Now()},
	}, 0, 0, time.Now()); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	// seed grades a fresh extract as of daysAgo, so a review lands on that
	// exact calendar day — going through the same CreateExtract +
	// SaveScheduleReviewed path a real grading action takes, rather than
	// poking activity_log directly.
	seed := func(daysAgo int) {
		t.Helper()
		day := today.AddDate(0, 0, -daysAgo)
		id, err := db.CreateExtract(NewExtract{
			ParentID: 1, DocumentID: 1, Quote: "x", ContentHTML: "<p>x</p>",
		}, day)
		if err != nil {
			t.Fatalf("seed CreateExtract: %v", err)
		}
		if err := db.SaveScheduleReviewed(id, ir.Schedule{State: ir.StateDone}, day); err != nil {
			t.Fatalf("seed SaveScheduleReviewed: %v", err)
		}
	}

	// Reviewed yesterday and the day before, nothing today yet: streak should
	// count from yesterday, not read as broken.
	seed(1)
	seed(2)
	if got, err := db.CurrentStreak(today); err != nil || got != 2 {
		t.Errorf("streak (no activity today) = %d, %v; want 2, nil", got, err)
	}

	// Reviewed today too: streak extends by one.
	seed(0)
	if got, err := db.CurrentStreak(today); err != nil || got != 3 {
		t.Errorf("streak (activity today) = %d, %v; want 3, nil", got, err)
	}

	// A gap three days back breaks the streak there.
	seed(4)
	if got, err := db.CurrentStreak(today); err != nil || got != 3 {
		t.Errorf("streak (gap at day 3) = %d, %v; want 3, nil", got, err)
	}
}

// TestActivityHeatmapBucketsByDay covers the zero-filled range a fixed grid
// depends on, and that reviews and extracts are tallied separately per day.
func TestActivityHeatmapBucketsByDay(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	today := ir.Day(now)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if err := db.SaveScheduleReviewed(1, ir.Schedule{State: ir.StateDone}, now); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}
	if _, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "mine", ContentHTML: "<p>mine</p>",
		Origin: OriginManual,
	}, now); err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	from := today.AddDate(0, 0, -2)
	heatmap, err := db.ActivityHeatmap(from, today)
	if err != nil {
		t.Fatalf("ActivityHeatmap: %v", err)
	}
	if len(heatmap) != 3 {
		t.Fatalf("got %d days, want 3 (from..today inclusive)", len(heatmap))
	}
	if heatmap[0].Reviews != 0 || heatmap[0].Extracts != 0 {
		t.Errorf("day with no activity = %+v, want zeroes", heatmap[0])
	}
	last := heatmap[len(heatmap)-1]
	if last.Reviews != 1 || last.Extracts != 1 {
		t.Errorf("today = %+v, want 1 review and 1 extract", last)
	}
	if last.Articles != 1 {
		t.Errorf("today's articles = %d, want 1 (the one document reviewed)", last.Articles)
	}
	if last.Words != 1 {
		t.Errorf("today's words = %d, want 1 (the one-word extract quote \"mine\")", last.Words)
	}
}

// TestActivityCountsArticlesApartFromExtracts is the distinction the calendar
// is built on: "articles read" means a document whose root topic was reviewed.
// Reviewing an extract is real work and is counted — as an extract review, in
// its own column. Folding it into the article count (which counting every
// review's document_id does, since an extract carries its parent's) would
// report an article as read on a day it was never opened.
func TestActivityCountsArticlesApartFromExtracts(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	today := ir.Day(now)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	extractID, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "a passage", ContentHTML: "<p>a passage</p>",
		Origin: OriginImport, ExternalRef: "1", // imported: not activity of its own
	}, now)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	// Only the extract is reviewed today; the article itself is never opened.
	if err := db.SaveScheduleReviewed(extractID, ir.Schedule{State: ir.StateReading}, now); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}

	heatmap, err := db.ActivityHeatmap(today, today)
	if err != nil {
		t.Fatalf("ActivityHeatmap: %v", err)
	}
	day := heatmap[0]
	if day.Articles != 0 {
		t.Errorf("articles read = %d, want 0 — only an extract was reviewed", day.Articles)
	}
	if day.Reviews != 0 {
		t.Errorf("article reviews = %d, want 0", day.Reviews)
	}
	if day.ExtractReviews != 1 {
		t.Errorf("extract reviews = %d, want 1", day.ExtractReviews)
	}

	// Now read the article itself, to the end.
	if err := db.SaveScheduleReviewed(1, ir.Schedule{State: ir.StateDone}, now); err != nil {
		t.Fatalf("SaveScheduleReviewed (article): %v", err)
	}
	heatmap, err = db.ActivityHeatmap(today, today)
	if err != nil {
		t.Fatalf("ActivityHeatmap: %v", err)
	}
	day = heatmap[0]
	if day.Articles != 1 || day.Reviews != 1 {
		t.Errorf("articles = %d, reviews = %d; want 1 and 1", day.Articles, day.Reviews)
	}
	if day.Finished != 1 {
		t.Errorf("finished = %d, want 1 — the review landed on done", day.Finished)
	}
	if day.ExtractReviews != 1 {
		t.Errorf("extract reviews = %d, want 1 still", day.ExtractReviews)
	}
}

// TestActivityBetweenCountsArticlesOnceAcrossTheRange is the difference
// between a range rollup and adding up its days: an article read on two days
// is one article this week, and a caller summing DayCounts would say two.
func TestActivityBetweenCountsArticlesOnceAcrossTheRange(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	today := ir.Day(now)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "One", UpdatedAt: now},
		{ExternalID: "2", Title: "Two", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	// Article 1 read on two different days, article 2 on one of them.
	if err := db.SaveScheduleReviewed(1, ir.Schedule{State: ir.StateReading}, now.AddDate(0, 0, -2)); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}
	if err := db.SaveScheduleReviewed(1, ir.Schedule{State: ir.StateDone}, now); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}
	if err := db.SaveScheduleReviewed(2, ir.Schedule{State: ir.StateReading}, now); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}

	rollup, err := db.ActivityBetween(today.AddDate(0, 0, -6), today)
	if err != nil {
		t.Fatalf("ActivityBetween: %v", err)
	}
	if rollup.Articles != 2 {
		t.Errorf("articles = %d, want 2 distinct across the range", rollup.Articles)
	}
	if rollup.Reviews != 3 {
		t.Errorf("reviews = %d, want 3 grading passes", rollup.Reviews)
	}
	if rollup.Finished != 1 {
		t.Errorf("finished = %d, want 1", rollup.Finished)
	}
	if rollup.ActiveDays != 2 {
		t.Errorf("active days = %d, want 2", rollup.ActiveDays)
	}

	empty, err := db.ActivityBetween(today.AddDate(0, 0, -60), today.AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("ActivityBetween (quiet range): %v", err)
	}
	if empty != (Rollup{}) {
		t.Errorf("quiet range = %+v, want a zero rollup", empty)
	}
}

// TestActivityByMonthZeroFills covers the calendar's month strip: every month
// in the range gets a column, and a month's article count is distinct within
// that month.
func TestActivityByMonthZeroFills(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "One", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	// Twice in the same month, on two different days.
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, time.Local)
	if err := db.SaveScheduleReviewed(1, ir.Schedule{State: ir.StateReading}, firstOfMonth); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}
	if err := db.SaveScheduleReviewed(1, ir.Schedule{State: ir.StateReading}, firstOfMonth.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}

	// To the last day of the current month, so both reviews are in range
	// however early in the month the test happens to run.
	months, err := db.ActivityByMonth(firstOfMonth.AddDate(0, -3, 0), firstOfMonth.AddDate(0, 1, -1))
	if err != nil {
		t.Fatalf("ActivityByMonth: %v", err)
	}
	if len(months) != 4 {
		t.Fatalf("got %d months, want 4 (three back, inclusive of both ends)", len(months))
	}
	if months[0].Articles != 0 {
		t.Errorf("a month with nothing logged = %+v, want zeroes", months[0])
	}
	current := months[len(months)-1]
	if current.Articles != 1 {
		t.Errorf("articles this month = %d, want 1 — the same article on two days", current.Articles)
	}
	if current.Reviews != 2 {
		t.Errorf("reviews this month = %d, want 2", current.Reviews)
	}
	if current.ActiveDays != 2 {
		t.Errorf("active days this month = %d, want 2", current.ActiveDays)
	}
}

// TestArticlesReadBetweenReturnsDayArticlePairs covers the shape the
// dashboard buckets into weeks: one row per article per day it was read,
// however many times it was graded that day.
func TestArticlesReadBetweenReturnsDayArticlePairs(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	today := ir.Day(now)

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "One", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	// Two grading passes on the same article today, plus one yesterday.
	if err := db.SaveScheduleReviewed(1, ir.Schedule{State: ir.StateReading}, now); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}
	if err := db.SaveScheduleReviewed(1, ir.Schedule{State: ir.StateReading}, now); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}
	if err := db.SaveScheduleReviewed(1, ir.Schedule{State: ir.StateReading}, now.AddDate(0, 0, -1)); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}

	reads, err := db.ArticlesReadBetween(today.AddDate(0, 0, -6), today)
	if err != nil {
		t.Fatalf("ArticlesReadBetween: %v", err)
	}
	if len(reads) != 2 {
		t.Fatalf("got %d rows, want 2 (one per day the article was read)", len(reads))
	}
	if !reads[0].Day.Equal(today.AddDate(0, 0, -1)) || !reads[1].Day.Equal(today) {
		t.Errorf("rows = %+v, want yesterday then today", reads)
	}
	if reads[0].DocumentID != 1 || reads[1].DocumentID != 1 {
		t.Errorf("rows = %+v, want both against document 1", reads)
	}
}

// TestActivityOnListsTheDaysEvents covers the calendar's day view: both
// kinds of event, joined with the document they belong to, in the order
// they actually happened.
func TestActivityOnListsTheDaysEvents(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	if _, err := db.UpsertDocuments("wallabag", []source.Document{
		{ExternalID: "1", Title: "Article", UpdatedAt: now},
	}, 0, 0, now); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	if err := db.SaveScheduleReviewed(1, ir.Schedule{State: ir.StateDone}, now); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}
	// A minute later, so ordering by created_at is unambiguous rather than
	// relying on two equal timestamps happening to come back in insertion
	// order.
	extractedAt := now.Add(time.Minute)
	if _, err := db.CreateExtract(NewExtract{
		ParentID: 1, DocumentID: 1, Quote: "a passage worth keeping",
		ContentHTML: "<p>a passage worth keeping</p>", Origin: OriginManual,
	}, extractedAt); err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	entries, err := db.ActivityOn(ir.Day(now))
	if err != nil {
		t.Fatalf("ActivityOn: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	review := entries[0]
	if review.Kind != ActivityReview || review.Grade != string(ir.StateDone) {
		t.Errorf("first entry = %+v, want the review graded done", review)
	}
	if !review.IsRoot() || review.DocumentTitle != "Article" {
		t.Errorf("review entry does not carry its document: %+v", review)
	}

	extract := entries[1]
	if extract.Kind != ActivityExtract || extract.Grade != "" {
		t.Errorf("second entry = %+v, want the extract with no grade", extract)
	}
	if extract.Quote != "a passage worth keeping" || extract.DocumentTitle != "Article" {
		t.Errorf("extract entry = %+v, want its own quote and the parent's title", extract)
	}
	if extract.Words != 4 {
		t.Errorf("extract word count = %d, want 4", extract.Words)
	}
	if review.Words != 0 {
		t.Errorf("review word count = %d, want 0 — a root topic has no quote to count", review.Words)
	}

	empty, err := db.ActivityOn(ir.Day(now).AddDate(0, 0, -1))
	if err != nil {
		t.Fatalf("ActivityOn (empty day): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("got %d entries for a day with nothing logged, want 0", len(empty))
	}
}
