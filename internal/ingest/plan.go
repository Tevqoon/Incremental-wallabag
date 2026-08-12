package ingest

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/wallabag"
)

// Action is what BuildPlan decided to do with one post.
type Action string

const (
	// ActionCreate means no wallabag entry matched this post's slug at all;
	// it does not exist there yet.
	ActionCreate Action = "create"

	// ActionUpdate means a matching entry exists but its content is not yet
	// the full post — most commonly a paywall preview waiting to be
	// replaced. Its annotations, if any, are re-anchored in the same pass
	// regardless of their own state, since the content they were anchored
	// against is about to change under them anyway.
	ActionUpdate Action = "update"

	// ActionAnnotationsOnly means the content is already full — this run,
	// or an earlier one, already put the real article up there — but one or
	// more annotations are not anchored to it. This is exactly the state a
	// run that PATCHed content and then died before re-anchoring leaves
	// behind, which is why content and annotation state are classified
	// independently rather than the latter being conditional on the former:
	// making annotation work depend on content work would strand these
	// annotations permanently on the very next clean run, since content
	// would already read as done.
	ActionAnnotationsOnly Action = "annotations"

	// ActionSkip means both content and every annotation are already
	// correct. A second run over a batch nothing has changed since must
	// classify everything this way and write nothing at all.
	ActionSkip Action = "skip"

	// ActionConflict means two or more wallabag entries under this post's
	// slug carry annotations, and nothing was planned for the post at all —
	// see the conflict handling in planOne for why picking one silently is
	// worse than refusing.
	ActionConflict Action = "conflict"
)

// Verdict is what an already-existing annotation's own position resolves to
// against the content a post is about to have (or already has).
type Verdict string

const (
	// VerdictAnchored means the annotation's stored ranges already resolve,
	// against this content, to text matching its own quote — wallabag's own
	// reader can draw it in place today, and re-anchoring it would be
	// pointless churn.
	VerdictAnchored Verdict = "anchored"

	// VerdictUnique means the ranges do not resolve (or the annotation has
	// none at all), but its quote occurs exactly once in this content —
	// re-anchoring is unambiguous.
	VerdictUnique Verdict = "unique"

	// VerdictAmbiguous means the quote occurs more than once. Re-anchoring
	// will still happen — see Apply — but which occurrence it lands on is
	// not decided by anything smarter than "the first one", and this
	// verdict exists to flag that to the operator, not to resolve it.
	VerdictAmbiguous Verdict = "ambiguous"

	// VerdictMissing means the quote does not occur in this content at all
	// — most likely the passage was edited or removed upstream since the
	// highlight was made. Listed in the report for the operator to fix by
	// hand; nothing here can recover it.
	VerdictMissing Verdict = "missing"
)

// truncateLimit mirrors wallabag.maxHighlightQuoteLength
// (internal/wallabag/write.go), which is unexported there. Duplicated here
// rather than exported across the package boundary for the sake of one
// caller: this package needs to answer "would re-anchoring this quote hit
// wallabag's own truncation", which is a fact about wallabag's behaviour
// that this comment exists to describe, not a capability this package needs
// to own. See CreateHighlight's own comment in write.go for where the
// number itself comes from — bisected against the live API, not read out of
// wallabag's published validator, which claims a limit ten times as large
// and is wrong in practice.
const truncateLimit = 900

// AnnotationPlan is what BuildPlan decided about one already-existing
// annotation.
type AnnotationPlan struct {
	// AnnotationID is the annotation's current id at wallabag — the value
	// UpdateHighlightLocation needs to identify what it is replacing, and
	// what RemapExternalRef's "old" side refers to afterward.
	AnnotationID int

	// Quote is the annotation's own stored text, as wallabag currently has
	// it — not necessarily what CreateHighlight will actually send if this
	// gets re-anchored, since re-anchoring always goes through
	// UpdateHighlightLocation with the quote a caller supplies, and this
	// plan does not decide what that caller passes; Apply passes this same
	// field back.
	Quote string

	Verdict     Verdict
	Occurrences int

	// Truncates is true when Quote is longer than truncateLimit — a
	// re-anchor of this annotation will come back from wallabag holding a
	// shortened, "…"-suffixed quote rather than the original text, which
	// means it will not byte-for-byte match anything local, and in
	// particular will miss insertHighlights' exact-quote adopt path
	// (elements.go) on the next sync. That is exactly why RemapExternalRef
	// exists as an explicit call rather than relying on that adopt path to
	// find its way there on its own.
	Truncates bool
}

// Item is BuildPlan's decision for one post.
type Item struct {
	Post source.Document
	Slug string

	// EntryID is the wallabag entry this post maps onto — zero for
	// ActionCreate, where there is nothing yet to map onto, and also zero
	// for ActionConflict, where BuildPlan deliberately refuses to pick one.
	EntryID int

	Action Action

	// ContentFull reports whether the matched entry's content already
	// equals this post's own content — meaningless (left false) for
	// ActionCreate and ActionConflict, where there is no matched content to
	// compare.
	ContentFull bool

	// Annotations is every annotation the matched entry currently carries,
	// classified independently of ContentFull — see ActionAnnotationsOnly's
	// own comment for why. Empty for ActionCreate (nothing exists yet to
	// have annotations) and for ActionConflict.
	Annotations []AnnotationPlan

	// Notes records anything the operator should read before trusting this
	// item at face value: which other entries shared this slug and were
	// shadowed rather than touched, or — for ActionConflict — the ids and
	// annotation counts of every annotated candidate that made this post
	// unresolvable.
	Notes []string
}

// Plan is BuildPlan's full decision over a batch of posts.
type Plan struct {
	Items []Item

	// Conflicts is the count of Items with ActionConflict — surfaced
	// separately from walking Items again so a caller (the report, most
	// directly) can headline it without a second pass.
	Conflicts int
}

// BuildPlan decides what to do with each post, from a Snapshot Gather has
// already read. It is a pure function: no context, no client, and no clock
// beyond now, which is what makes every rule below testable against literal
// values with no fake wallabag server anywhere in sight. now is threaded
// through for the same reason every other write path in this codebase takes
// it explicitly rather than reading the clock itself — nothing in the
// current rule set actually needs it, but a caller supplying a pinned time
// is what keeps a future rule that does need "now" (a grace period before
// treating a fresh preview as stale, say) from having to change this
// function's signature and, with it, every existing caller and test.
func BuildPlan(posts []source.Document, snap Snapshot, now time.Time) Plan {
	plan := Plan{Items: make([]Item, 0, len(posts))}
	for _, post := range posts {
		item := planOne(post, snap)
		if item.Action == ActionConflict {
			plan.Conflicts++
		}
		plan.Items = append(plan.Items, item)
	}
	return plan
}

// planOne decides one post's Item.
func planOne(post source.Document, snap Snapshot) Item {
	slug := slugOf(post.URL)
	candidates := snap.BySlug[slug]

	var annotated []wallabag.Entry
	for _, candidate := range candidates {
		if len(candidate.Annotations) > 0 {
			annotated = append(annotated, candidate)
		}
	}

	// Two or more annotated candidates under one slug: refuse rather than
	// guess. Filling one and leaving the other stale would leave two copies
	// of a post the operator actually highlighted, one of them silently
	// wrong — a worse outcome than doing nothing and telling the operator
	// to sort it out by hand.
	if len(annotated) >= 2 {
		notes := make([]string, 0, len(annotated))
		for _, entry := range annotated {
			notes = append(notes, fmt.Sprintf(
				"entry %d carries %d annotation(s) under this slug — refusing to choose between annotated duplicates",
				entry.ID, len(entry.Annotations)))
		}
		return Item{Post: post, Slug: slug, Action: ActionConflict, Notes: notes}
	}

	target, hasTarget := chooseTarget(candidates, annotated)

	item := Item{Post: post, Slug: slug}
	if !hasTarget {
		item.Action = ActionCreate
		return item
	}
	item.EntryID = target.ID

	for _, candidate := range candidates {
		if candidate.ID != target.ID {
			item.Notes = append(item.Notes, fmt.Sprintf(
				"entry %d also matches this slug but was shadowed by entry %d (the annotated or newer of the two)",
				candidate.ID, target.ID))
		}
	}

	full, hasFull := snap.Details[target.ID]
	if !hasFull {
		// Gather only populates Details for entries it fetched a full body
		// for, which should always include target once it has been chosen
		// from candidates — those came from the same slug lookup Gather
		// used to decide what to fetch. Falling back to "needs a content
		// update, nothing known about its annotations" rather than
		// panicking is a defensive floor for a Snapshot a caller assembled
		// by hand (every test in this package does exactly that) without
		// populating Details to match.
		item.Action = ActionUpdate
		return item
	}

	item.ContentFull = full.Content == post.ContentHTML

	var annotationsStale bool
	for _, ann := range full.Annotations {
		annPlan := planAnnotation(ann, post.ContentHTML)
		item.Annotations = append(item.Annotations, annPlan)
		if annPlan.Verdict != VerdictAnchored {
			annotationsStale = true
		}
	}

	switch {
	case !item.ContentFull:
		item.Action = ActionUpdate
	case annotationsStale:
		item.Action = ActionAnnotationsOnly
	default:
		item.Action = ActionSkip
	}
	return item
}

// chooseTarget picks which candidate a post maps onto, following the same
// rule for zero-or-one annotated candidates that planOne's caller already
// special-cased for two-or-more: the annotated one wins if there is exactly
// one, otherwise the newest by CreatedAt — "newest" being the most plausible
// guess for "the one still relevant" among plain unannotated duplicates,
// with nothing else to distinguish them by.
func chooseTarget(candidates, annotated []wallabag.Entry) (wallabag.Entry, bool) {
	switch {
	case len(annotated) == 1:
		return annotated[0], true
	case len(candidates) > 0:
		newest := candidates[0]
		for _, candidate := range candidates[1:] {
			if candidate.CreatedAt.After(newest.CreatedAt.Time) {
				newest = candidate
			}
		}
		return newest, true
	default:
		return wallabag.Entry{}, false
	}
}

// planAnnotation classifies one existing annotation against newContent — the
// content the matched entry either already has (ContentFull true) or is
// about to have (an ActionUpdate in progress). Using the same newContent
// either way is deliberate: an annotation anchored against the old preview
// body must be judged against where the passage will actually end up, not
// against a body about to be replaced out from under it.
func planAnnotation(ann wallabag.Annotation, newContent string) AnnotationPlan {
	// Annotation.Ranges is []json.RawMessage — one already-decoded element
	// per range — while QuoteAnchored wants the whole array as a single
	// json.RawMessage, the shape a source.Highlight carries it in. Encoding
	// back is the only place these two representations meet, and it cannot
	// fail on a value that was itself just decoded from JSON.
	rangesJSON, _ := json.Marshal(ann.Ranges)

	anchored := wallabag.QuoteAnchored(newContent, rangesJSON, ann.Quote)
	occurrences := wallabag.QuoteOccurrences(newContent, ann.Quote)
	count := len(occurrences)

	var verdict Verdict
	switch {
	case anchored:
		verdict = VerdictAnchored
	case count == 1:
		verdict = VerdictUnique
	case count > 1:
		verdict = VerdictAmbiguous
	default:
		verdict = VerdictMissing
	}

	return AnnotationPlan{
		AnnotationID: ann.ID,
		Quote:        ann.Quote,
		Verdict:      verdict,
		Occurrences:  count,
		Truncates:    len(ann.Quote) > truncateLimit,
	}
}
