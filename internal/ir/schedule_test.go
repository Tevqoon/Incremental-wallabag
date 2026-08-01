package ir

import (
	"math"
	"testing"
	"time"
)

var today = time.Date(2026, 7, 31, 14, 30, 0, 0, time.UTC)

func TestIntervalsGrowByAFactor(t *testing.T) {
	// A fresh topic at the lowest priority, so nothing is capped and the raw
	// A-Factor progression is visible.
	schedule := Schedule{Priority: 1.0}

	want := []float64{1, 2, 4, 8, 16, 32}
	for repetition, wantInterval := range want {
		schedule = Next(schedule, GradeNext, today)

		if !closeEnough(schedule.IntervalDays, wantInterval) {
			t.Errorf("repetition %d: interval = %.2f, want %.2f",
				repetition+1, schedule.IntervalDays, wantInterval)
		}
		if schedule.Reps != repetition+1 {
			t.Errorf("repetition %d: reps = %d", repetition+1, schedule.Reps)
		}
		if schedule.State != StateReading {
			t.Errorf("repetition %d: state = %q, want %q", repetition+1, schedule.State, StateReading)
		}
	}
}

func TestFirstRepetitionIsOneDay(t *testing.T) {
	schedule := Next(Schedule{Priority: 1.0}, GradeNext, today)

	if !closeEnough(schedule.IntervalDays, 1) {
		t.Errorf("interval = %.2f, want 1", schedule.IntervalDays)
	}
	want := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if !schedule.DueOn.Equal(want) {
		t.Errorf("due = %v, want %v", schedule.DueOn, want)
	}
}

func TestLaterPushesOutAndCompounds(t *testing.T) {
	// Postponing repeatedly must make a topic recede faster each time — that
	// is how uninteresting material drains out of the queue without ever
	// being explicitly abandoned.
	schedule := Schedule{IntervalDays: 4, AFactor: 2.0, Priority: 1.0}

	first := Next(schedule, GradeDefer, today)
	if !closeEnough(first.AFactor, 2.4) {
		t.Errorf("A-Factor after one Later = %.3f, want 2.4", first.AFactor)
	}
	if !closeEnough(first.IntervalDays, 9.6) {
		t.Errorf("interval after one Later = %.3f, want 9.6", first.IntervalDays)
	}

	second := Next(first, GradeDefer, today)
	if second.AFactor <= first.AFactor {
		t.Errorf("A-Factor did not compound: %.3f then %.3f", first.AFactor, second.AFactor)
	}

	// And an ordinary Next afterwards inherits the raised A-Factor.
	third := Next(second, GradeNext, today)
	if !closeEnough(third.AFactor, second.AFactor) {
		t.Errorf("Next changed the A-Factor: %.3f then %.3f", second.AFactor, third.AFactor)
	}
}

func TestSoonerShortensAndSlowsGrowth(t *testing.T) {
	schedule := Schedule{IntervalDays: 8, AFactor: 2.0, Priority: 1.0}

	got := Next(schedule, GradeSooner, today)

	if !closeEnough(got.IntervalDays, 4) {
		t.Errorf("interval = %.3f, want 4 (half of 8)", got.IntervalDays)
	}
	if got.AFactor >= 2.0 {
		t.Errorf("A-Factor = %.3f, want it reduced below 2.0", got.AFactor)
	}
	// Sooner must never push a topic further out than it already was.
	if got.IntervalDays > schedule.IntervalDays {
		t.Errorf("Sooner increased the interval from %.1f to %.1f",
			schedule.IntervalDays, got.IntervalDays)
	}
}

func TestAFactorIsClamped(t *testing.T) {
	// Repeated Later must not run away to a decade.
	schedule := Schedule{IntervalDays: 1, AFactor: maxAFactor, Priority: 1.0}
	for i := 0; i < 20; i++ {
		schedule = Next(schedule, GradeDefer, today)
	}
	if schedule.AFactor > maxAFactor {
		t.Errorf("A-Factor = %.3f, want at most %.3f", schedule.AFactor, maxAFactor)
	}

	// Repeated Sooner must not collapse to no growth at all.
	schedule = Schedule{IntervalDays: 100, AFactor: minAFactor, Priority: 1.0}
	for i := 0; i < 20; i++ {
		schedule = Next(schedule, GradeSooner, today)
	}
	if schedule.AFactor < minAFactor {
		t.Errorf("A-Factor = %.3f, want at least %.3f", schedule.AFactor, minAFactor)
	}
	if schedule.IntervalDays < minInterval {
		t.Errorf("interval = %.3f, want at least %.3f", schedule.IntervalDays, minInterval)
	}
}

// TestPriorityCapsTheInterval is what stops important material from drifting
// out of sight: however many times it is graded, high priority keeps it close.
func TestPriorityCapsTheInterval(t *testing.T) {
	tests := []struct {
		name        string
		priority    float64
		wantAtMost  float64
		wantAtLeast float64
	}{
		{"highest priority stays within a week", 0.0, 7.1, 6.9},
		{"middling priority settles in months", 0.5, 51, 49},
		{"lowest priority may drift a year out", 1.0, 366, 364},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schedule := Schedule{Priority: test.priority}
			// Far more repetitions than needed to reach the ceiling.
			for i := 0; i < 40; i++ {
				schedule = Next(schedule, GradeNext, today)
			}
			if schedule.IntervalDays > test.wantAtMost {
				t.Errorf("interval settled at %.1f, want at most %.1f",
					schedule.IntervalDays, test.wantAtMost)
			}
			if schedule.IntervalDays < test.wantAtLeast {
				t.Errorf("interval settled at %.1f, want at least %.1f",
					schedule.IntervalDays, test.wantAtLeast)
			}
		})
	}
}

func TestPriorityOrdersIntervals(t *testing.T) {
	// More important material must always come back sooner than less
	// important material, at every point in the progression.
	high := Schedule{Priority: 0.1}
	low := Schedule{Priority: 0.9}

	for repetition := 1; repetition <= 15; repetition++ {
		high = Next(high, GradeNext, today)
		low = Next(low, GradeNext, today)

		if high.IntervalDays > low.IntervalDays {
			t.Fatalf("repetition %d: high-priority interval %.1f exceeds low-priority %.1f",
				repetition, high.IntervalDays, low.IntervalDays)
		}
	}
}

func TestTerminalGrades(t *testing.T) {
	schedule := Schedule{IntervalDays: 8, AFactor: 2, Reps: 3}

	done := Next(schedule, GradeDone, today)
	if done.State != StateDone {
		t.Errorf("state = %q, want %q", done.State, StateDone)
	}
	if !done.DueOn.IsZero() {
		t.Errorf("finished material has a due date: %v", done.DueOn)
	}
	if done.Due(today) {
		t.Error("finished material is still reported as due")
	}

	dismissed := Next(schedule, GradeDismiss, today)
	if dismissed.State != StateDismissed {
		t.Errorf("state = %q, want %q", dismissed.State, StateDismissed)
	}
	if dismissed.Due(today) {
		t.Error("dismissed material is still reported as due")
	}
}

func TestDue(t *testing.T) {
	tests := []struct {
		name     string
		schedule Schedule
		want     bool
	}{
		{"never scheduled", Schedule{State: StateNew}, true},
		{"due yesterday", Schedule{State: StateReading, DueOn: today.AddDate(0, 0, -1)}, true},
		{"due today", Schedule{State: StateReading, DueOn: today}, true},
		{"due tomorrow", Schedule{State: StateReading, DueOn: today.AddDate(0, 0, 1)}, false},
		{"finished", Schedule{State: StateDone, DueOn: today.AddDate(0, 0, -5)}, false},
		{"dismissed", Schedule{State: StateDismissed}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.schedule.Due(today); got != test.want {
				t.Errorf("Due = %v, want %v", got, test.want)
			}
		})
	}
}

// TestDueIgnoresTimeOfDay is the day-boundary case: something scheduled for
// today must be due all day, whatever hour it is compared against.
func TestDueIgnoresTimeOfDay(t *testing.T) {
	dueDate := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	schedule := Schedule{State: StateReading, DueOn: dueDate}

	for _, hour := range []int{0, 6, 14, 23} {
		moment := time.Date(2026, 7, 31, hour, 59, 59, 0, time.UTC)
		if !schedule.Due(moment) {
			t.Errorf("not due at %02d:59 on its own due date", hour)
		}
	}

	justBefore := time.Date(2026, 7, 30, 23, 59, 59, 0, time.UTC)
	if schedule.Due(justBefore) {
		t.Error("due a second before its due date began")
	}
}

// TestDayUsesLocalLocation guards the timezone decision: "today" must mean the
// reader's today, not UTC's. In Ljubljana, 00:30 local is still the previous
// day in UTC, and truncating in the wrong zone shifts every due date.
func TestDayUsesLocalLocation(t *testing.T) {
	ljubljana, err := time.LoadLocation("Europe/Ljubljana")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	justAfterMidnight := time.Date(2026, 7, 31, 0, 30, 0, 0, ljubljana)
	day := Day(justAfterMidnight)

	if day.Day() != 31 || day.Month() != time.July {
		t.Errorf("Day() = %v, want 31 July in the local zone", day)
	}
	if day.Location() != ljubljana {
		t.Errorf("Day() moved to %v, want it to stay in %v", day.Location(), ljubljana)
	}
	// The same instant in UTC is still 30 July, which is exactly the trap.
	if justAfterMidnight.UTC().Day() != 30 {
		t.Fatal("test premise is wrong: the instant should be the previous day in UTC")
	}
}

func TestZeroValueScheduleGetsDefaults(t *testing.T) {
	// An element created before a field existed, or read from a row with
	// defaults, must not produce a zero A-Factor and freeze in place.
	got := Next(Schedule{}, GradeNext, today)

	if got.AFactor < minAFactor {
		t.Errorf("A-Factor = %.3f, want the default of %.1f", got.AFactor, defaultAFactor)
	}
	if got.IntervalDays < minInterval {
		t.Errorf("interval = %.3f, want at least %.1f", got.IntervalDays, minInterval)
	}
	if got.DueOn.IsZero() {
		t.Error("no due date was set")
	}
}

func TestRenderCloze(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		clozes  []Cloze
		want    string
		wantErr bool
	}{
		{
			name: "no deletions leaves the text alone",
			text: "The capital of France is Paris.",
			want: "The capital of France is Paris.",
		},
		{
			name:   "one deletion",
			text:   "The capital of France is Paris.",
			clozes: []Cloze{{Ordinal: 1, Start: 25, End: 30}},
			want:   "The capital of France is {{c1::Paris}}.",
		},
		{
			name: "two deletions become two cards",
			text: "The capital of France is Paris.",
			clozes: []Cloze{
				{Ordinal: 1, Start: 25, End: 30},
				{Ordinal: 2, Start: 15, End: 21},
			},
			want: "The capital of {{c2::France}} is {{c1::Paris}}.",
		},
		{
			name:   "a hint is appended",
			text:   "The capital of France is Paris.",
			clozes: []Cloze{{Ordinal: 1, Start: 25, End: 30, Hint: "city"}},
			want:   "The capital of France is {{c1::Paris::city}}.",
		},
		{
			name:   "a deletion at the very start",
			text:   "Paris is the capital.",
			clozes: []Cloze{{Ordinal: 1, Start: 0, End: 5}},
			want:   "{{c1::Paris}} is the capital.",
		},
		{
			name:   "a deletion running to the very end",
			text:   "The capital is Paris",
			clozes: []Cloze{{Ordinal: 1, Start: 15, End: 20}},
			want:   "The capital is {{c1::Paris}}",
		},
		{
			name: "overlapping deletions are rejected",
			text: "The capital of France is Paris.",
			clozes: []Cloze{
				{Ordinal: 1, Start: 4, End: 15},
				{Ordinal: 2, Start: 10, End: 21},
			},
			wantErr: true,
		},
		{
			name:    "a deletion past the end is rejected",
			text:    "Short.",
			clozes:  []Cloze{{Ordinal: 1, Start: 0, End: 99}},
			wantErr: true,
		},
		{
			name:    "an empty deletion is rejected",
			text:    "Short.",
			clozes:  []Cloze{{Ordinal: 1, Start: 2, End: 2}},
			wantErr: true,
		},
		{
			name:    "ordinal zero is rejected, Anki numbers from one",
			text:    "Short.",
			clozes:  []Cloze{{Ordinal: 0, Start: 0, End: 5}},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := RenderCloze(test.text, test.clozes)

			if test.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("RenderCloze: %v", err)
			}
			if got != test.want {
				t.Errorf("got  %q\nwant %q", got, test.want)
			}
		})
	}
}

func TestNextOrdinal(t *testing.T) {
	if got := NextOrdinal(nil); got != 1 {
		t.Errorf("first ordinal = %d, want 1", got)
	}
	if got := NextOrdinal([]Cloze{{Ordinal: 1}, {Ordinal: 2}}); got != 3 {
		t.Errorf("next ordinal = %d, want 3", got)
	}
	// Ordinals must not be reused after a deletion is removed, or the new
	// cloze would silently merge with the old card's history in Anki.
	if got := NextOrdinal([]Cloze{{Ordinal: 1}, {Ordinal: 5}}); got != 6 {
		t.Errorf("next ordinal after a gap = %d, want 6", got)
	}
}

func closeEnough(got, want float64) bool {
	return math.Abs(got-want) < 0.001
}

// TestSuspendIsReversible is what separates suspension from Done and Dismiss:
// it parks material without discarding the progress made on it, so resuming
// carries on rather than starting over.
func TestSuspendIsReversible(t *testing.T) {
	schedule := Schedule{
		State: StateReading, IntervalDays: 8, AFactor: 2.4, Reps: 3, Priority: 0.3,
	}

	got := Next(schedule, GradeSuspend, today)

	if got.State != StateSuspended {
		t.Errorf("state = %q, want %q", got.State, StateSuspended)
	}
	if got.Due(today) {
		t.Error("a suspended element is still reported as due")
	}
	if !got.DueOn.IsZero() {
		t.Errorf("suspended element kept a due date: %v", got.DueOn)
	}

	// Everything needed to resume must survive.
	if got.IntervalDays != schedule.IntervalDays {
		t.Errorf("interval = %.1f, want %.1f preserved", got.IntervalDays, schedule.IntervalDays)
	}
	if got.Reps != schedule.Reps {
		t.Errorf("reps = %d, want %d preserved", got.Reps, schedule.Reps)
	}
	if !closeEnough(got.AFactor, schedule.AFactor) {
		t.Errorf("A-Factor = %.2f, want %.2f preserved", got.AFactor, schedule.AFactor)
	}

	// Unlike Done, which also clears the due date, this is not terminal: the
	// next ordinary grade picks up from the preserved interval.
	resumed := Next(got, GradeNext, today)
	if resumed.State != StateReading {
		t.Errorf("resumed state = %q, want %q", resumed.State, StateReading)
	}
	if resumed.IntervalDays <= schedule.IntervalDays {
		t.Errorf("resumed interval %.1f did not grow from %.1f",
			resumed.IntervalDays, schedule.IntervalDays)
	}
}

func TestSuspendedIsNotDue(t *testing.T) {
	suspended := Schedule{State: StateSuspended, DueOn: today.AddDate(0, 0, -5)}
	if suspended.Due(today) {
		t.Error("a suspended element with a past due date is reported as due")
	}
}

func TestFormatInterval(t *testing.T) {
	tests := []struct {
		days float64
		want string
	}{
		{0, "today"},
		{0.4, "today"},
		{1, "1d"},
		{1.4, "1d"},
		{2.6, "3d"},
		{29, "29d"},
		{30, "1mo"},
		{45, "1.5mo"},
		{182, "6mo"},
		{364, "12mo"},
		{365, "1y"},
		{548, "1.5y"},
		// The reason this exists at all: nobody reasons about "412d".
		{412, "1.1y"},
	}

	for _, test := range tests {
		if got := FormatInterval(test.days); got != test.want {
			t.Errorf("FormatInterval(%.1f) = %q, want %q", test.days, got, test.want)
		}
	}
}

// TestPreviewsMatchWhatNextDoes is the property that makes the button labels
// trustworthy: the preview is produced by the scheduler, so a button cannot
// advertise an interval the scheduler would not actually apply.
func TestPreviewsMatchWhatNextDoes(t *testing.T) {
	schedules := []Schedule{
		{},
		{IntervalDays: 1, AFactor: 2, Priority: 0.5},
		{IntervalDays: 8, AFactor: 2.4, Reps: 3, Priority: 0.3},
		{IntervalDays: 200, AFactor: 3, Reps: 9, Priority: 0.9},
	}

	for _, schedule := range schedules {
		previews := Previews(schedule, today)

		for _, grade := range []Grade{GradeNext, GradeSooner, GradeDefer} {
			want := FormatInterval(Next(schedule, grade, today).IntervalDays)
			if got := previews[grade].Interval; got != want {
				t.Errorf("schedule %+v grade %d: preview %q, scheduler would give %q",
					schedule, grade, got, want)
			}
		}

		// Sooner must never advertise a longer wait than Next, or the labels
		// contradict the words on them.
		if previews[GradeSooner].Interval == previews[GradeDefer].Interval &&
			schedule.IntervalDays > 2 {
			t.Errorf("schedule %+v: Sooner and Defer preview the same interval", schedule)
		}
	}
}

func TestPreviewsMarkTerminalGrades(t *testing.T) {
	previews := Previews(Schedule{IntervalDays: 5, AFactor: 2}, today)

	for _, grade := range []Grade{GradeDone, GradeDismiss, GradeSuspend} {
		if !previews[grade].Terminal {
			t.Errorf("grade %d is not marked terminal", grade)
		}
	}
	for _, grade := range []Grade{GradeNext, GradeSooner, GradeDefer, GradeBury} {
		if previews[grade].Terminal {
			t.Errorf("grade %d is wrongly marked terminal", grade)
		}
	}
	if previews[GradeBury].Interval != "today" {
		t.Errorf("bury previews %q, want \"today\"", previews[GradeBury].Interval)
	}
}

// TestBuryLeavesTheScheduleAlone: burying moves an element within a day, not
// between days, so nothing about its scheduling may change.
func TestBuryLeavesTheScheduleAlone(t *testing.T) {
	before := Schedule{
		State: StateReading, IntervalDays: 8, AFactor: 2.4, Reps: 3,
		Priority: 0.3, DueOn: today,
	}

	after := Next(before, GradeBury, today)

	if after != before {
		t.Errorf("bury changed the schedule:\n got %+v\nwant %+v", after, before)
	}
}

// TestFreshInterval pins the curve's endpoints and its shape: geometric, not
// linear, for the same reason priorityCap is — halfway down the scale should
// feel like a middling wait, not land near six months.
func TestFreshInterval(t *testing.T) {
	if got := FreshInterval(0); !closeEnough(got, 1) {
		t.Errorf("FreshInterval(0) = %.2f, want 1 (a day, the floor)", got)
	}
	if got := FreshInterval(1); !closeEnough(got, 365) {
		t.Errorf("FreshInterval(1) = %.2f, want 365 (a year, the ceiling)", got)
	}
	if mid := FreshInterval(0.5); mid < 15 || mid > 25 {
		t.Errorf("FreshInterval(0.5) = %.1f, want roughly 19 (sqrt(365)), not linear interpolation (~183)", mid)
	}
}

// TestEffectiveScheduleSubstitutesForFreshNonRootElements: an ungraded
// extract or highlight has no interval of its own yet, so without this
// substitution its priority has nothing to act on until it is graded once —
// see FreshInterval.
func TestEffectiveScheduleSubstitutesForFreshNonRootElements(t *testing.T) {
	fresh := Schedule{State: StateNew, Priority: 0.6, DueOn: today}

	effective := fresh.EffectiveSchedule(false)

	want := FreshInterval(0.6)
	if !closeEnough(effective.IntervalDays, want) {
		t.Errorf("interval = %.1f, want %.1f (FreshInterval(0.6))", effective.IntervalDays, want)
	}
}

// TestEffectiveScheduleLeavesRootArticlesAlone: a whole article is due on
// import's own schedule, not a priority curve — substituting FreshInterval
// for it would make touching priority at all silently postpone a
// due-today article, with nothing in the UI suggesting that would happen.
func TestEffectiveScheduleLeavesRootArticlesAlone(t *testing.T) {
	fresh := Schedule{State: StateNew, Priority: 0.5, DueOn: today}

	effective := fresh.EffectiveSchedule(true)

	if effective != fresh {
		t.Errorf("EffectiveSchedule(isRoot=true) changed a root article's schedule: got %+v, want %+v", effective, fresh)
	}
}

// TestEffectiveScheduleLeavesGradedElementsAlone: once something has a due
// date earned by actually reading it, that supersedes any priority-derived
// guess — same reasoning as Reprioritize's own State check.
func TestEffectiveScheduleLeavesGradedElementsAlone(t *testing.T) {
	reading := Schedule{State: StateReading, IntervalDays: 8, Priority: 0.6, DueOn: today}

	effective := reading.EffectiveSchedule(false)

	if effective != reading {
		t.Errorf("EffectiveSchedule changed a graded element's schedule: got %+v, want %+v", effective, reading)
	}
}

// TestReprioritizeBacklogsAFreshElement: a fresh highlight has no due date
// earned by reading, so dragging its priority toward "matters less" must move
// the date itself immediately rather than waiting for a grade that may never
// come for months.
func TestReprioritizeBacklogsAFreshElement(t *testing.T) {
	fresh := Schedule{State: StateNew, Priority: 0.5, DueOn: today, AFactor: 2.0}

	after := Reprioritize(fresh, 1.0, false, today)

	if after.Priority != 1.0 {
		t.Errorf("priority = %.2f, want 1.0", after.Priority)
	}
	wantDue := Day(today).AddDate(0, 0, 365)
	if !after.DueOn.Equal(wantDue) {
		t.Errorf("due = %v, want %v (a year out, FreshInterval(1.0))", after.DueOn, wantDue)
	}
	if !closeEnough(after.IntervalDays, 365) {
		t.Errorf("interval = %.1f, want ~365 so a later grade grows from this anchor, not from zero", after.IntervalDays)
	}
	if after.State != StateNew {
		t.Errorf("state = %q, want %q — reprioritizing is not grading it", after.State, StateNew)
	}
}

// TestReprioritizeTracksTheSliderInBothDirections: priority always shapes an
// ungraded element's schedule, not just the first time it is dragged toward
// "matters less" — moving it back toward "matters more" afterwards must pull
// the due date back in too, not leave it pinned to the furthest point ever
// reached. This is also what keeps Sooner distinguishable from Next and
// Defer: pinned to a stale, oversized anchor, halving it (Sooner) can still
// exceed the cap and collapse into the same clamped value as the other two.
func TestReprioritizeTracksTheSliderInBothDirections(t *testing.T) {
	fresh := Schedule{State: StateNew, Priority: 0.5, DueOn: today, AFactor: 2.0}

	backlogged := Reprioritize(fresh, 1.0, false, today)
	settled := Reprioritize(backlogged, 0.8, false, today)

	wantInterval := FreshInterval(0.8)
	if !closeEnough(settled.IntervalDays, wantInterval) {
		t.Errorf("interval = %.1f, want ~%.1f (FreshInterval(0.8), not still anchored to FreshInterval(1.0))",
			settled.IntervalDays, wantInterval)
	}
	wantDue := Day(today).AddDate(0, 0, int(math.Round(wantInterval)))
	if !settled.DueOn.Equal(wantDue) {
		t.Errorf("due = %v, want %v", settled.DueOn, wantDue)
	}

	// With the anchor no longer oversized, the three grade previews must not
	// all collapse into the same clamped ceiling.
	previews := Previews(settled, today)
	if previews[GradeSooner].Interval == previews[GradeNext].Interval {
		t.Errorf("Sooner and Next both preview %q; the anchor is still too large for Sooner to differ",
			previews[GradeNext].Interval)
	}
}

// TestReprioritizeCanBacklogAWholeArticle: "I'll read this in a week" has to
// apply to a root the same as an extract — dragging priority toward less
// important pushes a fresh article's due date out too.
func TestReprioritizeCanBacklogAWholeArticle(t *testing.T) {
	article := Schedule{State: StateNew, Priority: 0.5, DueOn: today}

	after := Reprioritize(article, 1.0, true, today)

	if after.Priority != 1.0 {
		t.Errorf("priority = %.2f, want 1.0", after.Priority)
	}
	wantDue := Day(today).AddDate(0, 0, 365)
	if !after.DueOn.Equal(wantDue) {
		t.Errorf("due = %v, want %v (a year out, FreshInterval(1.0))", after.DueOn, wantDue)
	}
}

// TestReprioritizeNeverBuriesAnUntouchedArticle: an article is due
// immediately by default. The very first time priority moves on it, raising
// importance (the only direction that matters while it still sits at that
// default) must leave the due date alone — without this, nudging priority at
// all would jump a due-today article out to FreshInterval's floor, which is
// always later than "today" and the opposite of what raising priority means.
func TestReprioritizeNeverBuriesAnUntouchedArticle(t *testing.T) {
	article := Schedule{State: StateNew, Priority: 0.6, DueOn: today}

	after := Reprioritize(article, 0.1, true, today)

	if after.Priority != 0.1 {
		t.Errorf("priority = %.2f, want 0.1", after.Priority)
	}
	if !after.DueOn.Equal(today) || after.IntervalDays != article.IntervalDays {
		t.Errorf("raising priority on an untouched article changed its schedule: %+v", after)
	}
}

// TestReprioritizeTracksAnAlreadyBacklogedArticleFreely: once an article has
// been backlogged at all, its interval is no longer import's default, so
// further slider moves — including back toward more important — track it
// freely, the same as an extract does once anchored. Leaving it pinned to
// the furthest point ever dragged to would be the exact bug
// TestReprioritizeTracksTheSliderInBothDirections guards for extracts.
func TestReprioritizeTracksAnAlreadyBacklogedArticleFreely(t *testing.T) {
	article := Schedule{State: StateNew, Priority: 0.5, DueOn: today}

	backlogged := Reprioritize(article, 1.0, true, today)
	settled := Reprioritize(backlogged, 0.2, true, today)

	wantInterval := FreshInterval(0.2)
	if !closeEnough(settled.IntervalDays, wantInterval) {
		t.Errorf("interval = %.1f, want ~%.1f (FreshInterval(0.2), not still anchored to FreshInterval(1.0))",
			settled.IntervalDays, wantInterval)
	}
	wantDue := Day(today).AddDate(0, 0, int(math.Round(wantInterval)))
	if !settled.DueOn.Equal(wantDue) {
		t.Errorf("due = %v, want %v", settled.DueOn, wantDue)
	}
}

// TestReprioritizeOnlyPushesAnArticleOutPastItsCurrentDueDate: on an
// untouched article's first backlog, a small move toward less important must
// not pull the due date in if it is already scheduled further out than that —
// same reasoning as the guard against pulling a due date in at all, just for
// the case where there already is a later one to preserve.
func TestReprioritizeOnlyPushesAnArticleOutPastItsCurrentDueDate(t *testing.T) {
	farOut := Day(today).AddDate(0, 0, 300)
	article := Schedule{State: StateNew, Priority: 0.5, DueOn: farOut}

	// FreshInterval(0.6) is roughly 34 days — far short of the 300 days this
	// article is already scheduled out to.
	after := Reprioritize(article, 0.6, true, today)

	if !after.DueOn.Equal(farOut) {
		t.Errorf("due = %v, want unchanged at %v (already further out than the new interval)", after.DueOn, farOut)
	}
}

// TestReprioritizeLeavesGradedElementsAlone: an element already in circulation
// has a due date earned by actually reading it. Reprioritize must behave like
// a plain priority write there — see the min() clamp in Next for how priority
// still bounds it, just not by rewriting history on the spot.
func TestReprioritizeLeavesGradedElementsAlone(t *testing.T) {
	reading := Schedule{
		State: StateReading, IntervalDays: 8, AFactor: 2.4, Reps: 3,
		Priority: 0.3, DueOn: today,
	}

	after := Reprioritize(reading, 0.9, false, today)

	if after.Priority != 0.9 {
		t.Errorf("priority = %.2f, want 0.9", after.Priority)
	}
	if !after.DueOn.Equal(today) || after.IntervalDays != 8 || after.Reps != 3 {
		t.Errorf("reprioritizing a graded element changed its schedule: %+v", after)
	}
}
