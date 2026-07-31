// Package wallabag is a client for the wallabag REST API, plus a thin adapter
// that exposes it as a source.Source.
//
// It knows nothing about incremental reading, storage, or HTTP serving. The
// only increader package it imports is internal/source, and only for the
// Document type it produces.
package wallabag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config holds the credentials for one wallabag account.
//
// Client ID and secret come from the wallabag web UI under
// Settings → API clients management → Create a new client. For the official
// hosted instance, URL is https://app.wallabag.it.
type Config struct {
	URL          string
	ClientID     string
	ClientSecret string
	Username     string
	Password     string
}

// Client talks to one wallabag instance. It is safe for concurrent use: the
// only mutable state is the cached OAuth token, guarded by a mutex in auth.go.
type Client struct {
	baseURL string
	cfg     Config
	http    *http.Client

	// tokenState carries everything the mutex protects. Grouping it in one
	// embedded struct keeps it obvious which fields the lock covers.
	tokens tokenState

	// version caches the server version string, guarded by tokens.mu alongside
	// the credentials rather than taking a second lock for one field.
	version string
}

// New validates the configuration and returns a ready client. It performs no
// network I/O; the first request authenticates lazily.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("wallabag: URL is required")
	}
	parsed, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("wallabag: invalid URL %q: %w", cfg.URL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("wallabag: URL %q must be http or https", cfg.URL)
	}
	for name, value := range map[string]string{
		"client_id":     cfg.ClientID,
		"client_secret": cfg.ClientSecret,
		"username":      cfg.Username,
		"password":      cfg.Password,
	} {
		if value == "" {
			return nil, fmt.Errorf("wallabag: %s is required", name)
		}
	}

	return &Client{
		baseURL: strings.TrimRight(cfg.URL, "/"),
		cfg:     cfg,
		// A default http.Client has no timeout at all, which means a hung
		// server hangs increader forever. Always set one.
		http: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// errUnauthorized signals an expired or rejected token, so get can refresh once
// and retry rather than failing the whole sync.
var errUnauthorized = errors.New("wallabag: unauthorized")

// get performs an authenticated GET and decodes the JSON body into out.
//
// It retries once after re-authenticating, because access tokens live an hour
// and a long sync can outlast one.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	err := c.getOnce(ctx, path, query, out)
	if errors.Is(err, errUnauthorized) {
		c.forgetTokens()
		err = c.getOnce(ctx, path, query, out)
	}
	return err
}

func (c *Client) getOnce(ctx context.Context, path string, query url.Values, out any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("wallabag: build request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("wallabag: GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errUnauthorized
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("wallabag: GET %s: %s: %s", path, resp.Status, errorBody(resp.Body))
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("wallabag: decode response from %s: %w", path, err)
	}
	return nil
}

// errorBody reads a bounded prefix of an error response for the error message.
// The limit matters: an HTML error page from a proxy could otherwise be
// megabytes of noise in a log line.
func errorBody(r io.Reader) string {
	body, err := io.ReadAll(io.LimitReader(r, 1024))
	if err != nil {
		return "<unreadable body>"
	}
	return strings.TrimSpace(string(body))
}

// annotationFilterMinor is the wallabag 2.x minor release that added the
// ?annotations=1 filter on entry listings.
const annotationFilterMinor = 6

// Version returns the wallabag server's version string, e.g. "2.6.14".
//
// The result is cached for the client's lifetime: it cannot change under a
// running server, and the sync path would otherwise ask on every pass.
func (c *Client) Version(ctx context.Context) (string, error) {
	c.tokens.mu.Lock()
	cached := c.version
	c.tokens.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	// The endpoint answers with a bare JSON string rather than an object.
	var version string
	if err := c.get(ctx, "/api/version.json", nil, &version); err != nil {
		return "", fmt.Errorf("wallabag: read server version: %w", err)
	}

	c.tokens.mu.Lock()
	c.version = version
	c.tokens.mu.Unlock()
	return version, nil
}

// SupportsAnnotationFilter reports whether the server can filter a listing down
// to annotated entries.
//
// A version that cannot be reached or parsed is treated as unsupported. That
// direction matters: assuming support on an old server would silently return
// the *whole* library from the annotated pass and import highlights for entries
// that have none, whereas assuming no support only falls back to the slower
// per-article path.
func (c *Client) SupportsAnnotationFilter(ctx context.Context) bool {
	version, err := c.Version(ctx)
	if err != nil {
		return false
	}

	parts := strings.SplitN(strings.Trim(version, `"`), ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}

	return major > 2 || (major == 2 && minor >= annotationFilterMinor)
}
