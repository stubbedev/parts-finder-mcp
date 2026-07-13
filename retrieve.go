package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	readability "github.com/go-shiori/go-readability"
	"github.com/ledongthuc/pdf"
)

// Plain HTTP + DDG HTML endpoint covers static vendor/reseller pages;
// bot-blocked pages auto-escalate to the zero-config lightpanda renderer
// (render.go). Extraction quality is identical on both paths (extractHTML).

// userAgent is the default fingerprint's UA — used where a single UA string is
// needed (e.g. the lightpanda download).
const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// httpClient follows redirects (Go default, ≤10) and carries session cookies
// across the set-cookie-then-redirect dance half the shops do (cookie jar).
// Throttling, retries, and fingerprint rotation live in doRequest (net.go).
var httpClient = newHTTPClient()

func newHTTPClient() *http.Client {
	jar, _ := cookiejar.New(nil) // only errors on nil PublicSuffixList options
	return &http.Client{Timeout: 30 * time.Second, Jar: jar}
}

// get fetches a URL through the hardened core, preferring https: it upgrades
// plain-http URLs and falls back to the original scheme only if TLS won't
// answer.
func get(ctx context.Context, u string) (*http.Response, error) {
	if strings.HasPrefix(u, "http://") {
		if resp, err := doRequest(ctx, http.MethodGet, "https://"+strings.TrimPrefix(u, "http://"), nil); err == nil {
			return resp, nil
		}
	}
	return doRequest(ctx, http.MethodGet, u, nil)
}

// fetchPDFBrowserish re-tries a bot-blocked PDF download with a site-root
// referer. Covers hotlink-protection 403s; TLS-fingerprint walls still lose.
func fetchPDFBrowserish(ctx context.Context, rawURL string) (title, text string, err error) {
	extra := map[string]string{"Accept": "application/pdf,application/octet-stream,*/*"}
	if pu, perr := url.Parse(rawURL); perr == nil {
		extra["Referer"] = pu.Scheme + "://" + pu.Host + "/"
	}
	resp, err := doRequest(ctx, http.MethodGet, rawURL, extra)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("pdf retry got %s", resp.Status)
	}
	return extractPDF(io.LimitReader(resp.Body, 64<<20))
}

// fetchImage downloads an image for visual reading (spec-sheet diagrams,
// product photos, labels the model can OCR by eye). Returns raw bytes + MIME.
func fetchImage(ctx context.Context, rawURL, mode string, maxEdge int) (data []byte, mime string, err error) {
	resp, err := doRequest(ctx, http.MethodGet, rawURL, map[string]string{
		"Accept":         "image/avif,image/webp,image/png,image/jpeg,image/*,*/*;q=0.8",
		"Sec-Fetch-Dest": "image",
		"Sec-Fetch-Mode": "no-cors",
		"Sec-Fetch-Site": "cross-site",
	})
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch image %s: %s", rawURL, resp.Status)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, 12<<20)) // 12MB cap
	if err != nil {
		return nil, "", err
	}
	mime = resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	if !strings.HasPrefix(mime, "image/") {
		mime = http.DetectContentType(data) // trust bytes over a wrong header
	}
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", fmt.Errorf("fetch image %s: not an image (%s)", rawURL, mime)
	}
	data, mime = optimizeImage(data, mime, mode, maxEdge) // shrink before it hits context
	return data, mime, nil
}

// SearchHit is one search result.
type SearchHit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// search queries the keyless DuckDuckGo HTML endpoint, biased to the caller's
// detected region, and ranks local/preferred vendors first. Falls back to
// SearXNG (SEARXNG_URL) when DDG errors or returns nothing (rate-limited).
func search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	return searchRegion(ctx, query, limit, detectRegion(ctx))
}

// searchCache: repeat queries within a session (deep_specs angles, shop_spec
// re-runs) hit DDG once. 15 min TTL — fresh enough for shopping, kind enough
// to not get throttled.
var (
	searchMu    sync.Mutex
	searchCache = map[string]searchEntry{}
)

type searchEntry struct {
	hits []SearchHit
	at   time.Time
}

const searchTTL = 15 * time.Minute

func searchRegion(ctx context.Context, query string, limit int, r Region) ([]SearchHit, error) {
	if limit <= 0 {
		limit = 10
	}
	key := fmt.Sprintf("%s|%d|%s", query, limit, r.DDG)
	searchMu.Lock()
	if e, ok := searchCache[key]; ok && time.Since(e.at) < searchTTL {
		searchMu.Unlock()
		hits := append([]SearchHit(nil), e.hits...) // callers may reorder
		known, _ := store.knownVendors(r.Country)
		rankHits(hits, r, known)
		return hits, nil
	}
	searchMu.Unlock()
	hits, err := searchDDG(ctx, query, limit, r)
	if (err != nil || len(hits) == 0) && os.Getenv("SEARXNG_URL") != "" {
		if sx, sxErr := searchSearXNG(ctx, query, limit, r); sxErr == nil && len(sx) > 0 {
			hits, err = sx, nil
		}
	}
	if err == nil && len(hits) > 0 {
		searchMu.Lock()
		searchCache[key] = searchEntry{hits: append([]SearchHit(nil), hits...), at: time.Now()}
		searchMu.Unlock()
	}
	known, _ := store.knownVendors(r.Country) // learned vendor preference; nil on error is fine
	rankHits(hits, r, known)
	return hits, err
}

func searchDDG(ctx context.Context, query string, limit int, r Region) ([]SearchHit, error) {
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	if r.DDG != "" {
		u += "&kl=" + url.QueryEscape(r.DDG)
	}
	resp, err := get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckduckgo returned %s", resp.Status)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}
	var hits []SearchHit
	doc.Find(".result").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		a := s.Find("a.result__a")
		href, _ := a.Attr("href")
		link := decodeDDGLink(href)
		if link == "" {
			return true
		}
		hits = append(hits, SearchHit{
			Title:   strings.TrimSpace(a.Text()),
			URL:     link,
			Snippet: strings.TrimSpace(s.Find(".result__snippet").Text()),
		})
		return len(hits) < limit
	})
	return hits, nil
}

// searchSearXNG queries a self-hosted SearXNG instance's JSON API.
// SEARXNG_URL is the base URL, e.g. http://localhost:8888.
func searchSearXNG(ctx context.Context, query string, limit int, r Region) ([]SearchHit, error) {
	base := strings.TrimRight(os.Getenv("SEARXNG_URL"), "/")
	u := base + "/search?format=json&q=" + url.QueryEscape(query)
	if lang := ddgLangCode(r.DDG); lang != "" {
		u += "&language=" + lang // bias to the region's language, like the DDG kl param
	}
	resp, err := get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return nil, fmt.Errorf("searxng 403: enable the 'json' format in the instance settings.yml")
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("searxng 429: rate limited")
	default:
		return nil, fmt.Errorf("searxng returned %s", resp.Status)
	}
	var body struct {
		Results []struct {
			Title, URL, Content string
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var hits []SearchHit
	for _, res := range body.Results {
		if !strings.HasPrefix(res.URL, "http") { // skip malformed / relative URLs
			continue
		}
		hits = append(hits, SearchHit{Title: res.Title, URL: res.URL, Snippet: res.Content})
		if len(hits) >= limit {
			break
		}
	}
	return hits, nil
}

// ddgLangCode extracts the language part of a DDG kl code (e.g. "dk-da" -> "da").
func ddgLangCode(kl string) string {
	if i := strings.Index(kl, "-"); i >= 0 {
		return kl[i+1:]
	}
	return ""
}

// decodeDDGLink unwraps DDG's //duckduckgo.com/l/?uddg=<encoded> redirect.
func decodeDDGLink(href string) string {
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if real := parsed.Query().Get("uddg"); real != "" {
		return real
	}
	if strings.HasPrefix(href, "http") {
		return href
	}
	return ""
}

// fetchContent downloads a URL and extracts readable text. PDFs (spec sheets)
// go through the PDF text extractor; everything else through readability.
// Cache TTLs by kind — how long fetched content is trusted before
// revalidation. Spec sheets barely change; live listings/prices go stale fast.
// ponytail: three buckets cover the real cases; add a kind if a source needs
// its own cadence.
func ttlFor(kind string) time.Duration {
	switch kind {
	case "spec":
		return 30 * 24 * time.Hour
	case "listing":
		return time.Hour
	default: // "page" and unknown
		return 24 * time.Hour
	}
}

// fetchCached is the front door for all content retrieval. Fresh cache hits
// return instantly; stale-but-revalidatable entries do a cheap conditional GET
// (304 → keep, bump TTL); misses fetch live. On any fetch failure it serves
// stale content if we have it — a transient block must never erase
// last-known-good data. Returns (result, servedFromCache, error).
func fetchCached(ctx context.Context, rawURL, kind string, render bool) (Fetched, bool, error) {
	rec, have := store.getCache(rawURL)
	if have && !render {
		if time.Since(rec.FetchedAt) < ttlFor(kind) {
			return rec.fetched(), true, nil // fresh
		}
		if rec.ETag != "" || rec.LastModified != "" {
			f, notMod, err := revalidate(ctx, rawURL, rec.ETag, rec.LastModified)
			switch {
			case err == nil && notMod:
				store.touchCache(rawURL)
				return rec.fetched(), true, nil
			case err == nil:
				store.putCache(rawURL, f.Title, f.Text, f.ETag, f.LastModified, kind)
				return f, false, nil
			default:
				return rec.fetched(), true, nil // revalidation blocked → serve stale
			}
		}
	}
	var f Fetched
	var err error
	if render {
		var t, x string
		t, x, err = fetchRendered(ctx, rawURL)
		f = Fetched{Title: t, Text: x, Rendered: true}
	} else {
		f, err = fetchContent(ctx, rawURL)
	}
	if err != nil {
		if have {
			return rec.fetched(), true, nil // serve stale on failure
		}
		return Fetched{}, false, err
	}
	// Don't cache scanned-PDF image results — the cache only holds text, and a
	// hit would return empty text with no images. Re-extract each time (rare).
	if len(f.Images) == 0 {
		store.putCache(rawURL, f.Title, f.Text, f.ETag, f.LastModified, kind)
	}
	return f, false, nil
}

func (r cacheRec) fetched() Fetched {
	return Fetched{Title: r.Title, Text: r.Text, ETag: r.ETag, LastModified: r.LastModified}
}

// Fetched is the result of a content fetch plus HTTP validators for cheap
// cache revalidation.
type Fetched struct {
	Title        string
	Text         string
	ETag         string
	LastModified string
	Rendered     bool
	Images       []DocImage // scanned-PDF page images for visual OCR (not cached)
}

func fetchContent(ctx context.Context, rawURL string) (Fetched, error) {
	resp, err := get(ctx, rawURL)
	if err != nil {
		return Fetched{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		// Bot-blocked (eBay/Akamai and friends). Auto-escalate through the
		// headless renderer — zero config, lightpanda is spawned (and even
		// downloaded) on demand. PDFs can't be rendered by a browser DOM;
		// instead retry the download with full browser headers + referer
		// (most PDF 403s are referer/UA checks, not TLS fingerprinting).
		if isPDFURL(rawURL) {
			if t, x, perr := fetchPDFBrowserish(ctx, rawURL); perr == nil {
				return Fetched{Title: t, Text: x}, nil
			}
			return Fetched{}, fmt.Errorf("fetch %s: %s — PDF host blocks plain HTTP even with browser headers; find a mirror of the datasheet", rawURL, resp.Status)
		}
		if t, x, rerr := fetchRendered(ctx, rawURL); rerr == nil {
			return Fetched{Title: t, Text: x, Rendered: true}, nil
		} else {
			return Fetched{}, fmt.Errorf("fetch %s: %s — site blocks plain HTTP and the render fallback failed (%v); extract what you can from the search snippet", rawURL, resp.Status, rerr)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return Fetched{}, fmt.Errorf("fetch %s: %s", rawURL, resp.Status)
	}
	return extractResponse(resp, rawURL)
}

func isPDFURL(u string) bool { return strings.HasSuffix(strings.ToLower(u), ".pdf") }

// extractResponse turns a 200 response into Fetched: PDF sniffing (content-type,
// URL suffix, AND payload magic bytes — spec sheets are often mislabeled) then
// HTML extraction, carrying HTTP validators for later revalidation.
func extractResponse(resp *http.Response, rawURL string) (Fetched, error) {
	f := Fetched{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	br := bufio.NewReader(resp.Body)
	ct := resp.Header.Get("Content-Type")
	magic, _ := br.Peek(5)
	if strings.Contains(ct, "application/pdf") || isPDFURL(rawURL) || bytes.HasPrefix(magic, []byte("%PDF-")) {
		raw, err := io.ReadAll(io.LimitReader(br, 64<<20)) // 64MB PDF cap
		if err != nil {
			return Fetched{}, err
		}
		t, x, _ := extractPDF(bytes.NewReader(raw))
		f.Title, f.Text = t, x
		// Scanned/image-only PDF (no usable text layer): fall back to page
		// images so vision can OCR the datasheet.
		if len(strings.Fields(x)) < scannedTextThreshold/6 || len(strings.TrimSpace(x)) < scannedTextThreshold {
			if imgs := pdfPageImages(raw, 5); len(imgs) > 0 {
				f.Images = imgs
			}
		}
		return f, nil
	}
	buf, err := io.ReadAll(io.LimitReader(br, 8<<20)) // 8MB page cap
	if err != nil {
		return Fetched{}, err
	}
	t, x, err := extractHTML(buf, rawURL)
	f.Title, f.Text = t, x
	return f, err
}

// revalidate does one best-effort conditional GET. Returns notModified=true on
// a 304 (cache still good), or fresh content on 200. No escalation — a blocked
// revalidation just means the caller serves stale.
func revalidate(ctx context.Context, rawURL, etag, lastMod string) (f Fetched, notModified bool, err error) {
	extra := map[string]string{}
	if etag != "" {
		extra["If-None-Match"] = etag
	}
	if lastMod != "" {
		extra["If-Modified-Since"] = lastMod
	}
	resp, err := doRequest(ctx, http.MethodGet, rawURL, extra)
	if err != nil {
		return Fetched{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return Fetched{}, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Fetched{}, false, fmt.Errorf("revalidate %s: %s", rawURL, resp.Status)
	}
	f, err = extractResponse(resp, rawURL)
	return f, false, err
}

// extractHTML is the single HTML→text path — readability for prose plus
// <table> preservation as markdown (hardware specs ARE tables). Used by both
// the plain fetcher and the headless renderer so a bot-blocked site never
// yields worse extraction than an unblocked one.
func extractHTML(buf []byte, rawURL string) (title, text string, err error) {
	pageURL, _ := url.Parse(rawURL)
	art, err := readability.FromReader(bytes.NewReader(buf), pageURL)
	if err != nil {
		return "", "", err
	}
	text = art.TextContent
	if tables := extractTables(bytes.NewReader(buf)); tables != "" {
		text += "\n\n## Tables\n\n" + tables
	}
	return art.Title, text, nil
}

// extractTables renders every <table> on the page as a markdown table so
// key/value spec data survives extraction. Best-effort: returns "" on any
// parse problem or when there are no tables.
func extractTables(r io.Reader) string {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return ""
	}
	doc.Find("style, script").Remove() // inline CSS/JS would leak into cell text
	var b strings.Builder
	doc.Find("table").Each(func(_ int, tbl *goquery.Selection) {
		rows := tbl.Find("tr")
		if rows.Length() == 0 || rows.Length() > 500 { // skip empty/layout monsters
			return
		}
		wrote := false
		rows.Each(func(ri int, tr *goquery.Selection) {
			var cells []string
			tr.Find("th, td").Each(func(_ int, c *goquery.Selection) {
				cells = append(cells, strings.Join(strings.Fields(c.Text()), " "))
			})
			if len(cells) == 0 {
				return
			}
			b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
			if ri == 0 { // header separator after first row
				b.WriteString("|" + strings.Repeat(" --- |", len(cells)) + "\n")
			}
			wrote = true
		})
		if wrote {
			b.WriteString("\n")
		}
	})
	return strings.TrimSpace(b.String())
}

func extractPDF(body io.Reader) (title, text string, err error) {
	buf, err := io.ReadAll(body)
	if err != nil {
		return "", "", err
	}
	r, err := pdf.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return "", "", fmt.Errorf("parse pdf: %w", err)
	}
	tr, err := r.GetPlainText()
	if err != nil {
		return "", "", fmt.Errorf("pdf text: %w", err)
	}
	out, err := io.ReadAll(tr)
	if err != nil {
		return "", "", err
	}
	return "", string(out), nil
}
