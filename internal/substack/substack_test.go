package substack

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testHost is a fixed timestamp used across fixtures so tests reading it
// back do not have to special-case comparing against time.Now().
var testPostDate = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// newTestImporter builds an Importer pointed at server, with SessionID set
// to a value tests can grep the whole log/error/Result output for.
//
// Go note: httptest.NewTLSServer, not httptest.NewServer. fetchRaw always
// builds its endpoint as "https://" + Host + path — that is the real
// contract with Substack, and the test deliberately does not special-case
// production code to accept a bare http:// test server, since that would
// mean production and test code taking different paths through fetchRaw.
// httptest.NewTLSServer starts a real TLS listener with a self-signed
// certificate, and Server.Client() returns an *http.Client already
// configured to trust that certificate — the standard way to exercise HTTPS
// client code against a local fake without weakening what the client itself
// does.
func newTestImporter(t *testing.T, server *httptest.Server, tweak func(*Config)) *Importer {
	t.Helper()

	host := strings.TrimPrefix(server.URL, "https://")
	cfg := Config{
		Host: host,
		// The "s%3A" prefix is not decorative — New rejects a SessionID
		// missing it (see validSessionIDPrefix in session.go), so every
		// test value needs it too, not just a real cookie.
		SessionID:   "s%3As3cr3t-session-cookie.sig",
		CacheDir:    t.TempDir(),
		RequestGap:  time.Millisecond, // tests should not spend real seconds throttling
		MaxAttempts: 3,
		HTTPClient:  server.Client(),
	}
	if tweak != nil {
		tweak(&cfg)
	}

	importer, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return importer
}

// archiveFixture is one entry as the fake /api/v1/archive endpoint encodes
// it, matching archivePost's own JSON tags.
type archiveFixture struct {
	ID       int    `json:"id"`
	Slug     string `json:"slug"`
	Type     string `json:"type"`
	Audience string `json:"audience"`
	Title    string `json:"title"`
	PostDate string `json:"post_date"`
}

func newArchiveFixture(id int, slug, typ, audience string) archiveFixture {
	return archiveFixture{
		ID:       id,
		Slug:     slug,
		Type:     typ,
		Audience: audience,
		Title:    "Post " + strconv.Itoa(id),
		PostDate: testPostDate.Format(time.RFC3339),
	}
}

// postFixture is one entry as the fake /api/v1/posts/{slug} endpoint
// encodes it, matching postBody's own JSON tags.
//
// PreviewBodyHTML is not a real Substack field — postBody has no such
// tag, and it is never sent to json.Marshal for the wire response (see
// fakeSubstack.handlePost). It exists purely so the fake server can behave
// the way the real one does for a paid post: serve the full body_html when
// the request carries a cookie, and a shorter, genuinely different
// body_html when it does not — the exact differential verifySession's
// canary depends on. Left empty, a fixture serves BodyHTML regardless of
// the request's cookie, matching a real free post's own behaviour (nothing
// to gate on audience "everyone").
type postFixture struct {
	ID               int      `json:"id"`
	Slug             string   `json:"slug"`
	Type             string   `json:"type"`
	Audience         string   `json:"audience"`
	Title            string   `json:"title"`
	CanonicalURL     string   `json:"canonical_url"`
	PostDate         string   `json:"post_date"`
	BodyHTML         string   `json:"body_html"`
	Language         string   `json:"language"`
	PublishedBylines []byline `json:"publishedBylines"`

	PreviewBodyHTML string `json:"-"`
}

// paidBodyPadding pads a fixture's authenticated body_html well past
// sessionCanaryMinRatio's threshold relative to a short preview, so a test
// asserting the canary passes is not accidentally sensitive to the exact
// ratio picked.
const paidBodyPadding = "Real paid content that only a working session should ever see. "

func newFreePostFixture(id int, slug string) postFixture {
	return postFixture{
		ID:           id,
		Slug:         slug,
		Type:         "newsletter",
		Audience:     "everyone",
		Title:        "Post " + strconv.Itoa(id),
		CanonicalURL: "https://example.substack.com/p/" + slug,
		PostDate:     testPostDate.Format(time.RFC3339),
		BodyHTML:     "<p>This is the full body of a free post, long enough not to look like a preview by accident.</p>",
		Language:     "en",
		PublishedBylines: []byline{
			{Name: "Some Author"},
		},
	}
}

// newPaidPostFixture builds an only_paid post whose fake server response
// differs by whether the request is authenticated: fullBodyHTML with a
// cookie, previewBodyHTML without one — modelling a genuinely working
// session, where verifySession's canary should pass.
func newPaidPostFixture(id int, slug, fullBodyHTML, previewBodyHTML string) postFixture {
	return postFixture{
		ID:              id,
		Slug:            slug,
		Type:            "newsletter",
		Audience:        "only_paid",
		Title:           "Post " + strconv.Itoa(id),
		CanonicalURL:    "https://example.substack.com/p/" + slug,
		PostDate:        testPostDate.Format(time.RFC3339),
		BodyHTML:        fullBodyHTML,
		PreviewBodyHTML: previewBodyHTML,
		Language:        "en",
	}
}

// newWorkingPaidPostFixture is newPaidPostFixture with a generic,
// comfortably-larger-than-preview full body — the common case for tests
// that just need "a paid post whose session canary passes" without caring
// about the exact content.
func newWorkingPaidPostFixture(id int, slug string) postFixture {
	return newPaidPostFixture(id, slug,
		"<p>"+strings.Repeat(paidBodyPadding, 20)+"</p>",
		"<p>A short teaser visible to anyone.</p>",
	)
}

// newLapsedPaidPostFixture models a paid post fetched under a dead session:
// Substack cannot tell an expired cookie from no cookie at all, so both the
// "authenticated" and anonymous fetch get the same short preview — the
// scenario verifySession's canary must catch and abort on.
func newLapsedPaidPostFixture(id int, slug string) postFixture {
	preview := "<p>A short teaser visible to anyone, cookie or not.</p>"
	return newPaidPostFixture(id, slug, preview, preview)
}

// subscriptionFixture is the fake /api/v1/subscription endpoint's response
// shape, matching subscriptionState's own JSON tags but with plain `any`
// fields for Type/Expiry/BundleID rather than json.RawMessage — ergonomic
// for a test to set directly (nil, a string, whatever a given test needs),
// where subscriptionState itself deliberately does not commit to a concrete
// type (see subscriptionState's own doc comment in session.go for why).
type subscriptionFixture struct {
	MembershipState  string `json:"membership_state"`
	IsFreeSubscribed bool   `json:"is_free_subscribed"`
	IsSubscribed     bool   `json:"is_subscribed"`
	Type             any    `json:"type"`
	Expiry           any    `json:"expiry"`
	IsFounding       bool   `json:"is_founding"`
	BundleID         any    `json:"bundle_id"`
}

// workingSubscriptionFixture is the default fixture newFakeSubstack installs
// — a state that passes stage 1 (verifySubscriptionState) cleanly, with no
// expiry warning — so a test that has no interest in the subscription check
// itself never has to think about it. Its exact field values are arbitrary
// placeholders: what a real paid response actually contains was not
// confirmed live (see subscriptionState's own doc comment), so this only
// needs to be "clearly not free_signup, and not IsFreeSubscribed with
// everything else null" — the one thing looksFree actually checks.
func workingSubscriptionFixture() subscriptionFixture {
	return subscriptionFixture{
		MembershipState:  "subscribed",
		IsFreeSubscribed: false,
		IsSubscribed:     true,
		Type:             "paid",
		Expiry:           nil,
		IsFounding:       false,
		BundleID:         nil,
	}
}

// freeSubscriptionFixture models the one shape actually confirmed live: a
// free subscriber's own /api/v1/subscription response.
func freeSubscriptionFixture() subscriptionFixture {
	return subscriptionFixture{
		MembershipState:  "free_signup",
		IsFreeSubscribed: true,
		IsSubscribed:     false,
		Type:             nil,
		Expiry:           nil,
		IsFounding:       false,
		BundleID:         nil,
	}
}

// fakeSubstack stands in for a Substack publication's API. It serves
// /api/v1/archive from archivePages (one slice of fixtures per requested
// page, indexed by offset/limit), /api/v1/posts/{slug} from posts, and
// /api/v1/subscription from subscription, recording every request path so
// tests can assert on what was actually requested rather than only on the
// final result.
type fakeSubstack struct {
	*httptest.Server

	mu sync.Mutex

	// archivePages maps offset to the page fixture serves at that offset.
	// A missing offset serves an empty page. Ignored when archiveGenerator
	// is set.
	archivePages map[int][]archiveFixture

	// archiveGenerator, when set, computes the page for any offset instead
	// of looking it up in archivePages — used by the "novel ids forever"
	// test, which needs an archive that never runs out on its own so only
	// maxArchiveOffset can stop it.
	archiveGenerator func(offset int) []archiveFixture

	// posts maps slug to the fixture serves for that slug. A missing slug
	// serves 404.
	posts map[string]postFixture

	// postStatus overrides the status code served for a slug, for the
	// retry/backoff tests. Consumed one at a time: each request pops the
	// next status off the slice, and the last entry repeats once exhausted.
	postStatus map[string][]int

	// subscription and subscriptionStatus control the /api/v1/subscription
	// response — see workingSubscriptionFixture for the default, which
	// keeps stage 1 quietly out of the way of tests that are not about it.
	// subscriptionStatus of 0 means 200; set it to force a status (401, for
	// the "dead cookie" stage-1 test).
	subscription       subscriptionFixture
	subscriptionStatus int

	requestedPaths []string
	postRequests   []string
	sessionCookies []string
}

// newFakeSubstack starts a fake publication server. archivePages and posts
// may be filled in on the returned value before the first request; both are
// read under fake.mu so a test can also mutate them (e.g. postStatus) from
// its own goroutine-free setup code before Ingest runs.
func newFakeSubstack(t *testing.T) *fakeSubstack {
	t.Helper()

	fake := &fakeSubstack{
		archivePages: make(map[int][]archiveFixture),
		posts:        make(map[string]postFixture),
		postStatus:   make(map[string][]int),
		subscription: workingSubscriptionFixture(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/archive", fake.handleArchive)
	mux.HandleFunc("/api/v1/posts/", fake.handlePost)
	mux.HandleFunc("/api/v1/subscription", fake.handleSubscription)

	fake.Server = httptest.NewTLSServer(mux)
	t.Cleanup(fake.Close)
	return fake
}

func (f *fakeSubstack) handleSubscription(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requestedPaths = append(f.requestedPaths, r.URL.RequestURI())
	f.sessionCookies = append(f.sessionCookies, r.Header.Get("Cookie"))
	status := f.subscriptionStatus
	body := f.subscription
	f.mu.Unlock()

	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	json.NewEncoder(w).Encode(body)
}

func (f *fakeSubstack) handleArchive(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requestedPaths = append(f.requestedPaths, r.URL.RequestURI())
	f.sessionCookies = append(f.sessionCookies, r.Header.Get("Cookie"))

	// limit must always be archivePageSize: a larger value is documented in
	// archive.go as returning inconsistent results, so a test double that
	// silently accepted anything else would let a regression there go
	// unnoticed.
	if got := r.URL.Query().Get("limit"); got != strconv.Itoa(archivePageSize) {
		f.mu.Unlock()
		http.Error(w, fmt.Sprintf("unexpected limit %q", got), http.StatusBadRequest)
		return
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	generator := f.archiveGenerator
	page := f.archivePages[offset]
	f.mu.Unlock()

	if generator != nil {
		page = generator(offset)
	}
	json.NewEncoder(w).Encode(page)
}

func (f *fakeSubstack) handlePost(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/api/v1/posts/")
	authenticated := r.Header.Get("Cookie") != ""

	f.mu.Lock()
	f.requestedPaths = append(f.requestedPaths, r.URL.RequestURI())
	f.postRequests = append(f.postRequests, slug)
	f.sessionCookies = append(f.sessionCookies, r.Header.Get("Cookie"))

	if statuses, ok := f.postStatus[slug]; ok && len(statuses) > 0 {
		status := statuses[0]
		if len(statuses) > 1 {
			f.postStatus[slug] = statuses[1:]
		}
		f.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		// status == 200 falls through to serving the real fixture below.
		f.mu.Lock()
	}

	post, ok := f.posts[slug]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	// A real Substack server serves less content to an unauthenticated
	// request for a paid post — that differential is exactly what
	// verifySession's canary depends on. PreviewBodyHTML models that; left
	// empty (the ordinary case for a free post fixture), BodyHTML is served
	// either way, matching a real free post's own behaviour.
	if !authenticated && post.PreviewBodyHTML != "" {
		post.BodyHTML = post.PreviewBodyHTML
	}
	json.NewEncoder(w).Encode(post)
}

// postRequestCount returns how many times /api/v1/posts/{slug} was
// requested for slug.
func (f *fakeSubstack) postRequestCount(slug string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, s := range f.postRequests {
		if s == slug {
			count++
		}
	}
	return count
}

// testLogger returns a logger that writes to buf, so a test can assert on
// what was — or, for the secrecy tests, was never — narrated.
func testLogger(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
