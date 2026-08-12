package substack

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPostHonoursCache pre-seeds CacheDir with one free cached post and one
// paid cached post written under an unconfirmed (Authenticated: false)
// session, then runs Ingest over an archive naming both plus a paid post
// that must succeed its own session canary. The free one must be served
// from cache with no network request; the unconfirmed paid one, despite
// being "cached", cannot be trusted — its own doc comment on post explains
// why — and must be re-fetched.
func TestPostHonoursCache(t *testing.T) {
	fake := newFakeSubstack(t)
	// canary-target listed before unconfirmed-cached deliberately:
	// verifySession (stage 2) targets the *first* only_paid post the
	// archive names, and its own two probe fetches would otherwise land on
	// unconfirmed-cached instead — which would still end up correctly
	// re-fetched, but for the wrong reason, and this test's "exactly one
	// network request" assertion below is specifically about post's own
	// cache-skip rule, not about which slug verifySession happened to pick.
	fake.archivePages[0] = []archiveFixture{
		newArchiveFixture(1, "free-cached", "newsletter", "everyone"),
		newArchiveFixture(2, "canary-target", "newsletter", "only_paid"),
		newArchiveFixture(3, "unconfirmed-cached", "newsletter", "only_paid"),
	}
	fake.posts["canary-target"] = newWorkingPaidPostFixture(2, "canary-target")
	fake.posts["unconfirmed-cached"] = newWorkingPaidPostFixture(3, "unconfirmed-cached")

	importer := newTestImporter(t, fake.Server, nil)

	seedCache(t, importer, "free-cached", newFreePostFixture(1, "free-cached"), true)
	// Authenticated: false models a file left behind by a run made before
	// the session was ever confirmed to work — or one written before the
	// cacheEntry wrapper existed at all, which decodes the same way.
	seedCache(t, importer, "unconfirmed-cached", newWorkingPaidPostFixture(2, "unconfirmed-cached"), false)

	logger := testLogger(&strings.Builder{})
	documents, result, err := importer.Ingest(context.Background(), logger)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if result.Cached != 1 {
		t.Errorf("Result.Cached = %d, want 1 (only free-cached)", result.Cached)
	}
	// Fetched: unconfirmed-cached (re-fetched despite being on disk) and
	// canary-target (never cached at all). verifySession's own two probe
	// requests against canary-target do not count here — they go through
	// fetchRaw directly, not post, and post's own single confirming fetch
	// for canary-target is what Result.Fetched counts.
	if result.Fetched != 2 {
		t.Errorf("Result.Fetched = %d, want 2 (unconfirmed-cached refetched, canary-target fetched)", result.Fetched)
	}
	if got := fake.postRequestCount("free-cached"); got != 0 {
		t.Errorf("free-cached was requested %d times over the network, want 0", got)
	}
	if got := fake.postRequestCount("unconfirmed-cached"); got != 1 {
		t.Errorf("unconfirmed-cached was requested %d times over the network, want exactly 1", got)
	}
	if len(documents) != 3 {
		t.Fatalf("len(documents) = %d, want 3", len(documents))
	}

	// The refetched file must now be readable back as Authenticated: true —
	// otherwise every subsequent run would refetch it forever even though
	// the session verified as working this time.
	raw, err := os.ReadFile(importer.cachePath("unconfirmed-cached"))
	if err != nil {
		t.Fatalf("read refreshed cache file: %v", err)
	}
	var entry cacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("decode refreshed cache file: %v", err)
	}
	if !entry.Authenticated {
		t.Error("refetched cache entry has Authenticated = false, want true")
	}
}

// seedCache writes fixture directly to where cachePath says slug's cache
// file belongs, in the cacheEntry wrapper post itself writes, bypassing the
// network entirely — simulating a file left behind by an earlier run.
func seedCache(t *testing.T, importer *Importer, slug string, fixture postFixture, authenticated bool) {
	t.Helper()
	path := importer.cachePath(slug)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("seed cache dir: %v", err)
	}
	postRaw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	entry := cacheEntry{FetchedAt: time.Now(), Authenticated: authenticated, Post: postRaw}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal cache entry: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("seed cache file: %v", err)
	}
}

// TestPostTrustsPreWrapperCacheFileAsMissing checks the migration path
// loadCachedPost's own doc comment describes: a cache file written by a
// version of this package that predates the cacheEntry wrapper (Substack's
// raw post JSON stored bare, with no "post" key to unwrap) must not be
// mistaken for a valid, confirmed-authenticated entry. It should instead be
// treated the same as no cache at all, triggering a normal fetch.
func TestPostTrustsPreWrapperCacheFileAsMissing(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.posts["legacy"] = newFreePostFixture(1, "legacy")

	importer := newTestImporter(t, fake.Server, nil)

	path := importer.cachePath("legacy")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("seed cache dir: %v", err)
	}
	// The bare, pre-wrapper shape: Substack's own post JSON with no
	// enclosing {"post": ...}.
	bareRaw, err := json.Marshal(newFreePostFixture(1, "legacy"))
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, bareRaw, 0o644); err != nil {
		t.Fatalf("seed cache file: %v", err)
	}

	_, fromCache, err := importer.post(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if fromCache {
		t.Error("fromCache = true, want false (a pre-wrapper file must not be trusted as-is)")
	}
	if got := fake.postRequestCount("legacy"); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

// TestIngestAbortsWhenSessionVerificationFails covers the canary's failure
// path: when the authenticated and unauthenticated fetches of the probe
// post come back indistinguishable (the lapsed-session case
// newLapsedPaidPostFixture models), Ingest must abort the entire run before
// fetching anything else — not just skip the probe post and continue with
// the rest of the archive.
func TestIngestAbortsWhenSessionVerificationFails(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.archivePages[0] = []archiveFixture{
		newArchiveFixture(1, "free-post", "newsletter", "everyone"),
		newArchiveFixture(2, "lapsed-probe", "newsletter", "only_paid"),
		newArchiveFixture(3, "another-free-post", "newsletter", "everyone"),
	}
	fake.posts["free-post"] = newFreePostFixture(1, "free-post")
	fake.posts["lapsed-probe"] = newLapsedPaidPostFixture(2, "lapsed-probe")
	fake.posts["another-free-post"] = newFreePostFixture(3, "another-free-post")

	importer := newTestImporter(t, fake.Server, nil)
	logger := testLogger(&strings.Builder{})

	documents, _, err := importer.Ingest(context.Background(), logger)
	if err == nil {
		t.Fatal("expected an error aborting the run")
	}
	if !strings.Contains(err.Error(), "still returned what looks like a preview") {
		t.Errorf("error = %q, want it to name the stage 2 canary failure", err.Error())
	}
	if len(documents) != 0 {
		t.Errorf("len(documents) = %d, want 0 (nothing imported on a failed verification)", len(documents))
	}

	// The probe post is fetched exactly twice — once with the cookie, once
	// without, both via verifySession directly — and nothing else in the
	// archive is ever requested at all: the abort happens before the
	// per-post loop even starts.
	if got := fake.postRequestCount("lapsed-probe"); got != 2 {
		t.Errorf("lapsed-probe requested %d times, want 2 (authenticated + anonymous canary)", got)
	}
	for _, slug := range []string{"free-post", "another-free-post"} {
		if got := fake.postRequestCount(slug); got != 0 {
			t.Errorf("%s requested %d times, want 0 (the whole run must abort before reaching it)", slug, got)
		}
	}
}

// TestIngestSkipsSessionVerificationWithNoPaidPost checks the other edge:
// an archive containing no only_paid post at all has nothing to verify the
// cookie against, and Ingest must proceed rather than erroring or hanging.
func TestIngestSkipsSessionVerificationWithNoPaidPost(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.archivePages[0] = []archiveFixture{
		newArchiveFixture(1, "only-free-post", "newsletter", "everyone"),
	}
	fake.posts["only-free-post"] = newFreePostFixture(1, "only-free-post")

	importer := newTestImporter(t, fake.Server, nil)
	logger := testLogger(&strings.Builder{})

	documents, _, err := importer.Ingest(context.Background(), logger)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(documents) != 1 {
		t.Errorf("len(documents) = %d, want 1", len(documents))
	}
}

// TestFetchRetriesAfter429ThenSucceeds covers getJSON/fetchRaw's retry path:
// a 429 on the first attempt must not fail the request outright, and the
// eventual 200 must be the one that decides the result.
func TestFetchRetriesAfter429ThenSucceeds(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.archivePages[0] = []archiveFixture{
		newArchiveFixture(1, "flaky", "newsletter", "everyone"),
	}
	fake.posts["flaky"] = newFreePostFixture(1, "flaky")
	fake.postStatus["flaky"] = []int{http.StatusTooManyRequests, http.StatusOK}

	importer := newTestImporter(t, fake.Server, nil)

	body, fromCache, err := importer.post(context.Background(), "flaky")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if fromCache {
		t.Error("fromCache = true, want false (this was never cached)")
	}
	if body.ID != 1 {
		t.Errorf("body.ID = %d, want 1", body.ID)
	}
	if got := fake.postRequestCount("flaky"); got != 2 {
		t.Errorf("requests = %d, want 2 (one 429, one success)", got)
	}
}

// TestFetchGivesUpAfterMaxAttempts covers the other half: a slug that keeps
// failing with a retryable status must stop being retried once MaxAttempts
// is spent, and report an error rather than hang or retry forever.
func TestFetchGivesUpAfterMaxAttempts(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.postStatus["always-500"] = []int{http.StatusInternalServerError}

	importer := newTestImporter(t, fake.Server, func(cfg *Config) {
		cfg.MaxAttempts = 3
	})

	_, _, err := importer.post(context.Background(), "always-500")
	if err == nil {
		t.Fatal("expected an error after exhausting MaxAttempts")
	}
	if got := fake.postRequestCount("always-500"); got != 3 {
		t.Errorf("requests = %d, want exactly MaxAttempts (3)", got)
	}
}

// TestPostRejectsPathTraversalSlug pins validateSlug's job: an archive
// listing is untrusted input, and a slug built to escape CacheDir via "../"
// or an embedded "/" must be rejected outright, before any file I/O or
// network request is attempted.
func TestPostRejectsPathTraversalSlug(t *testing.T) {
	fake := newFakeSubstack(t)
	importer := newTestImporter(t, fake.Server, nil)

	for _, slug := range []string{
		"../../etc/passwd",
		"foo/../../bar",
		"nested/slug",
		"",
	} {
		t.Run(slug, func(t *testing.T) {
			_, _, err := importer.post(context.Background(), slug)
			if err == nil {
				t.Fatalf("post(%q): expected an error, got none", slug)
			}
			if got := fake.postRequestCount(slug); got != 0 {
				t.Errorf("post(%q) made %d network requests, want 0", slug, got)
			}
		})
	}

	// cachePath itself is a second, independent line of defense: even
	// called directly with something unsafe, it must never produce a path
	// outside CacheDir/Host.
	path := importer.cachePath("../../../etc/passwd")
	if strings.Contains(path, "..") {
		t.Errorf("cachePath(%q) = %q, still contains \"..\"", "../../../etc/passwd", path)
	}
	cacheRoot := filepath.Join(importer.cfg.CacheDir, importer.cfg.Host)
	if !strings.HasPrefix(path, cacheRoot) {
		t.Errorf("cachePath escaped CacheDir/Host: got %q, want a prefix of %q", path, cacheRoot)
	}
}

// TestPostDateParsesFractionalSecondsUTC pins that postBody's post_date
// field decodes Substack's real timestamp shape correctly: RFC3339 with
// millisecond fractional seconds and a bare "Z" for UTC, e.g.
// "2026-08-02T16:27:14.364Z" — confirmed as the live API's actual format on
// 2026-08-12, not RFC3339's own reference layout (which has no fractional
// seconds field at all).
//
// Go note: this works without any custom UnmarshalJSON because time.Parse
// (which encoding/json's time.Time.UnmarshalJSON calls with the RFC3339
// layout) has a documented special case: a fractional-second component in
// the input is accepted even when the layout string given to Parse does not
// itself mention one. wallabag.Time, by contrast, needs a custom
// UnmarshalJSON (see internal/wallabag/types.go) because wallabag's own
// timestamp omits the colon in its zone offset, which has no such
// leniency — a good reminder that this shape working "for free" is Go's
// parser being specifically forgiving about fractional seconds, not a
// general amnesty for any deviation from the reference layout.
func TestPostDateParsesFractionalSecondsUTC(t *testing.T) {
	raw := `{"post_date": "2026-08-02T16:27:14.364Z"}`

	var p postBody
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := time.Date(2026, 8, 2, 16, 27, 14, 364_000_000, time.UTC)
	if !p.PostDate.Equal(want) {
		t.Errorf("PostDate = %v, want %v", p.PostDate, want)
	}

	// archivePost carries the same field with the same json tag and no
	// custom decoding of its own, so the same parse behaviour applies there
	// too — checked directly rather than assumed, since archivePost and
	// postBody are two separate types that happen to agree, not one type
	// used in two places.
	var a archivePost
	rawArchive := `{"post_date": "2026-08-02T16:27:14.364Z"}`
	if err := json.Unmarshal([]byte(rawArchive), &a); err != nil {
		t.Fatalf("unmarshal archivePost: %v", err)
	}
	if !a.PostDate.Equal(want) {
		t.Errorf("archivePost.PostDate = %v, want %v", a.PostDate, want)
	}
}
