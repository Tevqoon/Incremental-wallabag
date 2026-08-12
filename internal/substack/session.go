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

// subscriptionState is the response shape of GET .../api/v1/subscription.
// The free-subscriber shape was confirmed live on 2026-08-12; the paid
// shape was confirmed live the same way once the operator's own
// subscription actually went through. A real paid response carried
// membership_state="subscribed", type="ios_app" (a plain string), and
// expiry=1789224995000 (a JSON number — see parseExpiry for what that
// number actually is), but bundle_id was still null even on that
// confirmed-paid account, so its own non-null shape remains unconfirmed.
// Type, Expiry, and BundleID stay decoded as json.RawMessage rather than
// concrete types for exactly that reason: committing this struct to one
// Go type per field would either break on BundleID's still-unknown shape
// or have broken already on Expiry's now-confirmed one being a number
// rather than the string this package first assumed. isFalsyJSON and
// parseExpiry each decide their own field's shape at the point that
// actually reads it, rather than this struct guessing up front.
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
//
// The compound second condition is load-bearing, not incidental, and this
// was confirmed the hard way: a real paid account's response still carries
// is_free_subscribed=true alongside membership_state="subscribed" and a
// real Type/Expiry/BundleID. Testing IsFreeSubscribed alone — the
// "obvious" simplification — would abort every genuine paid run, not just
// free ones. Requiring Type, Expiry, and BundleID to also all read as
// unset is what keeps a paid account, which never satisfies that, out of
// this branch. See TestSubscriptionStateLooksFree's confirmed-paid case,
// which pins this real payload as a non-free result specifically so this
// does not get "simplified" back to IsFreeSubscribed alone later.
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
func (s subscriptionState) expiresWithin(now time.Time, window time.Duration) (time.Time, bool) {
	if isFalsyJSON(s.Expiry) {
		return time.Time{}, false
	}
	parsed, ok := parseExpiry(s.Expiry)
	if !ok {
		return time.Time{}, false
	}
	remaining := parsed.Sub(now)
	if remaining > 0 && remaining <= window {
		return parsed, true
	}
	return time.Time{}, false
}

// secondsToMillisecondsCutoff disambiguates a bare numeric Expiry as Unix
// seconds or Unix milliseconds by magnitude: a value above this is read as
// milliseconds, at or below it as seconds.
//
// 1e11 seconds is the year 5138. Nothing that is genuinely a seconds-scale
// timestamp can exceed that in any lifetime this code will see, which is
// what makes the cutoff safe rather than merely convenient — there is no
// plausible real seconds value anywhere near this boundary for it to
// misclassify.
const secondsToMillisecondsCutoff = 1e11

// parseExpiry decodes raw into a time.Time, trying every shape Expiry has
// actually been seen to take, or is still plausible enough to keep
// supporting:
//
//   - A JSON number, confirmed live against a real paid subscription on
//     2026-08-12: raw was 1789224995000, a Unix millisecond timestamp
//     (2026-09-12, about a month out from a subscription taken on
//     2026-08-12 — consistent with Substack's own monthly billing cycle).
//     Tried first, since it is the confirmed shape; disambiguated from a
//     seconds-scale number by secondsToMillisecondsCutoff.
//   - A JSON string, in RFC3339 or a bare "2006-01-02" date — kept as a
//     fallback even though the confirmed shape is a number: a string is
//     still plausible on some other account or publication tier this
//     package has not seen, and dropping it on the strength of one
//     confirmed sample would be a regression for a case that has not
//     actually been ruled out.
//
// Anything else — a malformed number, a JSON object, anything that is
// neither of the two above — reports false rather than guessing.
func parseExpiry(raw json.RawMessage) (time.Time, bool) {
	var asNumber json.Number
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		millis, err := asNumber.Int64()
		if err != nil {
			// A number, but not a clean integer (a float, something with
			// an exponent) — not the confirmed shape, and not worth
			// guessing at.
			return time.Time{}, false
		}
		if millis > secondsToMillisecondsCutoff {
			return time.UnixMilli(millis), true
		}
		return time.Unix(millis, 0), true
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if parsed, err := time.Parse(layout, asString); err == nil {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

// diagnosisHost is a fixed, publication-independent host used only by
// diagnoseSubscriptionFailure's disambiguating probe. See that function's
// own doc comment for why a fixed host, rather than Config.Host, is the
// entire point.
//
// Read through Importer.settingsHost rather than referenced directly by
// diagnoseSubscriptionFailure below: New sets settingsHost to this value
// for every real caller, and that extra layer of indirection exists
// purely so a test in this package can redirect the probe at a fake
// server — the real substack.com obviously cannot itself be that fake
// server.
const diagnosisHost = "substack.com"

// diagnoseSubscriptionFailure runs when GET .../api/v1/subscription came
// back 401 or 404, resolving what that status alone cannot say.
//
// Confirmed live on 2026-08-12, directly against the API, not assumed:
//
//	request                                                        bogus session   valid session
//	GET https://substack.com/api/v1/settings                       401             200
//	GET https://{host}/api/v1/subscription                         404             200
//	GET https://nosuchpub-xyz99.substack.com/api/v1/subscription   404             —
//
// /api/v1/subscription is scoped to one publication (it lives under Host),
// so a 404 from it is genuinely ambiguous: it is the same status Substack
// returns both when the session cookie is dead and when Host simply does
// not name a real publication. There is no way to tell those two apart
// from that response alone — the whole reason this function exists.
//
// /api/v1/settings, by contrast, names no publication in its path at all —
// it is fixed at diagnosisHost regardless of what Config.Host is. A
// working cookie gets 200 from it no matter which publication (if any)
// Host names; a dead one gets 401 no matter what. That host-independence
// is what makes it authoritative about the cookie alone, where
// /api/v1/subscription cannot be.
//
// This second request is made only here, on the error path — never from a
// successful call to verifySubscriptionState — so a normal run still costs
// exactly one stage-1 request; see fetchOnce in post.go, which this uses
// specifically because it does not retry, keeping this to exactly one
// extra request even on a 5xx.
//
// Do not simplify this back to trusting /api/v1/subscription's status
// alone: that is precisely the shape that produced the bug this exists to
// fix — an operator with a perfectly good cookie and a mistyped Host
// seeing the exact same opaque 404 as one with a dead cookie, with no way
// to tell which problem they actually have.
func (i *Importer) diagnoseSubscriptionFailure(ctx context.Context, subscriptionErr error) error {
	endpoint := "https://" + i.settingsHost + "/api/v1/settings"
	_, err := i.fetchOnce(ctx, endpoint, true)

	switch {
	case err == nil:
		// settings succeeded — the cookie is fine — so subscription's own
		// failure must be about Host, not the session.
		return fmt.Errorf(
			"substack: %q does not appear to be a real Substack publication — the session cookie itself is valid (confirmed against %s), but the subscription check for %q found nothing there; Host must be the publication's own domain, e.g. \"example.substack.com\", or its mapped custom domain, not a guess",
			i.cfg.Host, i.settingsHost, i.cfg.Host,
		)
	case errors.Is(err, errUnauthorized):
		return fmt.Errorf(
			"substack: the session cookie was rejected (401 from %s) — it is dead or expired; re-export the substack.sid cookie value from a browser signed in to a paid Substack account and try again",
			i.settingsHost,
		)
	default:
		// Neither of the two known cases: settings itself returned
		// something other than 200 or 401 (a 5xx, say), or the probe
		// failed outright (a network error). Reporting both failures
		// verbatim, rather than guessing which of the two known causes
		// this resembles more, is deliberate — a wrong guess here would
		// send the operator chasing the wrong fix.
		return fmt.Errorf(
			"substack: could not determine why checking subscription state for %s failed — subscription check: %v; disambiguating settings probe: %v — this is neither of the two known cases (dead cookie, unknown publication), so nothing more specific can be said; try again",
			i.cfg.Host, subscriptionErr, err,
		)
	}
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
// conditions are well understood — "free_signup" was directly observed on a
// real free account, and a 401 or 404 is resolved unambiguously by
// diagnoseSubscriptionFailure below — but its *success* case is not: what a
// real paid subscriber's membership_state, type, and expiry actually look
// like was never confirmed, because the account available to check this
// against had no paid subscription to test with (see subscriptionState's
// own doc comment). So stage 1 can say "this account
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
		// A 401 or 404 here is exactly the ambiguous case
		// diagnoseSubscriptionFailure exists to resolve — see its own doc
		// comment for the live finding that this endpoint cannot itself
		// tell a dead session apart from a nonexistent publication.
		// Anything else (a persistent 5xx after fetchRaw's own retries, a
		// network failure) is reported as-is: there is no ambiguity to
		// resolve, only a plain failure to report.
		if errors.Is(err, errUnauthorized) || errors.Is(err, errNotFound) {
			return nil, i.diagnoseSubscriptionFailure(ctx, err)
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
