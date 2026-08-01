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
	// something down on purpose.
	GradeNext Grade = iota
	// GradeDefer: not compelling right now; push it out and let it drift
	// further each time.
	GradeDefer
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

	case GradeDefer:
		next.AFactor = clamp(next.AFactor*aFactorStep, minAFactor, maxAFactor)
		next.IntervalDays = grow(schedule.IntervalDays, next.AFactor)

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

// FreshInterval is the interval an ungraded element is treated as carrying
// before it has ever been graded: one day at priority 0.0 up to a year at
// priority 1.0. Used both to back-fill a preview (see EffectiveSchedule, for
// an extract or highlight) and to actually reschedule one (see Reprioritize,
// for either).
//
// priorityCap only ever caps an interval a real grade already produced —
// there is nothing to cap before the first one. Without a baseline of its
// own, an ungraded element's interval sits at zero regardless of priority, so
// grow's flat one-day floor is all Next, Sooner and Defer can ever preview
// for it, and priority has nothing to visibly act on until it is graded once.
func FreshInterval(priority float64) float64 {
	return math.Pow(maxPriorityCap, clamp(priority, 0, 1))
}

// EffectiveSchedule is the schedule Next and Previews should actually use.
//
// For anything already graded at least once, that is just s unchanged — a
// due date earned by actually reading something is not something priority
// second-guesses; see the clamp in Next for how priority still bounds it.
// Root articles are excluded even when ungraded: an article's due date is
// import's own, real and already accurate the moment Reprioritize has
// touched it, so there is nothing left to substitute for preview purposes —
// unlike an extract or highlight, which only ever gets a real interval once
// graded, so FreshInterval fills in for the stored (zero) one, keeping its
// preview honest about what priority alone would do right now.
func (s Schedule) EffectiveSchedule(isRoot bool) Schedule {
	if isRoot || s.State != StateNew {
		return s
	}
	effective := s
	effective.IntervalDays = FreshInterval(s.Priority)
	return effective
}

// Reprioritize changes an element's priority and, for anything ungraded,
// immediately pushes its due date out to match — see EffectiveSchedule for
// why priority otherwise has nothing to act on until graded once. That
// covers both an imported highlight and a whole article the reader wants to
// put off — "I'll read this in a week" is exactly this, applied to a root.
//
// A root gets one extra protection an extract does not need: the very first
// time this runs on it, only a move toward less important reschedules it,
// and only ever further out than wherever it is already due. An article is
// due immediately by default, and without that guard, simply raising its
// priority (nudging it toward more important) would first jump the due date
// out to FreshInterval's own floor — always later than "today" — which is
// the opposite of what raising priority is supposed to mean. An extract has
// no such default to protect: importedPriority already delays it, so there
// is no "immediate" state a stray touch could undo.
//
// Once a root has been through here at all, the guard no longer applies: its
// interval is no longer import's default, and from then on it tracks the
// slider freely in both directions, exactly like an extract already does —
// see the non-root case for why leaving it anchored to the furthest point
// ever dragged to would collapse Sooner, Next and Defer into one identical,
// useless preview.
func Reprioritize(schedule Schedule, priority float64, isRoot bool, today time.Time) Schedule {
	next := schedule
	next.AFactor = clamp(orDefault(schedule.AFactor, defaultAFactor), minAFactor, maxAFactor)
	next.Priority = clamp(priority, 0, 1)

	if schedule.State != StateNew {
		return next
	}

	interval := FreshInterval(next.Priority)
	candidate := Day(today).AddDate(0, 0, int(math.Round(interval)))

	if isRoot && schedule.IntervalDays < minInterval {
		switch {
		case next.Priority <= schedule.Priority:
			return next
		case !schedule.DueOn.IsZero() && !candidate.After(schedule.DueOn):
			return next
		}
	}

	next.IntervalDays = interval
	next.DueOn = candidate
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

	for _, grade := range []Grade{GradeNext, GradeSooner, GradeDefer} {
		next := Next(schedule, grade, today)
		previews[grade] = Preview{Grade: grade, Interval: FormatInterval(next.IntervalDays)}
	}

	previews[GradeBury] = Preview{Grade: GradeBury, Interval: "today"}
	for _, grade := range []Grade{GradeDone, GradeDismiss, GradeSuspend} {
		previews[grade] = Preview{Grade: grade, Terminal: true}
	}
	return previews
}
