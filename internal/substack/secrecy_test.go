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
const testSessionID = "sEcReT-sess10n-c00k1e-do-not-leak-me"

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

// TestIngestNeverLeaksSessionID runs a full Ingest — including the abort
// path, which builds its own error message, and a fetch failure, which logs
// a warning — and checks the session cookie appears nowhere in the log
// output, the returned error, or a %+v dump of the Result. This is the
// single most important test in this package per the brief it was built
// from: the cookie is the entire credential this package holds, and a leak
// anywhere here is a real secret ending up in a log file or an error
// surfaced to a UI.
func TestIngestNeverLeaksSessionID(t *testing.T) {
	fake := newFakeSubstack(t)

	var archive []archiveFixture
	for id := 1; id <= 4; id++ {
		slug := fmt.Sprintf("paid-%d", id)
		archive = append(archive, newArchiveFixture(id, slug, "newsletter", "only_paid"))
		fake.posts[slug] = newPaywalledPostFixture(id, slug)
	}
	// One slug that fails outright, to exercise the "skipping a post after
	// its fetch failed" log line and its Result.Warnings entry too.
	archive = append(archive, newArchiveFixture(5, "will-500", "newsletter", "only_paid"))
	fake.postStatus["will-500"] = []int{http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError}
	fake.archivePages[0] = archive

	importer := newTestImporter(t, fake.Server, func(cfg *Config) {
		cfg.SessionID = testSessionID
	})

	var logBuf strings.Builder
	logger := testLogger(&logBuf)

	_, result, err := importer.Ingest(context.Background(), logger)
	// An error is expected here (the paywall abort); what matters is that
	// nothing anywhere along the way carried the cookie.

	if strings.Contains(logBuf.String(), testSessionID) {
		t.Errorf("log output leaked SessionID:\n%s", logBuf.String())
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
	// this test cares about is increader's own logs and error surfaces, not
	// the wire, where the cookie has to travel to be useful at all.
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
