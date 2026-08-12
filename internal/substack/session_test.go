package substack

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNewRejectsMalformedSessionIDPrefix pins the cookie-shape check: a
// SessionID missing the "s%3A"/"s:" prefix must be rejected at
// construction, not left to surface as an unexplained 401 later — the exact
// failure mode a real operator hit (see validSessionIDPrefix's own doc
// comment in session.go).
func TestNewRejectsMalformedSessionIDPrefix(t *testing.T) {
	tests := []string{
		"aBcDeF1234567890.signature", // the "s%3A" stripped off, as the real operator did
		"3AaBcDeF.signature",         // missing even the leading "s"
	}
	for _, id := range tests {
		t.Run(id, func(t *testing.T) {
			_, err := New(Config{Host: "example.substack.com", CacheDir: t.TempDir(), SessionID: id})
			if err == nil {
				t.Fatalf("New(SessionID=%q): expected an error", id)
			}
		})
	}
}

// TestNewAcceptsBothSessionIDForms checks both the URL-encoded and decoded
// spellings validSessionIDPrefix accepts — both were confirmed live to
// authenticate against the real API, so New must not reject either.
func TestNewAcceptsBothSessionIDForms(t *testing.T) {
	for _, id := range []string{
		"s%3AaBcDeF1234567890.signature",
		"s:aBcDeF1234567890.signature",
	} {
		t.Run(id, func(t *testing.T) {
			_, err := New(Config{Host: "example.substack.com", CacheDir: t.TempDir(), SessionID: id})
			if err != nil {
				t.Errorf("New(SessionID=%q): unexpected error: %v", id, err)
			}
		})
	}
}

// TestSubscriptionStateLooksFree is a table over subscriptionState.looksFree
// covering both spellings of "no paid access" it recognises: the literal
// membership_state value actually confirmed live, and the
// IsFreeSubscribed+all-null fallback for a publication that might spell
// membership_state differently.
func TestSubscriptionStateLooksFree(t *testing.T) {
	tests := []struct {
		name  string
		state subscriptionState
		want  bool
	}{
		{
			name: "confirmed live free_signup shape",
			state: subscriptionState{
				MembershipState:  "free_signup",
				IsFreeSubscribed: true,
				Type:             json.RawMessage("null"),
				Expiry:           json.RawMessage("null"),
				BundleID:         json.RawMessage("null"),
			},
			want: true,
		},
		{
			name: "unrecognised membership_state but same null pattern as the free account",
			state: subscriptionState{
				MembershipState:  "something_this_package_has_not_seen",
				IsFreeSubscribed: true,
				Type:             json.RawMessage("null"),
				Expiry:           json.RawMessage("null"),
				BundleID:         json.RawMessage("null"),
			},
			want: true,
		},
		{
			name: "unrecognised membership_state with a real type/expiry present — must NOT be treated as free",
			state: subscriptionState{
				MembershipState:  "something_this_package_has_not_seen",
				IsFreeSubscribed: false,
				Type:             json.RawMessage(`"paid_subscription"`),
				Expiry:           json.RawMessage(`"2026-09-01"`),
				BundleID:         json.RawMessage("null"),
			},
			want: false,
		},
		{
			name: "is_free_subscribed true but type present — not the free pattern",
			state: subscriptionState{
				MembershipState:  "some_state",
				IsFreeSubscribed: true,
				Type:             json.RawMessage(`"paid"`),
				Expiry:           json.RawMessage("null"),
				BundleID:         json.RawMessage("null"),
			},
			want: false,
		},
		{
			// The confirmed real response from a paid subscriber, once the
			// operator actually subscribed on 2026-08-12 — see
			// subscriptionState's own doc comment. is_free_subscribed is
			// still true here, exactly as it was on the free account; what
			// keeps this out of looksFree is Type and Expiry both being
			// real, non-null values. This is the case looksFree's own doc
			// comment specifically warns a future "simplify this to just
			// IsFreeSubscribed" edit would break — pinned here so that
			// edit fails a test instead of silently aborting every real
			// paid run.
			name: "confirmed live paid account — is_free_subscribed is true here too, and must NOT be treated as free",
			state: subscriptionState{
				MembershipState:  "subscribed",
				IsFreeSubscribed: true,
				IsSubscribed:     true,
				Type:             json.RawMessage(`"ios_app"`),
				Expiry:           json.RawMessage(`1789224995000`),
				IsFounding:       false,
				BundleID:         json.RawMessage("null"),
			},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.state.looksFree(); got != test.want {
				t.Errorf("looksFree() = %v, want %v", got, test.want)
			}
		})
	}
}

// TestSubscriptionStateExpiresWithin pins the pure decision logic behind
// the Result.Warnings expiry notice: now is passed in explicitly (see
// expiresWithin's own doc comment on why), so this tests the window
// arithmetic itself without racing a real clock. See TestParseExpiry for
// the separate, lower-level question of how a raw Expiry value decodes
// into a time.Time at all — this table is about what expiresWithin does
// with that decoded time relative to now and window, using both a string
// and a numeric encoding to prove both actually flow through end to end,
// not just parseExpiry in isolation.
func TestSubscriptionStateExpiresWithin(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	window := 7 * 24 * time.Hour

	tests := []struct {
		name   string
		expiry json.RawMessage
		want   bool
	}{
		{
			name:   "no expiry at all",
			expiry: json.RawMessage("null"),
			want:   false,
		},
		{
			name:   "expires in 3 days, within the window (string date)",
			expiry: json.RawMessage(`"2026-08-15"`),
			want:   true,
		},
		{
			name:   "expires in 30 days, outside the window (string date)",
			expiry: json.RawMessage(`"2026-09-11"`),
			want:   false,
		},
		{
			name:   "already expired (in the past) — not a future warning",
			expiry: json.RawMessage(`"2026-08-01"`),
			want:   false,
		},
		{
			name:   "expires in 3 days, within the window, as a millisecond epoch number",
			expiry: json.RawMessage(strconv.FormatInt(now.Add(3*24*time.Hour).UnixMilli(), 10)),
			want:   true,
		},
		{
			name:   "expires in 30 days, outside the window, as a seconds epoch number",
			expiry: json.RawMessage(strconv.FormatInt(now.Add(30*24*time.Hour).Unix(), 10)),
			want:   false,
		},
		{
			name:   "malformed JSON — fails silently rather than guessing",
			expiry: json.RawMessage(`not valid json`),
			want:   false,
		},
		{
			name:   "a JSON object — neither known shape",
			expiry: json.RawMessage(`{"foo":"bar"}`),
			want:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := subscriptionState{Expiry: test.expiry}
			_, got := state.expiresWithin(now, window)
			if got != test.want {
				t.Errorf("expiresWithin() ok = %v, want %v", got, test.want)
			}
		})
	}
}

// TestParseExpiry pins the raw shape-decoding parseExpiry does, independent
// of the window arithmetic expiresWithin layers on top — see that
// function's own doc comment in session.go for the live finding this
// exists to cover: Expiry is confirmed to arrive as a millisecond Unix
// epoch number on a real paid subscription, not the date string this
// package first assumed with no data to check it against.
func TestParseExpiry(t *testing.T) {
	tests := []struct {
		name   string
		raw    json.RawMessage
		want   time.Time
		wantOK bool
	}{
		{
			// The exact value confirmed live on 2026-08-12 against a real
			// paid subscription: 1789224995000. Read as milliseconds (not
			// seconds — secondsToMillisecondsCutoff is what tells them
			// apart), this is 2026-09-12T14:56:35Z, roughly a month after
			// the subscription was taken, consistent with Substack's own
			// monthly billing. Comparing against time.UnixMilli(...)
			// directly, rather than a hand-written date, is deliberate: if
			// the magnitude cutoff ever misclassified this as seconds
			// instead, the result would land tens of thousands of years in
			// the future, not a day or two off, so this comparison catches
			// exactly the failure mode that matters here.
			name:   "confirmed live millisecond epoch from a real paid subscription",
			raw:    json.RawMessage(`1789224995000`),
			want:   time.UnixMilli(1789224995000),
			wantOK: true,
		},
		{
			name:   "a seconds-scale epoch number, well below the cutoff",
			raw:    json.RawMessage(`1700000000`),
			want:   time.Unix(1700000000, 0),
			wantOK: true,
		},
		{
			name:   "an RFC3339 string still works",
			raw:    json.RawMessage(`"2026-09-11T00:00:00Z"`),
			want:   time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "a bare 2006-01-02 date string still works",
			raw:    json.RawMessage(`"2026-09-11"`),
			want:   time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "malformed JSON",
			raw:    json.RawMessage(`not valid json at all`),
			wantOK: false,
		},
		{
			name:   "a JSON object — neither a number nor a string",
			raw:    json.RawMessage(`{"foo":"bar"}`),
			wantOK: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseExpiry(test.raw)
			if ok != test.wantOK {
				t.Fatalf("parseExpiry() ok = %v, want %v", ok, test.wantOK)
			}
			if ok && !got.Equal(test.want) {
				t.Errorf("parseExpiry() = %v, want %v", got, test.want)
			}
		})
	}
}

// TestIngestAbortsWhenSubscriptionIsFree covers stage 1's primary failure
// mode: a session that authenticates fine but has no paid access to the
// publication at all. This must abort before the archive is even walked —
// asserted here by checking the archive endpoint was never requested.
func TestIngestAbortsWhenSubscriptionIsFree(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.subscription = freeSubscriptionFixture()
	// Deliberately not populated: if Ingest reached the archive walk at
	// all, it would 400 against the fake server's own limit check, making
	// any accidental archive request loudly visible as a different failure
	// than the one this test is checking for.

	importer := newTestImporter(t, fake.Server, nil)
	logger := testLogger(&strings.Builder{})

	_, result, err := importer.Ingest(context.Background(), logger)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no paid access") {
		t.Errorf("error = %q, want it to say the account has no paid access", err.Error())
	}
	if !strings.Contains(err.Error(), "free_signup") {
		t.Errorf("error = %q, want it to name the actual membership_state", err.Error())
	}
	if result.Pages != 0 {
		t.Errorf("Result.Pages = %d, want 0 (the archive must never be walked)", result.Pages)
	}

	for _, path := range fake.requestedPaths {
		if strings.HasPrefix(path, "/api/v1/archive") {
			t.Errorf("archive was requested (%s) despite stage 1 failing first", path)
		}
	}
	if fake.settingsRequests != 0 {
		t.Errorf("settings was requested %d times, want 0 — subscription succeeded (200), so there is nothing for diagnoseSubscriptionFailure to resolve", fake.settingsRequests)
	}
}

// TestIngestDisambiguatesSubscription404 is the table behind the bug this
// was built to fix: /api/v1/subscription answers 404 both when the session
// is dead and when Host names no real publication — confirmed live,
// unable to tell those apart on its own — so diagnoseSubscriptionFailure's
// second probe against /api/v1/settings is what actually decides, and each
// of its three possible outcomes must produce a distinguishably different
// error. Asserting on the returned error's exact text, not just that an
// error occurred, is the entire point: three cases that all just said
// "subscription check failed" would be exactly the regression this guards
// against.
func TestIngestDisambiguatesSubscription404(t *testing.T) {
	tests := []struct {
		name           string
		settingsStatus int
		wantContains   []string
		wantNotContain []string
	}{
		{
			name:           "settings 401: the cookie itself is dead",
			settingsStatus: http.StatusUnauthorized,
			wantContains:   []string{"dead or expired", "401"},
			wantNotContain: []string{"does not appear to be a real Substack publication", "neither of the two known cases"},
		},
		{
			name:           "settings 200: the cookie works, so Host must be wrong",
			settingsStatus: http.StatusOK,
			wantContains:   []string{"does not appear to be a real Substack publication", "Host must be"},
			wantNotContain: []string{"dead or expired", "neither of the two known cases"},
		},
		{
			name:           "settings 500: neither known case, report both failures rather than guess",
			settingsStatus: http.StatusInternalServerError,
			wantContains:   []string{"neither of the two known cases", "subscription check:", "settings probe:"},
			wantNotContain: []string{"dead or expired", "does not appear to be a real Substack publication"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeSubstack(t)
			fake.subscriptionStatus = http.StatusNotFound
			fake.settingsStatus = test.settingsStatus

			importer := newTestImporter(t, fake.Server, nil)
			logger := testLogger(&strings.Builder{})

			_, _, err := importer.Ingest(context.Background(), logger)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range test.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), want)
				}
			}
			for _, notWant := range test.wantNotContain {
				if strings.Contains(err.Error(), notWant) {
					t.Errorf("error = %q, must not contain %q (that belongs to a different one of the three cases)", err.Error(), notWant)
				}
			}
			if fake.settingsRequests != 1 {
				t.Errorf("settings was requested %d times, want exactly 1", fake.settingsRequests)
			}
		})
	}
}

// TestIngestProceedsOnUnrecognisedMembershipState checks the deliberate
// asymmetry in stage 1: an unrecognised membership_state must NOT be
// treated as failure, since the paid spelling was never confirmed live —
// guessing it wrong would abort every real paid run. Only stage 2 (the
// differential canary) actually gates the import.
func TestIngestProceedsOnUnrecognisedMembershipState(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.subscription = subscriptionFixture{
		MembershipState:  "some_paid_tier_this_package_has_never_seen",
		IsFreeSubscribed: false,
		IsSubscribed:     true,
		Type:             "paid_subscriber",
		Expiry:           "2027-01-01",
	}
	fake.archivePages[0] = []archiveFixture{
		newArchiveFixture(1, "only-free-post", "newsletter", "everyone"),
	}
	fake.posts["only-free-post"] = newFreePostFixture(1, "only-free-post")

	importer := newTestImporter(t, fake.Server, nil)
	logger := testLogger(&strings.Builder{})

	documents, _, err := importer.Ingest(context.Background(), logger)
	if err != nil {
		t.Fatalf("Ingest: %v (an unrecognised membership_state must not itself abort)", err)
	}
	if len(documents) != 1 {
		t.Errorf("len(documents) = %d, want 1", len(documents))
	}
	if fake.settingsRequests != 0 {
		t.Errorf("settings was requested %d times on a successful run, want 0 — the disambiguating probe must cost nothing when there is nothing to disambiguate", fake.settingsRequests)
	}
}

// TestIngestWarnsOnExpirySoon checks the Result.Warnings notice: an expiry
// within soonExpiryWindow must produce a warning naming the date, without
// aborting the run.
func TestIngestWarnsOnExpirySoon(t *testing.T) {
	fake := newFakeSubstack(t)
	soon := time.Now().Add(3 * 24 * time.Hour).Format("2006-01-02")
	fake.subscription = subscriptionFixture{
		MembershipState:  "subscribed",
		IsFreeSubscribed: false,
		IsSubscribed:     true,
		Type:             "paid",
		Expiry:           soon,
	}
	fake.archivePages[0] = []archiveFixture{
		newArchiveFixture(1, "only-free-post", "newsletter", "everyone"),
	}
	fake.posts["only-free-post"] = newFreePostFixture(1, "only-free-post")

	importer := newTestImporter(t, fake.Server, nil)
	logger := testLogger(&strings.Builder{})

	_, result, err := importer.Ingest(context.Background(), logger)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "expires") && strings.Contains(w, soon) {
			found = true
		}
	}
	if !found {
		t.Errorf("Result.Warnings = %v, want an entry naming the expiry date %s", result.Warnings, soon)
	}
}
