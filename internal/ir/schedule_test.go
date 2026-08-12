package ir

import (
	"math"
	"testing"
	"time"
)

var today = time.Date(2026, 7, 31, 14, 30, 0, 0, time.UTC)

// fuzzSpreadFor mirrors fuzzedIntervalOffset's own spread calculation, so
// tests can assert "within the fuzz's own bound" instead of duplicating the
// seed arithmetic and asserting an exact number the fuzz is designed to move.
func fuzzSpreadFor(scale float64) float64 {
	return float64(max(minIntervalFuzzSpread, int(math.Round(scale))/intervalFuzzDivisor))
}

func TestIntervalsGrowByAFactor(t *testing.T) {
	// A fresh topic at the lowest priority, so nothing is capped and the raw
	// A-Factor progression is visible.
	schedule := Schedule{Priority: 1.0}
	elementID := int64(1)

	for repetition := 1; repetition <= 4; repetition++ {
		prevInterval := schedule.IntervalDays
		wantRaw := grow(prevInterval, defaultAFactor)

		schedule = Next(schedule, GradeNext, today, elementID)

		if spread := fuzzSpreadFor(wantRaw); math.Abs(schedule.IntervalDays-wantRaw) > spread {
			t.Errorf("repetition %d: interval = %.2f, want within %.1f of %.2f",
				repetition, schedule.IntervalDays, spread, wantRaw)
		}
		if schedule.IntervalDays <= prevInterval {
			t.Errorf("repetition %d: interval did not grow: %.2f -> %.2f",
				repetition, prevInterval, schedule.IntervalDays)
		}
		if schedule.Reps != repetition {
			t.Errorf("repetition %d: reps = %d", repetition, schedule.Reps)
		}
		if schedule.State != StateReading {
			t.Errorf("repetition %d: state = %q, want %q", repetition, schedule.State, StateReading)
		}
	}
}

// TestFirstRepetitionIsAMonth: with no history to grow from, Next needs a
// baseline distinct from Sooner's flat one-day floor, or the two read as the
// same decision on a topic's very first grade — see firstInterval. Fuzz
// moves it off the exact figure, so this checks the fuzz's own bound rather
// than the bare constant.
func TestFirstRepetitionIsAMonth(t *testing.T) {
	elementID := int64(1)
	schedule := Next(Schedule{Priority: 1.0}, GradeNext, today, elementID)

	if spread := fuzzSpreadFor(firstInterval); math.Abs(schedule.IntervalDays-firstInterval) > spread {
		t.Errorf("interval = %.2f, want within %.1f of %.0f", schedule.IntervalDays, spread, firstInterval)
	}
	want := Day(today).AddDate(0, 0, int(math.Round(schedule.IntervalDays)))
	if !schedule.DueOn.Equal(want) {
		t.Errorf("due = %v, want %v", schedule.DueOn, want)
	}
}

// TestSoonerFuzzesFromAFreshTopic guards the split TestFirstRepetitionIsAMonth
// depends on: Sooner never grows past a handful of days just because there is
// no history yet to halve. Unlike the backlog's "1d" preset, a fresh topic's
// flat one-day floor is not a preset a reader chose — see
// minIntervalFuzzSpread — so it still fuzzes, spread off Next's own growth
// branch (firstInterval) rather than off Sooner's own tiny raw value.
func TestSoonerFuzzesFromAFreshTopic(t *testing.T) {
	elementID := int64(1)
	schedule := Next(Schedule{Priority: 1.0}, GradeSooner, today, elementID)

	if schedule.IntervalDays < minInterval {
		t.Errorf("interval = %.2f, want at least %.1f", schedule.IntervalDays, minInterval)
	}
	if max := 1 + fuzzSpreadFor(firstInterval); schedule.IntervalDays > max {
		t.Errorf("interval = %.2f, want at most %.1f", schedule.IntervalDays, max)
	}
}

func TestSoonerShortensAndSlowsGrowth(t *testing.T) {
	schedule := Schedule{IntervalDays: 8, AFactor: 2.0, Priority: 1.0}
	elementID := int64(1)

	got := Next(schedule, GradeSooner, today, elementID)

	rawHalf := schedule.IntervalDays / 2
	growthScale := grow(schedule.IntervalDays, schedule.AFactor)
	if spread := fuzzSpreadFor(growthScale); math.Abs(got.IntervalDays-rawHalf) > spread {
		t.Errorf("interval = %.3f, want within %.1f of %.1f (half of 8)", got.IntervalDays, spread, rawHalf)
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
	// Repeated Sooner must not collapse to no growth at all.
	schedule := Schedule{IntervalDays: 100, AFactor: minAFactor, Priority: 1.0}
	for i := 0; i < 20; i++ {
		schedule = Next(schedule, GradeSooner, today, 1)
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
				schedule = Next(schedule, GradeNext, today, 1)
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
		high = Next(high, GradeNext, today, 1)
		low = Next(low, GradeNext, today, 1)

		if high.IntervalDays > low.IntervalDays {
			t.Fatalf("repetition %d: high-priority interval %.1f exceeds low-priority %.1f",
				repetition, high.IntervalDays, low.IntervalDays)
		}
	}
}

func TestTerminalGrades(t *testing.T) {
	schedule := Schedule{IntervalDays: 8, AFactor: 2, Reps: 3}

	done := Next(schedule, GradeDone, today, 1)
	if done.State != StateDone {
		t.Errorf("state = %q, want %q", done.State, StateDone)
	}
	if !done.DueOn.IsZero() {
		t.Errorf("finished material has a due date: %v", done.DueOn)
	}
	if done.Due(today) {
		t.Error("finished material is still reported as due")
	}

	dismissed := Next(schedule, GradeDismiss, today, 1)
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
	got := Next(Schedule{}, GradeNext, today, 1)

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

	got := Next(schedule, GradeSuspend, today, 1)

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
	resumed := Next(got, GradeNext, today, 1)
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
//
// It also covers the invariant fuzzing must not break: Sooner and Next are
// fuzzed with the same offset (see fuzzedIntervalOffset) precisely so that,
// whatever elementID the jitter is seeded with, Sooner can never advertise a
// longer wait than Next on the same schedule.
func TestPreviewsMatchWhatNextDoes(t *testing.T) {
	schedules := []Schedule{
		{},
		{IntervalDays: 1, AFactor: 2, Priority: 0.5},
		{IntervalDays: 8, AFactor: 2.4, Reps: 3, Priority: 0.3},
		{IntervalDays: 200, AFactor: 3, Reps: 9, Priority: 0.9},
	}

	for _, schedule := range schedules {
		for elementID := int64(1); elementID <= 30; elementID++ {
			previews := Previews(schedule, today, elementID)

			for _, grade := range []Grade{GradeNext, GradeSooner} {
				want := FormatInterval(Next(schedule, grade, today, elementID).IntervalDays)
				if got := previews[grade].Interval; got != want {
					t.Errorf("schedule %+v element %d grade %d: preview %q, scheduler would give %q",
						schedule, elementID, grade, got, want)
				}
			}

			// Sooner must never advertise a longer wait than Next, or the
			// labels contradict the words on them.
			sooner := Next(schedule, GradeSooner, today, elementID).IntervalDays
			next := Next(schedule, GradeNext, today, elementID).IntervalDays
			if sooner > next {
				t.Errorf("schedule %+v element %d: Sooner (%.1f) exceeds Next (%.1f)",
					schedule, elementID, sooner, next)
			}
		}
	}
}

func TestPreviewsMarkTerminalGrades(t *testing.T) {
	previews := Previews(Schedule{IntervalDays: 5, AFactor: 2}, today, 1)

	for _, grade := range []Grade{GradeDone, GradeDismiss, GradeSuspend} {
		if !previews[grade].Terminal {
			t.Errorf("grade %d is not marked terminal", grade)
		}
	}
	for _, grade := range []Grade{GradeNext, GradeSooner, GradeBury} {
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

	after := Next(before, GradeBury, today, 1)

	if after != before {
		t.Errorf("bury changed the schedule:\n got %+v\nwant %+v", after, before)
	}
}

// TestIntervalFuzzStaysWithinSpread mirrors TestFuzzedBacklogDaysStaysWithinSpread
// for the scheduler's own growth: whatever the offset does, it must stay
// inside the spread fuzzedIntervalOffset itself defines.
func TestIntervalFuzzStaysWithinSpread(t *testing.T) {
	scales := []float64{1, 7, 30, 60, 200, 900}

	for _, scale := range scales {
		spread := fuzzSpreadFor(scale)
		for elementID := int64(1); elementID <= 200; elementID++ {
			for reps := 0; reps < 5; reps++ {
				got := fuzzedIntervalOffset(elementID, reps, scale)
				if float64(got) < -spread || float64(got) > spread {
					t.Fatalf("scale %.0f, element %d, reps %d: offset = %d, want in [-%.0f, %.0f]",
						scale, elementID, reps, got, spread, spread)
				}
			}
		}
	}
}

// TestIntervalFuzzSpreadsAcrossElements is the whole point: if every element
// graded "Next" from a fresh start landed on the same due date, the pile-up
// the fuzz exists to prevent would happen anyway, just on the grading path
// instead of the backlog buttons — see TestFuzzedBacklogDaysSpreadsAcrossElements.
func TestIntervalFuzzSpreadsAcrossElements(t *testing.T) {
	seen := make(map[float64]bool)
	for elementID := int64(1); elementID <= 50; elementID++ {
		graded := Next(Schedule{Priority: 1.0}, GradeNext, today, elementID)
		seen[graded.IntervalDays] = true
	}
	if len(seen) < 5 {
		t.Errorf("50 elements grading Next from fresh produced only %d distinct intervals, want a real spread", len(seen))
	}
}

// TestSoonerFuzzSpreadsAcrossElements is TestIntervalFuzzSpreadsAcrossElements
// for the left-hand button: fifty readers all hitting Sooner on a fresh topic
// on the same day must not all land on the exact same tomorrow.
func TestSoonerFuzzSpreadsAcrossElements(t *testing.T) {
	seen := make(map[float64]bool)
	for elementID := int64(1); elementID <= 50; elementID++ {
		graded := Next(Schedule{Priority: 1.0}, GradeSooner, today, elementID)
		seen[graded.IntervalDays] = true
	}
	if len(seen) < 3 {
		t.Errorf("50 elements grading Sooner from fresh produced only %d distinct intervals, want a real spread", len(seen))
	}
}

// TestIntervalFuzzIsDeterministic: grading the same element from the same
// schedule must always compute the same interval, or the button's own
// preview (see TestPreviewsMatchWhatNextDoes) could not promise anything.
func TestIntervalFuzzIsDeterministic(t *testing.T) {
	schedule := Schedule{IntervalDays: 8, AFactor: 2.4, Reps: 3, Priority: 0.3}
	first := Next(schedule, GradeNext, today, 42)
	for i := 0; i < 5; i++ {
		if got := Next(schedule, GradeNext, today, 42); got.IntervalDays != first.IntervalDays {
			t.Fatalf("call %d: got %.2f, want %.2f (same every time)", i, got.IntervalDays, first.IntervalDays)
		}
	}
}

// TestFuzzedBacklogDaysStaysWithinSpread guards the shape of the jitter: it
// must move the preset, or the fuzz is pointless, but never past the eighth
// on either side that defines it.
func TestFuzzedBacklogDaysStaysWithinSpread(t *testing.T) {
	for _, preset := range BacklogPresets {
		spread := 0
		if preset.Days > 1 {
			spread = max(1, preset.Days/backlogFuzzDivisor)
		}
		low, high := preset.Days-spread, preset.Days+spread

		// A spread of many elements, not just one, since the property being
		// checked is about the whole distribution, not a single sample.
		for elementID := int64(1); elementID <= 200; elementID++ {
			got := FuzzedBacklogDays(elementID, preset)
			if got < low || got > high {
				t.Fatalf("preset %s, element %d: fuzzed = %d, want in [%d, %d]",
					preset.Label, elementID, got, low, high)
			}
		}
	}
}

// TestFuzzedBacklogDaysSpreadsAcrossElements is the whole point of fuzzing:
// if everyone who clicks "1mo" today lands on the exact same due date, the
// pile-up the fuzz exists to prevent happens anyway, just one step removed —
// see FuzzedAnnotationDelay for the same problem on the import side.
func TestFuzzedBacklogDaysSpreadsAcrossElements(t *testing.T) {
	preset := BacklogPreset{Days: 30, Label: "1mo"}
	seen := make(map[int]bool)
	for elementID := int64(1); elementID <= 50; elementID++ {
		seen[FuzzedBacklogDays(elementID, preset)] = true
	}
	if len(seen) < 5 {
		t.Errorf("50 elements picking the same preset produced only %d distinct due dates, want a real spread", len(seen))
	}
}

// TestFuzzedBacklogDaysIsDeterministic: the label a button shows and the date
// clicking it actually sets must always be the same number, or the button
// promised something it did not do.
func TestFuzzedBacklogDaysIsDeterministic(t *testing.T) {
	preset := BacklogPreset{Days: 180, Label: "6mo"}
	first := FuzzedBacklogDays(42, preset)
	for i := 0; i < 5; i++ {
		if got := FuzzedBacklogDays(42, preset); got != first {
			t.Fatalf("call %d: got %d, want %d (same every time)", i, got, first)
		}
	}
}

// TestFuzzedBacklogDaysNeverZeroesOutTheJitter: it would be tempting to skip
// fuzzing when a preset is already small, but a day either way is exactly
// what stops several extracts snoozed to "7d" on the same afternoon from all
// landing on the same future date — the case fuzzing exists for in the first
// place.
func TestFuzzedBacklogDaysNeverZeroesOutTheJitter(t *testing.T) {
	preset := BacklogPreset{Days: 7, Label: "7d"}
	seen := make(map[int]bool)
	for elementID := int64(1); elementID <= 30; elementID++ {
		seen[FuzzedBacklogDays(elementID, preset)] = true
	}
	if len(seen) < 2 {
		t.Error("the 7d preset never varies across elements — the fuzz was neglected at the low end")
	}
}

// TestBacklogOptionsLabelMatchesAppliedDays: the label rendered on a button
// must describe the exact number of days Backlog would apply if that button
// were clicked — never the preset's own round number once fuzz has moved it.
func TestBacklogOptionsLabelMatchesAppliedDays(t *testing.T) {
	for _, option := range BacklogOptions(7) {
		want := FormatInterval(float64(option.Days))
		if option.Label != want {
			t.Errorf("days=%d label=%q, want %q", option.Days, option.Label, want)
		}
	}
}

// TestBacklogSetsIntervalAndDueDate: the interval and due date move to
// exactly what was asked for, starting from today — and nothing about the
// SM-2 state (state, reps, A-Factor) is touched, because putting something
// off is not a grade and must not be recorded as one.
func TestBacklogSetsIntervalAndDueDate(t *testing.T) {
	schedule := Schedule{State: StateReading, IntervalDays: 8, AFactor: 2.4, Reps: 3, Priority: 0.3}

	after := Backlog(schedule, 30, today)

	if after.IntervalDays != 30 {
		t.Errorf("interval = %.1f, want 30", after.IntervalDays)
	}
	wantDue := Day(today).AddDate(0, 0, 30)
	if !after.DueOn.Equal(wantDue) {
		t.Errorf("due = %v, want %v", after.DueOn, wantDue)
	}
	if after.State != schedule.State || after.Reps != schedule.Reps || after.AFactor != schedule.AFactor {
		t.Errorf("Backlog changed grading state: got %+v, want state/reps/afactor unchanged from %+v", after, schedule)
	}
}

// TestBacklogZeroDaysIsToday: 0 is the "today" preset, for undoing an
// earlier backlog — it must land exactly on today, not tomorrow or some
// rounding artifact, since it is meant to put the element back in reach
// immediately.
func TestBacklogZeroDaysIsToday(t *testing.T) {
	schedule := Schedule{State: StateReading, IntervalDays: 30, AFactor: 2.4, Reps: 3, Priority: 0.3}

	after := Backlog(schedule, 0, today)

	if after.IntervalDays != 0 {
		t.Errorf("interval = %.1f, want 0", after.IntervalDays)
	}
	if !after.DueOn.Equal(Day(today)) {
		t.Errorf("due = %v, want today (%v)", after.DueOn, Day(today))
	}
}

// TestBacklogAppliesToFreshAndGradedElementsAlike: unlike the slider it
// replaced, a backlog button has no default resting position that could be
// mistaken for indifference, so it needs no special case for an element that
// has never been graded — it behaves exactly the same either way.
func TestBacklogAppliesToFreshAndGradedElementsAlike(t *testing.T) {
	fresh := Schedule{State: StateNew, Priority: 0.5, DueOn: today}
	after := Backlog(fresh, 7, today)

	if after.State != StateNew {
		t.Errorf("state = %q, want %q — backlogging is not grading it", after.State, StateNew)
	}
	wantDue := Day(today).AddDate(0, 0, 7)
	if !after.DueOn.Equal(wantDue) {
		t.Errorf("due = %v, want %v", after.DueOn, wantDue)
	}
}

// TestFuzzedFirstDueDaysStaysWithinSpread mirrors
// TestFuzzedBacklogDaysStaysWithinSpread: the jitter must move the delay, but
// never past the eighth on either side that defines it.
func TestFuzzedFirstDueDaysStaysWithinSpread(t *testing.T) {
	for _, delayDays := range []int{2, 7, 10, 30} {
		spread := max(1, delayDays/firstDueFuzzDivisor)
		low, high := delayDays-spread, delayDays+spread

		for seed := int64(1); seed <= 200; seed++ {
			got := FuzzedFirstDueDays(seed, delayDays)
			if got < low || got > high {
				t.Fatalf("delay %d, seed %d: fuzzed = %d, want in [%d, %d]",
					delayDays, seed, got, low, high)
			}
		}
	}
}

// TestFuzzedFirstDueDaysSpreadsAcrossSeeds is the whole point: several
// extracts pulled from one article in a sitting must not all come back on the
// exact same future date.
func TestFuzzedFirstDueDaysSpreadsAcrossSeeds(t *testing.T) {
	seen := make(map[int]bool)
	for seed := int64(1); seed <= 50; seed++ {
		seen[FuzzedFirstDueDays(seed, 10)] = true
	}
	if len(seen) < 3 {
		t.Errorf("50 seeds at delay=10 produced only %d distinct due dates, want a real spread", len(seen))
	}
}

// TestFuzzedFirstDueDaysIsDeterministic: the same seed and delay must always
// produce the same offset, since store.CreateExtract computes it once, before
// the row (and its own id) exists, with nothing to reconcile it against later.
func TestFuzzedFirstDueDaysIsDeterministic(t *testing.T) {
	first := FuzzedFirstDueDays(12345, 10)
	for i := 0; i < 5; i++ {
		if got := FuzzedFirstDueDays(12345, 10); got != first {
			t.Fatalf("call %d: got %d, want %d (same every time)", i, got, first)
		}
	}
}

// TestFuzzedFirstDueDaysLeavesShortDelaysAlone: a configured delay of zero or
// one day has no equivalent of a preset chosen on purpose, but is small
// enough that fuzzing it would either do nothing or read as broken — see
// FuzzedFirstDueDays's own doc comment.
func TestFuzzedFirstDueDaysLeavesShortDelaysAlone(t *testing.T) {
	for _, delayDays := range []int{0, 1} {
		for seed := int64(1); seed <= 20; seed++ {
			if got := FuzzedFirstDueDays(seed, delayDays); got != delayDays {
				t.Errorf("delay %d, seed %d: got %d, want it unchanged", delayDays, seed, got)
			}
		}
	}
}

// TestFuzzedAnnotationDelayRespectsTheFloor is the property the whole
// function exists to guarantee: nothing lands before floorDays, no matter
// what the seed is.
func TestFuzzedAnnotationDelayRespectsTheFloor(t *testing.T) {
	const floor, spread = 30, 60
	for seed := int64(-100); seed <= 100; seed++ {
		got := FuzzedAnnotationDelay(seed, floor, spread)
		if got < floor || got >= floor+spread {
			t.Fatalf("seed %d: fuzzed = %d, want in [%d, %d)", seed, got, floor, floor+spread)
		}
	}
}

// TestFuzzedAnnotationDelaySpreadsAcrossSeeds is the point of the whole
// design: a batch of highlights from one document, one seed per highlight,
// must land across many different days rather than piling onto one — the bug
// this replaces (a spread seeded on the document instead of the highlight,
// so the seed never varied within one import) is exactly what this guards.
func TestFuzzedAnnotationDelaySpreadsAcrossSeeds(t *testing.T) {
	seen := make(map[int]bool)
	for seed := int64(1); seed <= 200; seed++ {
		seen[FuzzedAnnotationDelay(seed, 30, 60)] = true
	}
	if len(seen) < 20 {
		t.Errorf("200 seeds produced only %d distinct due dates across a 60-day window, want a wide spread", len(seen))
	}
}

// TestFuzzedAnnotationDelayNoSpreadIsExactlyTheFloor: spreadDays of zero is
// how a caller asks for no randomisation at all, not a window of width zero
// that happens to divide by zero.
func TestFuzzedAnnotationDelayNoSpreadIsExactlyTheFloor(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		if got := FuzzedAnnotationDelay(seed, 30, 0); got != 30 {
			t.Errorf("seed %d: got %d, want exactly the floor (30) with no spread", seed, got)
		}
	}
}

// TestFuzzedAnnotationDelayIsDeterministic: store.insertHighlights computes
// this once per highlight, before that row (and its id) exists. Re-syncing or
// re-importing the same file must recompute the identical offset, or an
// annotation's due date would reshuffle every time its source was revisited.
func TestFuzzedAnnotationDelayIsDeterministic(t *testing.T) {
	first := FuzzedAnnotationDelay(98765, 30, 60)
	for i := 0; i < 5; i++ {
		if got := FuzzedAnnotationDelay(98765, 30, 60); got != first {
			t.Fatalf("call %d: got %d, want %d (same every time)", i, got, first)
		}
	}
}
