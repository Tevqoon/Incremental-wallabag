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
		Host:        host,
		SessionID:   "s3cr3t-session-cookie",
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
}

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

func newPaywalledPostFixture(id int, slug string) postFixture {
	return postFixture{
		ID:           id,
		Slug:         slug,
		Type:         "newsletter",
		Audience:     "only_paid",
		Title:        "Post " + strconv.Itoa(id),
		CanonicalURL: "https://example.substack.com/p/" + slug,
		PostDate:     testPostDate.Format(time.RFC3339),
		BodyHTML:     `<p>A short teaser.</p><div class="paywall"><p>Subscribe to keep reading.</p></div>`,
		Language:     "en",
	}
}

// fakeSubstack stands in for a Substack publication's API. It serves
// /api/v1/archive from archivePages (one slice of fixtures per requested
// page, indexed by offset/limit) and /api/v1/posts/{slug} from posts,
// recording every request path so tests can assert on what was actually
// requested rather than only on the final result.
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
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/archive", fake.handleArchive)
	mux.HandleFunc("/api/v1/posts/", fake.handlePost)

	fake.Server = httptest.NewTLSServer(mux)
	t.Cleanup(fake.Close)
	return fake
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
