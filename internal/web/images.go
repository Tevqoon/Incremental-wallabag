package web

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	// Registered for image.DecodeConfig's side, not called directly: each
	// import's init() adds its format to the registry DecodeConfig consults,
	// the same mechanism database/sql uses for driver names. Without these
	// blank imports DecodeConfig recognises nothing at all and every image
	// would measure as 0x0 — see decodeImageDimensions.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/store"
)

const (
	// maxImageBytes caps a single fetched image. Generous for an article
	// illustration, small enough that one hostile or misbehaving image
	// cannot balloon the database.
	maxImageBytes = 15 << 20

	imageFetchTimeout = 15 * time.Second

	// imageResolveConcurrency caps how many images resolveImages fetches at
	// once. High enough that a handful of slow hosts do not queue up behind
	// each other, low enough that an article with dozens of images does not
	// open dozens of simultaneous connections out to dozens of — possibly
	// hostile — origins from inside a single page request.
	imageResolveConcurrency = 5

	// imageResolveBudget bounds the whole resolve step, not any single fetch
	// (imageFetchTimeout is that). Without an overall budget, an
	// image-heavy article on a slow host could keep a page request open for
	// minutes with nothing rendered — thirty images at up to 15s each, even
	// spread over imageResolveConcurrency workers, is still minutes. Past the
	// budget, whatever has not resolved yet is simply left out of this
	// render: it stays uncached and gets fetched properly the next time the
	// article is opened, which costs nothing extra — resolveImages already
	// treats "not yet cached" as the ordinary state of a fresh image, not a
	// failure, so there is nothing special to clean up.
	imageResolveBudget = 25 * time.Second
)

// resolveImages fetches and caches every image an article references, and
// returns a map from each image's original Src (see ir.Image) to somewhere
// safe to load it from, plus its intrinsic size — the shape
// ir.RenderOptions.ImageURLs expects.
//
// An image already cached, success or failure alike, costs one lookup and no
// network request — see store.DocumentImage.OK. A fresh miss is fetched, up
// to imageResolveConcurrency at a time, within imageResolveBudget for the
// whole set — see resolveOneImage and imageResolveBudget. Fetches run in
// parallel; the SQLite writes they produce do not — see saveMu below.
func (s *Server) resolveImages(ctx context.Context, documentID int64, images []ir.Image) map[string]ir.ResolvedImage {
	resolved := make(map[string]ir.ResolvedImage, len(images))

	// Dedupe by Src up front, exactly as the old sequential version did: the
	// same picture can appear twice in one article (a thumbnail and a
	// full-size copy of the same URL, say), and it must be fetched once, not
	// once per occurrence.
	seen := make(map[string]bool, len(images))
	srcs := make([]string, 0, len(images))
	for _, image := range images {
		if image.Src == "" || seen[image.Src] {
			continue
		}
		seen[image.Src] = true
		srcs = append(srcs, image.Src)
	}
	if len(srcs) == 0 {
		return resolved
	}

	ctx, cancel := context.WithTimeout(ctx, imageResolveBudget)
	defer cancel()

	var (
		resultsMu sync.Mutex
		saveMu    sync.Mutex // see resolveOneImage
		sem       = make(chan struct{}, imageResolveConcurrency)
		wg        sync.WaitGroup
	)

dispatch:
	for _, src := range srcs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// The budget is spent. Anything not already dispatched is left
			// unresolved for this render rather than queued to wait — see
			// imageResolveBudget.
			break dispatch
		}

		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			defer func() { <-sem }()

			image, ok := s.resolveOneImage(ctx, documentID, src, &saveMu)
			if !ok {
				return
			}
			resultsMu.Lock()
			resolved[src] = ir.ResolvedImage{
				URL:    fmt.Sprintf("/documents/%d/images/%d", documentID, image.ID),
				Width:  image.Width,
				Height: image.Height,
			}
			resultsMu.Unlock()
		}(src)
	}
	wg.Wait()

	return resolved
}

// resolveOneImage resolves a single image: a cache hit costs one lookup and
// no network request (see store.DocumentImage.OK); a miss is fetched and the
// outcome recorded before returning.
func (s *Server) resolveOneImage(ctx context.Context, documentID int64, src string, saveMu *sync.Mutex) (store.DocumentImage, bool) {
	cached, found, err := s.store.CachedImage(documentID, src)
	if err != nil {
		s.logger.Error("could not read cached image",
			"document", documentID, "url", src, "error", err)
		return store.DocumentImage{}, false
	}

	if !found {
		cached, err = s.fetchAndCacheImage(ctx, documentID, src, saveMu)
		if err != nil {
			s.logger.Warn("could not fetch article image",
				"document", documentID, "url", src, "error", err)
			return store.DocumentImage{}, false
		}
	} else if cached.OK && (cached.Width == 0 || cached.Height == 0) && len(cached.Data) > 0 {
		// A row cached before migrations/011_image_dimensions.sql existed —
		// or one whose fetch predates dimension tracking some other way —
		// keeps width=0/height=0 forever unless something measures it here:
		// fetchAndCacheImage above is the only caller of SaveDocumentImage,
		// and it only runs on a cache miss, so nothing ever re-fetches an
		// image just to measure it. The bytes needed for that measurement
		// are already sitting in Data, though, so there is nothing to fetch —
		// see backfillImageDimensions.
		cached = s.backfillImageDimensions(documentID, cached, saveMu)
	}

	if !cached.OK {
		return store.DocumentImage{}, false
	}
	return cached, true
}

// backfillImageDimensions measures an already-cached image from the bytes
// already stored for it and persists the result, so the cost is paid at most
// once per image rather than on every render that opens the article.
//
// A format decodeImageDimensions cannot parse (SVG, AVIF) reports 0,0 here,
// the same as it would on a fresh fetch — and that 0 is deliberately not
// persisted, since it would be recorded as a measurement rather than the
// absence of one. The result is that an unmeasurable image repeats this same
// cheap in-memory decode on every future open, rather than being marked
// "measured, no size" once and never checked again. A dedicated column for
// that ("tried and it's unmeasurable" vs "never tried") would tell the two
// apart, but nothing downstream needs that distinction, and a failed decode
// of bytes already sitting in memory costs microseconds — not worth a schema
// change to save.
func (s *Server) backfillImageDimensions(documentID int64, cached store.DocumentImage, saveMu *sync.Mutex) store.DocumentImage {
	width, height := decodeImageDimensions(cached.Data)
	if width == 0 || height == 0 {
		return cached
	}

	saveMu.Lock()
	err := s.store.SetDocumentImageDimensions(cached.ID, width, height)
	saveMu.Unlock()
	if err != nil {
		// The measurement is still good for this one render even if saving
		// it failed — only the next open pays the decode cost again, which
		// is why this does not also fail resolveOneImage.
		s.logger.Error("could not persist backfilled image dimensions",
			"document", documentID, "url", cached.URL, "error", err)
	}

	cached.Width, cached.Height = width, height
	return cached
}

// fetchAndCacheImage fetches one image and records the outcome, success or
// failure alike, so a later render never re-fetches it — see
// store.DocumentImage.OK.
//
// saveMu serializes this call across every concurrent caller in the same
// resolveImages: SQLite tolerates exactly one writer (see Store.Open, which
// caps the connection pool at one), so fetching images in parallel while
// letting every worker call SaveDocumentImage whenever its own fetch
// finishes would turn into concurrent writes racing for that one connection.
// The pool would still serialize them correctly, but only by accident of
// database/sql's own locking; taking the mutex here makes the constraint
// visible at the call site instead of resting on that implementation detail.
func (s *Server) fetchAndCacheImage(ctx context.Context, documentID int64, src string, saveMu *sync.Mutex) (store.DocumentImage, error) {
	data, contentType, fetchErr := fetchImage(ctx, src)
	ok := fetchErr == nil

	var width, height int
	if ok {
		width, height = decodeImageDimensions(data)
	}

	saveMu.Lock()
	id, err := s.store.SaveDocumentImage(documentID, src, contentType, data, ok, width, height, time.Now())
	saveMu.Unlock()
	if err != nil {
		return store.DocumentImage{}, fmt.Errorf("web: cache image %q: %w", src, err)
	}
	if !ok {
		return store.DocumentImage{}, fetchErr
	}
	return store.DocumentImage{
		ID: id, DocumentID: documentID, URL: src,
		ContentType: contentType, Data: data,
		Width: width, Height: height, OK: true,
	}, nil
}

// decodeImageDimensions reads an already-fetched image just far enough to
// learn its pixel size. image.DecodeConfig, unlike image.Decode, reads only
// the header — for every format registered above that is a few dozen bytes,
// not the whole image — so this costs nothing worth worrying about next to
// the fetch that just happened.
//
// A format DecodeConfig does not recognise is not an error: increader allows
// SVG and AVIF images through the sanitiser same as any other <img>, but
// neither has a decoder registered (SVG is XML, not a raster format at all;
// there is no AVIF decoder in std or golang.org/x/image), so both simply
// report 0,0 here. That is the "unknown" value the rest of the stack already
// expects — see DocumentImage.Width and renderImage — so an unmeasurable
// image degrades to exactly the no-width/height behaviour that existed
// before this measurement was added, rather than failing the fetch.
func decodeImageDimensions(data []byte) (width, height int) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return config.Width, config.Height
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
