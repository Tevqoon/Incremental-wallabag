package wallabag

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// tokenPath is wallabag's OAuth2 token endpoint. The "v2" is part of the path,
// not a version of the OAuth protocol.
const tokenPath = "/oauth/v2/token"

// expiryMargin retires a token slightly before the server would, so a request
// cannot be sent with a token that expires in flight.
const expiryMargin = 60 * time.Second

// tokenState is the client's cached OAuth credentials. Every field is guarded
// by mu.
type tokenState struct {
	mu        sync.Mutex
	access    string
	refresh   string
	expiresAt time.Time
}

// tokenResponse is the JSON returned by the token endpoint for both the
// password and refresh_token grants.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// accessToken returns a valid bearer token, authenticating if necessary.
//
// wallabag uses the OAuth2 *password grant*: increader holds the username and
// password directly rather than going through a browser redirect. That is the
// only flow wallabag offers for API clients, and it is why the credentials live
// in the config file.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.tokens.mu.Lock()
	defer c.tokens.mu.Unlock()

	if c.tokens.access != "" && time.Now().Before(c.tokens.expiresAt) {
		return c.tokens.access, nil
	}

	// Prefer the refresh token: it avoids sending the password again. If the
	// refresh fails — it expires too, and a server restart can invalidate it —
	// fall through to a full password grant rather than failing the request.
	if c.tokens.refresh != "" {
		err := c.grantLocked(ctx, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {c.tokens.refresh},
			"client_id":     {c.cfg.ClientID},
			"client_secret": {c.cfg.ClientSecret},
		})
		if err == nil {
			return c.tokens.access, nil
		}
	}

	err := c.grantLocked(ctx, url.Values{
		"grant_type":    {"password"},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"username":      {c.cfg.Username},
		"password":      {c.cfg.Password},
	})
	if err != nil {
		return "", err
	}
	return c.tokens.access, nil
}

// grantLocked posts to the token endpoint and stores the result.
//
// The "Locked" suffix is a Go convention meaning: the caller must already hold
// the mutex. It is not enforced by the compiler, so the name is the contract.
func (c *Client) grantLocked(ctx context.Context, form url.Values) error {
	// The wallabag docs demonstrate this as a GET with credentials in the query
	// string, which leaks the password into server logs. A POST with a form
	// body is equivalent to the server and does not.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+tokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("wallabag: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("wallabag: token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Deliberately does not echo the form: it contains the password.
		return fmt.Errorf("wallabag: token request (%s grant): %s: %s",
			form.Get("grant_type"), resp.Status, errorBody(resp.Body))
	}

	var token tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return fmt.Errorf("wallabag: decode token response: %w", err)
	}
	if token.AccessToken == "" {
		return fmt.Errorf("wallabag: token response contained no access_token")
	}

	c.tokens.access = token.AccessToken
	c.tokens.refresh = token.RefreshToken
	c.tokens.expiresAt = time.Now().
		Add(time.Duration(token.ExpiresIn) * time.Second).
		Add(-expiryMargin)
	return nil
}

// forgetTokens drops the cached credentials so the next request re-authenticates.
// Called when the server rejects a token we believed was still valid.
func (c *Client) forgetTokens() {
	c.tokens.mu.Lock()
	defer c.tokens.mu.Unlock()
	c.tokens.access = ""
	c.tokens.expiresAt = time.Time{}
}
