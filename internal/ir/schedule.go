package ir

import (
	"math"
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
	// GradePause: read a slice, extracted what mattered, stop here for now.
	// The read point is kept and the interval grows normally. This is the
	// everyday action — incremental reading is mostly the act of putting
	// something down on purpose.
	GradePause Grade = iota
	// GradeLater: not compelling right now; push it out and let it drift further each time.
	GradeLater
	// GradeSooner: more interesting than expected; bring it back and let it drift more slowly.
	GradeSooner
	// GradeDone: finished with this material.
	GradeDone
	// GradeDismiss: abandoned; not worth finishing.
	GradeDismiss
	// GradeSuspend: parked indefinitely, resumable. Keeps the interval and
	// repetition count, unlike the two terminal grades above.
	GradeSuspend
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

	case GradeLater:
		next.AFactor = clamp(next.AFactor*aFactorStep, minAFactor, maxAFactor)
		next.IntervalDays = grow(schedule.IntervalDays, next.AFactor)

	case GradeSooner:
		next.AFactor = clamp(next.AFactor/aFactorStep, minAFactor, maxAFactor)
		// Come back in half the time just waited, rather than growing at all.
		// Sooner is a request to see this again, so growth would contradict it.
		next.IntervalDays = math.Max(minInterval, schedule.IntervalDays/2)

	default: // GradePause
		next.IntervalDays = grow(schedule.IntervalDays, next.AFactor)
	}

	next.IntervalDays = math.Min(next.IntervalDays, priorityCap(next.Priority))
	next.Reps = schedule.Reps + 1
	next.State = StateReading
	next.DueOn = Day(today).AddDate(0, 0, int(math.Round(next.IntervalDays)))
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
