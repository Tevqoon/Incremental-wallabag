package substack

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// testSessionID is deliberately distinctive — unlikely to appear anywhere
// by coincidence — so grepping for it in log output, error strings, or a
// %v dump is a meaningful test rather than a search for a common substring.
// Carries the "s%3A" prefix New now requires (see validSessionIDPrefix in
// session.go), so it stays usable as a real Config.SessionID value.
const testSessionID = "s%3AsEcReT-sess10n-c00k1e-do-not-leak-me.sig"

// TestConfigStringRedactsSessionID pins Config.String(): the whole reason it
// exists is so an accidental %v of a Config never shows the cookie.
func TestConfigStringRedactsSessionID(t *testing.T) {
	cfg := Config{
		Host:      "example.substack.com",
		SessionID: testSessionID,
		CacheDir:  "/tmp/cache",
	}

	for _, rendered := range []string{
		cfg.String(),
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%s", cfg),
		fmt.Sprint(cfg),
	} {
		if strings.Contains(rendered, testSessionID) {
			t.Errorf("rendered Config leaked SessionID: %q", rendered)
		}
	}
}

// TestNewErrorsDoNotLeakSessionID checks that a validation failure in New —
// the one path guaranteed to run before any network I/O, and the one most
// likely to get a full Config handed to fmt.Errorf by a careless future
// edit — never echoes the cookie back in its error message.
func TestNewErrorsDoNotLeakSessionID(t *testing.T) {
	_, err := New(Config{SessionID: testSessionID}) // missing Host
	if err == nil {
		t.Fatal("expected an error for a missing Host")
	}
	if strings.Contains(err.Error(), testSessionID) {
		t.Errorf("New's error leaked SessionID: %q", err.Error())
	}
}

// TestIngestNeverLeaksSessionIDOnVerificationFailure runs a full Ingest
// whose session canary fails — the error path that builds its own message
// out of byte counts and a slug, the likeliest place a careless future edit
// would accidentally interpolate more than that — and checks the session
// cookie appears nowhere in the log output, the returned error, or a %+v
// dump of the Result.
func TestIngestNeverLeaksSessionIDOnVerificationFailure(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.archivePages[0] = []archiveFixture{
		newArchiveFixture(1, "lapsed-probe", "newsletter", "only_paid"),
	}
	fake.posts["lapsed-probe"] = newLapsedPaidPostFixture(1, "lapsed-probe")

	importer := newTestImporter(t, fake.Server, func(cfg *Config) {
		cfg.SessionID = testSessionID
	})

	var logBuf strings.Builder
	logger := testLogger(&logBuf)

	_, result, err := importer.Ingest(context.Background(), logger)
	if err == nil {
		t.Fatal("expected a session verification error")
	}

	assertNoSessionIDLeak(t, logBuf.String(), err, result, fake)
}

// TestIngestNeverLeaksSessionIDOnFetchFailure covers the other route a
// cookie could leak through: a per-post fetch failure, which logs its own
// "skipping a post" warning and records the error text in Result.Warnings.
// Session verification succeeds first here, so the run reaches the per-post
// loop at all.
func TestIngestNeverLeaksSessionIDOnFetchFailure(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.archivePages[0] = []archiveFixture{
		newArchiveFixture(1, "canary-target", "newsletter", "only_paid"),
		newArchiveFixture(2, "will-500", "newsletter", "everyone"),
	}
	fake.posts["canary-target"] = newWorkingPaidPostFixture(1, "canary-target")
	fake.postStatus["will-500"] = []int{
		http.StatusInternalServerError,
		http.StatusInternalServerError,
		http.StatusInternalServerError,
	}

	importer := newTestImporter(t, fake.Server, func(cfg *Config) {
		cfg.SessionID = testSessionID
	})

	var logBuf strings.Builder
	logger := testLogger(&logBuf)

	_, result, err := importer.Ingest(context.Background(), logger)
	if err != nil {
		t.Fatalf("Ingest: %v (session verification should have succeeded here)", err)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("expected a warning recording will-500's fetch failure — the test setup itself is broken if there is none")
	}

	assertNoSessionIDLeak(t, logBuf.String(), err, result, fake)
}

// assertNoSessionIDLeak is the shared assertion behind both leak tests
// above: the cookie must appear nowhere in increader's own log output,
// error surfaces, or Result — but it must have appeared on the wire, or the
// test setup itself proves nothing.
func assertNoSessionIDLeak(t *testing.T, logOutput string, err error, result Result, fake *fakeSubstack) {
	t.Helper()

	if strings.Contains(logOutput, testSessionID) {
		t.Errorf("log output leaked SessionID:\n%s", logOutput)
	}
	if err != nil && strings.Contains(err.Error(), testSessionID) {
		t.Errorf("Ingest error leaked SessionID: %q", err.Error())
	}
	resultDump := fmt.Sprintf("%+v", result)
	if strings.Contains(resultDump, testSessionID) {
		t.Errorf("Result leaked SessionID: %q", resultDump)
	}
	for _, w := range result.Warnings {
		if strings.Contains(w, testSessionID) {
			t.Errorf("Result.Warnings leaked SessionID: %q", w)
		}
	}

	// The fake server itself received the cookie, of course — that is the
	// entire point of sending it. This is not a contradiction: the boundary
	// these tests care about is increader's own logs and error surfaces,
	// not the wire, where the cookie has to travel to be useful at all.
	sawCookie := false
	for _, cookie := range fake.sessionCookies {
		if strings.Contains(cookie, testSessionID) {
			sawCookie = true
		}
	}
	if !sawCookie {
		t.Error("the fake server never saw the session cookie at all — the test setup itself is broken")
	}
}
