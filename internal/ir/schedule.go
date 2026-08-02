package ir

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// State is where an element sits in its lifecycle.
type State string

const (
	// StateNew is an element that has never been read.
	StateNew State = "new"
	// StateReading is an element in circulation.
	StateReading State = "reading"
	// StateDone is finished material, kept for its extracts but never shown again.
	StateDone State = "done"
	// StateDismissed is material abandoned unread.
	StateDismissed State = "dismissed"
	// StateSuspended is material parked indefinitely.
	//
	// Unlike Done and Dismissed it is not terminal: the interval, A-Factor and
	// repetition count are all preserved, so unsuspending resumes rather than
	// restarts. It is also what an already-archived article gets on import.
	StateSuspended State = "suspended"
)

// Grade is what the reader decides to do with a topic.
//
// This is the point where incremental reading departs from ordinary spaced
// repetition. A flashcard grade reports how well you recalled something, and
// the scheduler infers an interval from that. A topic grade is an *intention* —
// how soon you want this material back — because there is nothing to recall
// while reading; you are deciding what deserves your attention next.
type Grade int

const (
	// GradeNext: read a slice, extracted what mattered, stop here for now.
	// The read point is kept and the interval grows normally. This is the
	// everyday action — incremental reading is mostly the act of putting
	// something down on purpose. Putting off material that isn't worth this
	// is Backlog's job now, not a grade — see the schedule-buttons template.
	GradeNext Grade = iota
	// GradeSooner: more interesting than expected; bring it back and let it drift more slowly.
	GradeSooner
	// GradeDone: finished with this material.
	GradeDone
	// GradeDismiss: abandoned; not worth finishing.
	GradeDismiss
	// GradeSuspend: parked indefinitely, resumable. Keeps the interval and
	// repetition count, unlike the two terminal grades above.
	GradeSuspend
	// GradeBury: not this one right now, but before the session ends.
	//
	// Alone among the grades it does not touch the schedule at all — it moves
	// an element within today rather than out of it, so Next leaves it
	// unchanged and the store handles it by recording the date. Deliberately
	// not Anki's bury, which hides until tomorrow: the case being served is
	// "come back to it before I stop reading", which hiding until tomorrow
	// does not serve.
	GradeBury
)

// Scheduling constants.
const (
	// firstInterval is how long a topic waits after its first pass.
	firstInterval = 1.0

	// defaultAFactor is the initial interval multiplier. Each repetition
	// multiplies the interval by the A-Factor, so 2.0 means intervals double:
	// 1, 2, 4, 8 days and so on.
	defaultAFactor = 2.0

	// The A-Factor is clamped. Below ~1.2 a topic barely moves and clogs the
	// queue; above ~6 one pass throws it out of sight for a year.
	minAFactor = 1.2
	maxAFactor = 6.0

	// aFactorStep is how sharply Later and Sooner bend the A-Factor. It
	// compounds, so repeatedly postponing something makes it recede faster and
	// faster — which is how uninteresting material drains out of the queue
	// without ever being explicitly abandoned.
	aFactorStep = 1.2

	// Interval bounds by priority. The highest-priority material is never let
	// out of sight for more than a week; the lowest can wait a year.
	minPriorityCap = 7.0
	maxPriorityCap = 365.0

	minInterval = 1.0
)

// Schedule is an element's scheduling state.
type Schedule struct {
	State        State
	IntervalDays float64
	AFactor      float64
	Reps         int
	DueOn        time.Time

	// Priority runs from 0.0 (most important) to 1.0 (least), following
	// SuperMemo's convention that priority is a percentile position in the
	// queue rather than a rank.
	Priority float64
}

// Next applies a grade and returns the updated schedule.
//
// It is a pure function: same inputs, same output, no clock and no storage.
// today is supplied by the caller, which is what makes the day-boundary
// behaviour testable and keeps the timezone decision in one place.
func Next(schedule Schedule, grade Grade, today time.Time) Schedule {
	next := schedule
	next.AFactor = clamp(orDefault(schedule.AFactor, defaultAFactor), minAFactor, maxAFactor)
	next.Priority = clamp(schedule.Priority, 0, 1)

	switch grade {
	case GradeDone:
		next.State = StateDone
		next.DueOn = time.Time{}
		return next

	case GradeDismiss:
		next.State = StateDismissed
		next.DueOn = time.Time{}
		return next

	case GradeSuspend:
		// Interval, A-Factor and repetition count are left untouched: this is
		// a pause, and resuming should carry on rather than start over.
		next.State = StateSuspended
		next.DueOn = time.Time{}
		return next

	case GradeBury:
		// Position within today, not a change of schedule.
		return schedule

	case GradeSooner:
		next.AFactor = clamp(next.AFactor/aFactorStep, minAFactor, maxAFactor)
		// Come back in half the time just waited, rather than growing at all.
		// Sooner is a request to see this again, so growth would contradict it.
		next.IntervalDays = math.Max(minInterval, schedule.IntervalDays/2)

	default: // GradeNext
		next.IntervalDays = grow(schedule.IntervalDays, next.AFactor)
	}

	next.IntervalDays = math.Min(next.IntervalDays, priorityCap(next.Priority))
	next.Reps = schedule.Reps + 1
	next.State = StateReading
	next.DueOn = Day(today).AddDate(0, 0, int(math.Round(next.IntervalDays)))
	return next
}

// BacklogPreset is one fixed duration offered for putting an element off.
type BacklogPreset struct {
	Days  int
	Label string
}

// BacklogPresets are the choices on the schedule panel, from "barely wait"
// to "basically shelved" — a fixed menu rather than a continuous slider.
// A slider's resting position looked identical to a deliberate choice, so a
// stray drag near the import default silently changed nothing, and a drag
// away from it jumped by whatever a priority curve implied at that pixel
// rather than a duration anyone actually asked for.
var BacklogPresets = []BacklogPreset{
	{Days: 1, Label: "1d"},
	{Days: 7, Label: "7d"},
	{Days: 30, Label: "1mo"},
	{Days: 60, Label: "2mo"},
	{Days: 180, Label: "6mo"},
	{Days: 365, Label: "1y"},
	{Days: 730, Label: "2y"},
}

// backlogFuzzDivisor sets the jitter's width relative to the preset it is
// applied to: about an eighth of it, each direction.
const backlogFuzzDivisor = 8

// FuzzedBacklogDays nudges a preset by a deterministic, element-specific
// amount, so that everyone who picks the same preset on the same day does
// not land on the exact same due date. Piling a self-inflicted backlog onto
// one future day is the same failure spreading a fresh import already
// exists to avoid — see spreadOffset — just chosen by the reader instead of
// created by an import.
//
// The jitter is proportional to the preset rather than a fixed number of
// days: a day either way matters at the "1d" end and is invisible at the
// "2y" end, so a fixed spread would be pointless at one end or absurd at the
// other. It is floored at one day for every preset above the very shortest,
// rather than truncating to zero — integer division alone would silently
// turn it off for "7d" (7/8 rounds to 0), which is exactly the pile-up this
// exists to prevent, and neglecting it there for the sake of a round number
// would defeat the whole point.
//
// Deterministic on (elementID, preset) so the label a button shows and the
// date clicking it actually sets always agree — see BacklogOptions.
func FuzzedBacklogDays(elementID int64, preset BacklogPreset) int {
	if preset.Days <= 1 {
		return preset.Days
	}
	spread := max(1, preset.Days/backlogFuzzDivisor)
	width := int64(2*spread + 1)
	// The second term varies the seed by preset, not just by element, so
	// different presets on the same element don't all drift the same way.
	seed := elementID*2654435761 + int64(preset.Days)*40503
	offset := int(((seed % width) + width) % width)
	return preset.Days + offset - spread
}

// BacklogOption is one preset resolved for a specific element: the day count
// its button will actually apply, fuzz included, and a label rendering it.
type BacklogOption struct {
	Days  int
	Label string
}

// BacklogOptions resolves every preset for one element, fuzz applied, for
// rendering the schedule panel. The label is the exact duration clicking the
// button will use, never the preset's own round number — the same "preview
// is the behaviour" rule Previews follows for grading.
func BacklogOptions(elementID int64) []BacklogOption {
	options := make([]BacklogOption, len(BacklogPresets))
	for i, preset := range BacklogPresets {
		days := FuzzedBacklogDays(elementID, preset)
		options[i] = BacklogOption{Days: days, Label: FormatInterval(float64(days))}
	}
	return options
}

// Backlog puts an element off by the given number of days, starting today —
// the explicit counterpart to grading it, for something not worth reading
// right now but not worth abandoning either. Interval and due date are the
// only things it touches; state, A-Factor and repetition count are left
// alone, so this is not a grade and does not pretend to be one.
//
// Applies the same way to a root article and an extract. Unlike a slider, a
// button click has no resting position that could be mistaken for
// indifference, so there is no default state left to protect the way an
// earlier, priority-driven version of this had to.
func Backlog(schedule Schedule, days int, today time.Time) Schedule {
	next := schedule
	next.IntervalDays = float64(days)
	next.DueOn = Day(today).AddDate(0, 0, days)
	return next
}

// grow advances an interval by one repetition.
func grow(interval, afactor float64) float64 {
	if interval < minInterval {
		return firstInterval
	}
	return interval * afactor
}

// priorityCap is the longest interval allowed at a given priority.
//
// The curve is geometric rather than linear, because what matters is the ratio
// between "soon" and "eventually", not the arithmetic difference: halfway down
// the priority scale should feel like a middling wait, and linear interpolation
// would instead put it near six months.
func priorityCap(priority float64) float64 {
	return minPriorityCap * math.Pow(maxPriorityCap/minPriorityCap, clamp(priority, 0, 1))
}

// Day truncates a time to midnight in its own location.
//
// Scheduling works in whole days, so every due date passes through here. Doing
// it with the time's own location, rather than converting to UTC, is what makes
// "today" mean the reader's today.
func Day(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, t.Location())
}

// Due reports whether an element should appear in the queue on the given day.
func (s Schedule) Due(today time.Time) bool {
	if s.State == StateDone || s.State == StateDismissed || s.State == StateSuspended {
		return false
	}
	if s.DueOn.IsZero() {
		return true
	}
	return !Day(s.DueOn).After(Day(today))
}

func clamp(value, low, high float64) float64 {
	return math.Min(math.Max(value, low), high)
}

// orDefault substitutes a fallback for a zero value, which is what an element
// created before a field existed will carry.
func orDefault(value, fallback float64) float64 {
	if value <= 0 {
		return fallback
	}
	return value
}

// FormatInterval renders a number of days the way a reader thinks about it.
//
// Anki puts the resulting interval on each answer button, and it is right to:
// a grade whose effect you cannot see is a grade you have to remember the
// meaning of. "412d" is not a quantity anyone reasons about, so longer spans
// become months and years.
func FormatInterval(days float64) string {
	switch {
	case days < 1:
		return "today"
	case days < 30:
		return strconv.Itoa(int(math.Round(days))) + "d"
	case days < 365:
		return trimZero(days/30.44) + "mo"
	default:
		return trimZero(days/365.25) + "y"
	}
}

// trimZero renders one decimal place, dropping it when it adds nothing:
// "3.2" but "12" rather than "12.0".
func trimZero(value float64) string {
	text := strconv.FormatFloat(value, 'f', 1, 64)
	return strings.TrimSuffix(text, ".0")
}

// Preview is what one grade would do, for labelling a button with it.
type Preview struct {
	Grade    Grade
	Interval string

	// Terminal marks a grade that removes the element from the queue rather
	// than rescheduling it, so the interval is not the interesting part.
	Terminal bool
}

// Previews describes what each grade would do to a schedule.
//
// It calls Next rather than reimplementing the arithmetic, so a button can
// never advertise something the scheduler would not actually do — the preview
// is the behaviour, not a description of it.
func Previews(schedule Schedule, today time.Time) map[Grade]Preview {
	previews := make(map[Grade]Preview, 4)

	for _, grade := range []Grade{GradeNext, GradeSooner} {
		next := Next(schedule, grade, today)
		previews[grade] = Preview{Grade: grade, Interval: FormatInterval(next.IntervalDays)}
	}

	previews[GradeBury] = Preview{Grade: GradeBury, Interval: "today"}
	for _, grade := range []Grade{GradeDone, GradeDismiss, GradeSuspend} {
		previews[grade] = Preview{Grade: grade, Terminal: true}
	}
	return previews
}
