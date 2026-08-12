package substack

import (
	"context"
	"encoding/json"
	"net/http"
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
// arithmetic itself without racing a real clock.
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
			name:   "expires in 3 days, within the window",
			expiry: json.RawMessage(`"2026-08-15"`),
			want:   true,
		},
		{
			name:   "expires in 30 days, outside the window",
			expiry: json.RawMessage(`"2026-09-11"`),
			want:   false,
		},
		{
			name:   "already expired (in the past) — not a future warning",
			expiry: json.RawMessage(`"2026-08-01"`),
			want:   false,
		},
		{
			name:   "unparseable shape — fails silently rather than guessing",
			expiry: json.RawMessage(`12345`),
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
}

// TestIngestAbortsWithDistinctErrorOn401 covers stage 1's other failure
// mode: the cookie itself is rejected outright. This must produce a
// different error from the "no paid access" case — the two point the
// operator at different fixes (refresh the cookie vs. actually subscribe).
func TestIngestAbortsWithDistinctErrorOn401(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.subscriptionStatus = http.StatusUnauthorized

	importer := newTestImporter(t, fake.Server, nil)
	logger := testLogger(&strings.Builder{})

	_, _, err := importer.Ingest(context.Background(), logger)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "dead or malformed") {
		t.Errorf("error = %q, want it to say the cookie is dead or malformed", err.Error())
	}
	if strings.Contains(err.Error(), "no paid access") {
		t.Errorf("error = %q, must not be conflated with the 'no paid access' diagnosis", err.Error())
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
