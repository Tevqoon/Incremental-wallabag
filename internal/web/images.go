package web

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/store"
)

const (
	// maxImageBytes caps a single fetched image. Generous for an article
	// illustration, small enough that one hostile or misbehaving image
	// cannot balloon the database.
	maxImageBytes = 15 << 20

	imageFetchTimeout = 15 * time.Second
)

// resolveImages fetches and caches every image an article references, and
// returns a map from each image's original Src (see ir.Image) to a local URL
// safe to load it from — the shape ir.RenderOptions.ImageURLs expects.
//
// An image already cached, success or failure alike, costs one lookup and no
// network request — see store.DocumentImage.OK. A fresh miss is fetched here,
// synchronously, the same way an article's body is fetched on first open:
// the cost lands once, on whoever opens the article first, never again after.
func (s *Server) resolveImages(ctx context.Context, documentID int64, images []ir.Image) map[string]string {
	resolved := make(map[string]string, len(images))

	for _, image := range images {
		if image.Src == "" {
			continue
		}
		if _, done := resolved[image.Src]; done {
			continue
		}

		cached, found, err := s.store.CachedImage(documentID, image.Src)
		if err != nil {
			s.logger.Error("could not read cached image",
				"document", documentID, "url", image.Src, "error", err)
			continue
		}

		if !found {
			cached, err = s.fetchAndCacheImage(ctx, documentID, image.Src)
			if err != nil {
				s.logger.Warn("could not fetch article image",
					"document", documentID, "url", image.Src, "error", err)
				continue
			}
		}

		if !cached.OK {
			continue
		}
		resolved[image.Src] = fmt.Sprintf("/documents/%d/images/%d", documentID, cached.ID)
	}
	return resolved
}

// fetchAndCacheImage fetches one image and records the outcome, success or
// failure alike, so a later render never re-fetches it — see
// store.DocumentImage.OK.
func (s *Server) fetchAndCacheImage(ctx context.Context, documentID int64, src string) (store.DocumentImage, error) {
	data, contentType, fetchErr := fetchImage(ctx, src)
	ok := fetchErr == nil

	id, err := s.store.SaveDocumentImage(documentID, src, contentType, data, ok, time.Now())
	if err != nil {
		return store.DocumentImage{}, fmt.Errorf("web: cache image %q: %w", src, err)
	}
	if !ok {
		return store.DocumentImage{}, fetchErr
	}
	return store.DocumentImage{
		ID: id, DocumentID: documentID, URL: src,
		ContentType: contentType, Data: data, OK: true,
	}, nil
}

// fetchImage retrieves one image over HTTP(S), guarding against being turned
// into a proxy onto the Tailscale network or the host's own loopback — see
// isDisallowedHost. The guard is applied at dial time via imageClient's
// transport, not just against the URL's nominal hostname, because checking
// the hostname alone and then letting the client resolve it again would let
// a hostile DNS answer something different the second time (DNS rebinding).
func fetchImage(ctx context.Context, rawURL string) (data []byte, contentType string, err error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse image url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", fmt.Errorf("image scheme %q is not allowed", parsed.Scheme)
	}

	ctx, cancel := context.WithTimeout(ctx, imageFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	// A browser reading the article would fetch exactly this image; Go's
	// default client identifies itself as "Go-http-client" instead, which
	// enough hosts reflexively block — as anti-hotlinking, not because this
	// request is any less legitimate than the browser's would be — that
	// leaving it off would silently lose real images.
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "image/*")

	resp, err := imageClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}

	contentType = resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		return nil, "", fmt.Errorf("content-type %q is not an image", contentType)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxImageBytes {
		return nil, "", fmt.Errorf("exceeds %d bytes", maxImageBytes)
	}
	return body, contentType, nil
}

// imageClient fetches article images. Its transport resolves and validates
// the destination itself, at dial time, rather than trusting a check made
// earlier against the URL alone — see fetchImage and isDisallowedHost.
var imageClient = &http.Client{
	Timeout: imageFetchTimeout,
	Transport: &http.Transport{
		DialContext: dialPublicOnly,
	},
}

// dialPublicOnly resolves addr and refuses to connect if any resolved
// address is not a public one — loopback, private (RFC 1918), link-local, or
// the CGNAT range Tailscale itself uses (100.64.0.0/10, which is none of the
// above by the standard library's own reckoning).
//
// Applying this in DialContext rather than as a pre-flight check on the
// original URL is what makes it also cover every redirect hop: Go's
// http.Client calls DialContext again for each new host a redirect points
// at, so there is no separate place a check could be forgotten.
//
// Without this, increader would fetch an article's <img> tags from wherever
// they point — including, on a Tailscale-only deployment, other machines on
// that same private network. An article is untrusted content; this is what
// stops it from turning increader into a probe against its own network.
func dialPublicOnly(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("web: %s did not resolve", host)
	}
	for _, ip := range ips {
		if isDisallowedHost(ip) {
			return nil, fmt.Errorf("web: refusing to fetch from %s (%s): not a public address", host, ip)
		}
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

// cgnatRange is Tailscale's own address space (RFC 6598, "Shared Address
// Space" for carrier-grade NAT). net.IP has no built-in method for it —
// IsPrivate covers only RFC 1918 and RFC 4193 — so it is checked explicitly.
var cgnatRange = mustParseCIDR("100.64.0.0/10")

func mustParseCIDR(cidr string) *net.IPNet {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return network
}

// isDisallowedHost reports whether ip is not a public, external address —
// see dialPublicOnly.
func isDisallowedHost(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		cgnatRange.Contains(ip)
}

// handleDocumentImage serves one cached image — see resolveImages and
// migrations/009_document_images.sql.
//
// Caching is long and unconditional because a row, once written, never
// changes: it is fetched exactly once and kept forever, so there is nothing
// for a client to ever need to revalidate.
func (s *Server) handleDocumentImage(w http.ResponseWriter, r *http.Request) {
	documentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid document id", http.StatusBadRequest)
		return
	}
	imageID, err := strconv.ParseInt(r.PathValue("imageID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid image id", http.StatusBadRequest)
		return
	}

	image, err := s.store.DocumentImageByID(imageID)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}
	// The id in the URL is enough to serve the row on its own, but the
	// document id is checked too: an image belongs to the article that owns
	// it, not to whichever numeric id a client happens to try.
	if image.DocumentID != documentID || !image.OK {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", image.ContentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(image.Data)
}
