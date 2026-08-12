package substack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

// validSessionIDPrefix reports whether id has the shape a real substack.sid
// cookie value has.
//
// A valid value starts with "s%3A" — the URL-encoded form of "s:" — or its
// already-decoded equivalent "s:"; both were confirmed to authenticate
// against the live API. That prefix is not incidental: it is
// connect/express-style signed-cookie convention (Substack's backend
// evidently uses it), marking the value as a signed cookie rather than a
// bare token, and it is part of the credential itself.
//
// Checked at construction, in New, because a SessionID with this prefix
// accidentally stripped off produces a 401 that is otherwise
// indistinguishable at the HTTP level from a genuinely expired session —
// confirmed the hard way: an operator mistook "s%3A" for URL-escaping or
// shell syntax to strip off before pasting the value in, rather than part
// of the cookie itself, and the resulting 401 gave no hint which of "the
// session expired" or "the value is malformed" was actually true. Catching
// the shape here turns that into an immediate, specific error at startup
// instead of a baffling runtime 401 discovered later — see
// errUnauthorized's own doc comment in post.go for the other half of that
// same "which kind of auth failure is this" problem, at the point where a
// prefix that looked fine at construction time still turns out to be
// wrong.
func validSessionIDPrefix(id string) bool {
	return strings.HasPrefix(id, "s%3A") || strings.HasPrefix(id, "s:")
}

// soonExpiryWindow is how close to expiring a paid subscription has to be
// before verifySubscriptionState adds a Result.Warnings entry about it. The
// operator's own described workflow is subscribing for a month at a time
// specifically to backfill and then lapsing again, so a week of runway is
// enough notice to fit in one more run before that happens on its own.
const soonExpiryWindow = 7 * 24 * time.Hour

// subscriptionState is the response shape of GET .../api/v1/subscription —
// confirmed live against a free subscriber's own account on 2026-08-12.
// Type, Expiry, and BundleID are decoded as json.RawMessage rather than
// concrete types deliberately: for a free subscriber every one of them came
// back JSON null, but what a paid subscriber's response actually puts in
// their place — a string? a number? an object? — was never observed (the
// account used to check this had no active paid subscription to any
// publication at the time), so this does not guess a Go type that might not
// match. isFalsyJSON below only needs to tell "present and meaningful" apart
// from "null, empty, or false", which json.RawMessage can do without ever
// assuming a shape for the meaningful case.
type subscriptionState struct {
	MembershipState  string          `json:"membership_state"`
	IsFreeSubscribed bool            `json:"is_free_subscribed"`
	IsSubscribed     bool            `json:"is_subscribed"`
	Type             json.RawMessage `json:"type"`
	Expiry           json.RawMessage `json:"expiry"`
	IsFounding       bool            `json:"is_founding"`
	BundleID         json.RawMessage `json:"bundle_id"`
}

// isFalsyJSON reports whether raw is absent, JSON null, JSON false, or an
// empty string — every shape a "this is not set" field was actually seen to
// take, or could plausibly take given how loosely Substack's API appears to
// serialize an unset value (a bare null for one field, potentially a
// boolean false or an empty string for another that this package has not
// specifically observed).
func isFalsyJSON(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "false", `""`:
		return true
	default:
		return false
	}
}

// looksFree reports whether s describes an account with no paid access —
// the exact condition verifySubscriptionState aborts on.
//
// Two independent spellings are checked because only one of them was
// actually observed: membership_state == "free_signup" is the literal value
// a real free-subscriber response carried. The second — is_free_subscribed
// true alongside Type, Expiry, and BundleID all reading as unset — is
// there because membership_state's paid spelling is unknown (see
// subscriptionState's own doc comment), so a publication or Substack account
// tier that spells "free" some other way in membership_state should still
// be caught by the fields that were actually confirmed null on a free
// account, rather than slipping through because this only recognised one
// exact string.
func (s subscriptionState) looksFree() bool {
	if s.MembershipState == "free_signup" {
		return true
	}
	return s.IsFreeSubscribed && isFalsyJSON(s.Type) && isFalsyJSON(s.Expiry) && isFalsyJSON(s.BundleID)
}

// expiresWithin reports whether s.Expiry names a moment strictly between now
// and now+window, and what that moment is.
//
// now is a parameter rather than time.Now() called internally, so the
// window logic itself — the part actually worth unit testing — can be
// tested without the test racing a real clock. verifySubscriptionState is
// what supplies the real time.Now() at its own outer edge, the same
// clock-as-parameter split store and syncer already draw between their
// imperative shell and their pure decision logic.
//
// Expiry's actual format is unconfirmed the same way its presence is (see
// subscriptionState's own doc comment) — the account checked live had no
// paid subscription, so no real expiry value was ever seen, only inferred
// to be some date-shaped string. RFC3339 and a bare "2006-01-02" date are
// tried as the two most likely shapes; anything else fails to parse and
// this simply reports false, skipping the warning rather than guessing
// wrong and reporting a nonsense date to the operator.
func (s subscriptionState) expiresWithin(now time.Time, window time.Duration) (time.Time, bool) {
	if isFalsyJSON(s.Expiry) {
		return time.Time{}, false
	}
	var raw string
	if err := json.Unmarshal(s.Expiry, &raw); err != nil {
		// Expiry was present but not a JSON string (a number, an object —
		// some shape this package has not seen). Nothing to parse a date
		// out of, so this stays silent rather than guessing.
		return time.Time{}, false
	}

	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		parsed, err := time.Parse(layout, raw)
		if err != nil {
			continue
		}
		remaining := parsed.Sub(now)
		if remaining > 0 && remaining <= window {
			return parsed, true
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

// verifySubscriptionState is stage 1 of Ingest's two-stage session check: a
// single, cheap GET of the publication's own report of this account's
// access level, checked before anything else in a run — before even the
// archive walk — because it is the fastest way to give the operator a
// specific diagnosis if something is wrong, and because a bad SessionID
// should fail before spending any of the archive walk's own request budget.
//
// It exists alongside verifySession (stage 2, in post.go) rather than
// instead of it, and that is deliberate, not redundant: stage 1's failure
// conditions are well understood (a 401 is unambiguous; "free_signup" was
// directly observed on a real free account), but its *success* case is not
// — what a real paid subscriber's membership_state, type, and expiry
// actually look like was never confirmed, because the account available to
// check this against had no paid subscription to test with (see
// subscriptionState's own doc comment). So stage 1 can say "this account
// definitely has no paid access" or "this cookie is definitely dead" with
// confidence, but it cannot say "this account definitely does have paid
// access" — it can only fail to find a reason to say otherwise. Stage 2
// exists to supply the confirmation stage 1 cannot: it measures the thing
// that actually matters (does a paid post's body get longer with the
// cookie than without) rather than trusting an unverified success value.
// Whoever is tempted to delete one of these two checks as duplicating the
// other should read this paragraph first — they check different things,
// and only one of them (stage 2) is actually proven to detect "paid
// access" rather than merely failing to detect its absence.
func (i *Importer) verifySubscriptionState(ctx context.Context, logger *slog.Logger) ([]string, error) {
	raw, err := i.fetchRaw(ctx, "/api/v1/subscription", true)
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			return nil, fmt.Errorf(
				"substack: the session cookie was rejected (401) checking subscription state for %s — it is dead or malformed; refresh SessionID (the substack.sid cookie value)",
				i.cfg.Host,
			)
		}
		return nil, fmt.Errorf("substack: check subscription state for %s: %w", i.cfg.Host, err)
	}

	var state subscriptionState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("substack: decode subscription state for %s: %w", i.cfg.Host, err)
	}

	logger.Debug("subscription state",
		"host", i.cfg.Host,
		"membership_state", state.MembershipState,
		"is_free_subscribed", state.IsFreeSubscribed,
		"is_subscribed", state.IsSubscribed,
		"is_founding", state.IsFounding,
	)

	if state.looksFree() {
		return nil, fmt.Errorf(
			"substack: the session is valid but has no paid access to %s (membership_state=%q) — a paid subscription to this publication is required to backfill its paywalled posts",
			i.cfg.Host, state.MembershipState,
		)
	}

	var warnings []string
	if expiry, soon := state.expiresWithin(time.Now(), soonExpiryWindow); soon {
		warnings = append(warnings, fmt.Sprintf(
			"subscription to %s expires %s — run again before then to keep backfilling paywalled posts",
			i.cfg.Host, expiry.Format("2006-01-02"),
		))
	}
	return warnings, nil
}

// sessionCanaryMinRatio is how much larger an authenticated fetch's
// body_html must be than the same slug's unauthenticated fetch for
// verifySession (stage 2) to consider the session cookie genuinely working.
// See verifySession's own doc comment for the live evidence behind this
// approach and this specific number.
const sessionCanaryMinRatio = 1.2

// verifySession is stage 2 of Ingest's two-stage session check: ground
// truth, measured directly, for what stage 1 (verifySubscriptionState
// above) cannot actually confirm. It fetches one paid post twice — once
// with the cookie, once without — and compares how much body_html comes
// back each time.
//
// This replaced an earlier design (markup-based paywall detection: an
// element class, a body-length cutoff) that a live check against a real
// publication disproved outright, in both directions:
//
//   - A genuinely free, complete post can itself carry a
//     "subscription-widget" element (Substack's own subscribe prompt,
//     appended to the end of every post regardless of whether it is
//     paywalled) — so detecting "paywalled" on that class inverts the
//     signal on exactly the posts it must not fire on.
//   - A genuine paywalled preview, fetched with no cookie at all, carried
//     none of "paywall", "subscribe-widget", or "subscription-widget" —
//     confirmed against a real only_paid post's response, 24,767 bytes of
//     body_html that is nothing but ordinary prose markup, ending mid-piece
//     with a plain closing </p> and no marker of any kind. A body-length
//     cutoff fares no better: that preview is 24 KB, comfortably past any
//     threshold that would not also reject plenty of genuinely short free
//     posts.
//   - No field in a single post's own JSON reports truncation either.
//     should_send_free_preview looked promising but turned out to be a
//     publication-wide setting, not a property of the response — true for
//     the paid post checked and false for the free one, regardless of which
//     was actually being fetched. free_unlock_required, post_preview_limit,
//     exempt_from_archive_paywall, and unlockedWithIP were all identical
//     between a real free response and a real paywalled one. (The separate
//     /api/v1/subscription endpoint stage 1 checks is a different surface
//     entirely — an account-level access report, not a per-post field — and
//     is not what this finding is about.)
//
// In short: a paywalled preview is not distinguishable from a complete post
// by anything in a single response. What is reliable is the differential
// between two fetches of the same post — one with the cookie, one without —
// since Substack itself has to be able to tell the two apart to decide what
// to send back. sessionCanaryMinRatio is not a tight bound (a lapsed session
// still gets the same anonymous-length preview both times, i.e. a ratio near
// 1.0; a working one gets the full article, which was empirically far more
// than 1.2x the preview on the one pair actually checked) — it exists to
// catch a working-but-barely-larger preview variant this has not seen, not
// to be a precisely-tuned threshold.
//
// Only checked once per run, against the first only_paid post the archive
// names, rather than per-post: the cookie either works for the whole run or
// it does not, there is no notion of it working for some paid posts and not
// others, so one confirmation is exactly as much evidence as fetching every
// paid post twice would be, at a small fraction of the cost.
func (i *Importer) verifySession(ctx context.Context, logger *slog.Logger, posts []archivePost) error {
	var target *archivePost
	for idx := range posts {
		if posts[idx].Type == "newsletter" && posts[idx].Audience == "only_paid" {
			target = &posts[idx]
			break
		}
	}
	if target == nil {
		// Nothing in this archive is paid, so there is no post to verify
		// the cookie against — and nothing to protect either, since every
		// post Ingest is about to fetch is free regardless of whether the
		// cookie works.
		logger.Debug("no paid post found in archive; skipping stage 2 session verification")
		return nil
	}
	if err := validateSlug(target.Slug); err != nil {
		return fmt.Errorf("substack: cannot verify session against %q: %w", target.Slug, err)
	}

	path := "/api/v1/posts/" + url.PathEscape(target.Slug)

	authenticatedRaw, err := i.fetchRaw(ctx, path, true)
	if err != nil {
		return fmt.Errorf("substack: session verification: authenticated fetch of %q: %w", target.Slug, err)
	}
	anonymousRaw, err := i.fetchRaw(ctx, path, false)
	if err != nil {
		return fmt.Errorf("substack: session verification: unauthenticated fetch of %q: %w", target.Slug, err)
	}

	authLen, err := bodyHTMLLength(authenticatedRaw)
	if err != nil {
		return fmt.Errorf("substack: session verification: decode authenticated fetch of %q: %w", target.Slug, err)
	}
	anonLen, err := bodyHTMLLength(anonymousRaw)
	if err != nil {
		return fmt.Errorf("substack: session verification: decode unauthenticated fetch of %q: %w", target.Slug, err)
	}

	logger.Debug("stage 2 session verification",
		"slug", target.Slug, "authenticated_bytes", authLen, "anonymous_bytes", anonLen)

	if float64(authLen) < float64(anonLen)*sessionCanaryMinRatio {
		// This is the genuinely confusing case, and the message says so:
		// stage 1 already found a membership_state that did not look free,
		// so the account's own report says this should work — and yet the
		// post itself did not visibly unlock. That mismatch is exactly why
		// stage 2 exists rather than trusting stage 1 alone.
		return fmt.Errorf(
			"substack: the account reports paid access to %s but a live fetch of %q still returned what looks like a preview (authenticated body_html %d bytes vs %d unauthenticated, ratio below %.1fx) — membership_state passed stage 1, yet this post did not visibly unlock; the account may not actually be entitled to this specific publication despite what it reports, or Substack may be serving a stale response — try again, and if it persists, verify paid access to this exact publication manually",
			i.cfg.Host, target.Slug, authLen, anonLen, sessionCanaryMinRatio,
		)
	}
	logger.Info("stage 2 session verified", "slug", target.Slug, "authenticated_bytes", authLen, "anonymous_bytes", anonLen)
	return nil
}

// bodyHTMLLength decodes just enough of a raw post response to read the
// length of its body_html field, without paying for a full postBody decode.
func bodyHTMLLength(raw []byte) (int, error) {
	var partial struct {
		BodyHTML string `json:"body_html"`
	}
	if err := json.Unmarshal(raw, &partial); err != nil {
		return 0, err
	}
	return len(partial.BodyHTML), nil
}
