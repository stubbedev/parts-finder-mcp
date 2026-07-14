package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"golang.org/x/net/html/charset"
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
	// Header timeout catches dead hosts fast; the overall timeout only bounds
	// the body read, so a big datasheet on a slow link isn't killed at 30s.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	// Handshake with a real browser's ClientHello (per-host, matching the UA
	// family) so JA3 doesn't betray us as a bot — the actual weak link. h1-only;
	// see dialUTLS.
	tr.DialTLSContext = dialUTLS
	return &http.Client{Timeout: 120 * time.Second, Jar: jar, Transport: tr}
}

// get fetches a URL through the hardened core, preferring https: it upgrades
// plain-http URLs and falls back to the original scheme if TLS won't answer —
// or answers with an error status (a parked https vhost must not shadow a
// working http:// page).
func get(ctx context.Context, u string) (*http.Response, error) {
	if strings.HasPrefix(u, "http://") {
		// The upgrade is a PROBE: one attempt, no retry accumulation — a host
		// with no TLS must not burn the retry budget before the http fallback.
		if resp, err := doRequestN(ctx, http.MethodGet, "https://"+strings.TrimPrefix(u, "http://"), nil, 1); err == nil {
			if resp.StatusCode < 400 {
				return resp, nil
			}
			drainClose(resp.Body)
		}
	}
	return doRequest(ctx, http.MethodGet, u, nil)
}

// readCapped reads r up to cap bytes and reports whether the source had more —
// a hit cap is TRUNCATION and the caller must say so, never present a cut
// document as complete.
func readCapped(r io.Reader, cap int64) (data []byte, truncated bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, cap+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > cap {
		return data[:cap], true, nil
	}
	return data, false, nil
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
	data, truncated, err := readCapped(resp.Body, 12<<20) // 12MB cap
	if err != nil {
		return nil, "", err
	}
	if truncated {
		// A cut image still has a valid header — vision would silently read
		// half a spec sheet. Fail loudly instead.
		return nil, "", fmt.Errorf("fetch image %s: exceeds the 12MB cap", rawURL)
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

// search queries a chain of keyless engines, biased to the caller's detected
// region, and ranks local/preferred vendors first. Getting zero results
// because ONE engine throttled us is unacceptable — search is the tool's
// front door — so engines are tried in order and a rate-limited engine is
// put on cooldown and skipped, never hammered deeper into a ban.
func search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	return searchRegion(ctx, query, limit, detectRegion(ctx))
}

// errRateLimited marks an engine response as a throttle/bot-wall so the chain
// cools that engine down instead of retrying it.
var errRateLimited = errors.New("rate limited")

// rateLimitedStatus: statuses search endpoints use to say "go away" — DDG
// answers 202 with an anomaly page, others 403/429.
func rateLimitedStatus(code int) bool {
	return code == http.StatusAccepted || code == http.StatusForbidden ||
		code == http.StatusTooManyRequests
}

type searchEngine struct {
	name string
	fn   func(context.Context, string, int, Region) ([]SearchHit, error)
}

// searchEngines is the fallback chain. A configured SearXNG (SEARXNG_URL)
// goes first — the user's own instance has no rate-limit exposure. Then two
// INDEPENDENT DuckDuckGo endpoints (html and lite throttle separately), then
// Brave (own index), Ecosia (Bing/Google-backed) and Yahoo (Bing-backed but a
// separate frontend + throttle bucket) — unrelated throttle regimes. All
// keyless, all through the hardened doRequest path. Bing and Mojeek were
// evaluated and rejected: Bing serves degraded generic results to non-JS
// clients (silently wrong is worse than failing), Mojeek serves a JS/captcha
// challenge. Qwant (403 bot-wall) and Marginalia (dead public API, thin
// commercial coverage) were probed and dropped too.
func searchEngines() []searchEngine {
	var es []searchEngine
	if os.Getenv("SEARXNG_URL") != "" {
		es = append(es, searchEngine{"searxng", searchSearXNG})
	}
	return append(es,
		searchEngine{"ddg-html", searchDDG},
		searchEngine{"ddg-lite", searchDDGLite},
		searchEngine{"brave", searchBrave},
		searchEngine{"ecosia", searchEcosia},
		searchEngine{"yahoo", searchYahoo},
	)
}

// Engine cooldowns: a rate-limited engine sits out; the chain moves on.
var (
	cooldownMu sync.Mutex
	cooldowns  = map[string]time.Time{}
)

const engineCooldownFor = 5 * time.Minute

func coolingDown(name string) bool {
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	return time.Now().Before(cooldowns[name])
}

func startCooldown(name string) {
	cooldownMu.Lock()
	cooldowns[name] = time.Now().Add(engineCooldownFor)
	cooldownMu.Unlock()
	fmt.Fprintf(os.Stderr, "parts-finder: search engine %s rate-limited, cooling down %s\n", name, engineCooldownFor)
}

// searchChain runs the engines in order: first one to yield hits wins.
// Rate-limits start a cooldown and move on. Distinguishes "every engine
// failed" (error — the caller must know search is blind) from "engines
// answered but the query has no results" (legit empty).
func searchChain(ctx context.Context, engines []searchEngine, query string, limit int, r Region) ([]SearchHit, error) {
	var errs []string
	answered := false
	for _, e := range engines {
		if coolingDown(e.name) {
			errs = append(errs, e.name+": cooling down")
			continue
		}
		hits, err := e.fn(ctx, query, limit, r)
		switch {
		case errors.Is(err, errRateLimited):
			startCooldown(e.name)
			errs = append(errs, e.name+": rate limited")
		case err != nil:
			errs = append(errs, e.name+": "+err.Error())
		case len(hits) > 0:
			return hits, nil
		default:
			answered = true // engine worked, query just has no hits here
			errs = append(errs, e.name+": 0 results")
		}
	}
	if answered {
		return nil, nil
	}
	// Every keyless engine is throttled/blocked. Rather than dead-end, hand the
	// search leg back to the calling agent: if the MCP host has its own
	// web_search tool (Claude Code and friends do), it runs on the provider's
	// infra — a different IP and quota the user already pays for, zero setup
	// here. The tool result IS the handoff channel; the model acts on this text.
	return nil, fmt.Errorf("all keyless search engines are throttled/blocked for %q [%s].\n"+
		"FALLBACK: if you (the calling agent) have a web_search / WebSearch tool, run it now with this exact query, "+
		"then pass the top result URLs to fetch_content or deep_specs — that reuses your own search (no setup, separate quota). "+
		"For a permanent private backend, a self-hoster can set SEARXNG_URL.", query, strings.Join(errs, "; "))
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
	hits, err := searchChain(ctx, searchEngines(), query, limit, r)
	if err == nil && len(hits) > 0 {
		searchMu.Lock()
		searchCache[key] = searchEntry{hits: append([]SearchHit(nil), hits...), at: time.Now()}
		searchMu.Unlock()
	}
	known, _ := store.knownVendors(r.Country) // learned vendor preference; nil on error is fine
	rankHits(hits, r, known)
	return hits, err
}

// fetchSearchPage GETs a search-results URL and returns the body, translating
// throttle statuses and challenge pages into errRateLimited. Each engine gets
// a short budget: search is the front door, and a slow engine must cost
// seconds before the chain moves on — not the whole request's patience.
func fetchSearchPage(ctx context.Context, u, engineLabel string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if rateLimitedStatus(resp.StatusCode) {
		return nil, fmt.Errorf("%s %s: %w", engineLabel, resp.Status, errRateLimited)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", engineLabel, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func searchDDG(ctx context.Context, query string, limit int, r Region) ([]SearchHit, error) {
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	if r.DDG != "" {
		u += "&kl=" + url.QueryEscape(r.DDG)
	}
	body, err := fetchSearchPage(ctx, u, "duckduckgo")
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
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
	// DDG serves its bot-wall as a 200 "anomaly" page with zero results —
	// that's a throttle, not an empty query.
	if len(hits) == 0 && isDDGAnomaly(body) {
		return nil, fmt.Errorf("duckduckgo anomaly page: %w", errRateLimited)
	}
	return hits, nil
}

// isDDGAnomaly recognizes DuckDuckGo's bot-challenge page.
func isDDGAnomaly(body []byte) bool {
	b := strings.ToLower(string(body))
	return strings.Contains(b, "anomaly") || strings.Contains(b, "bots use duckduckgo")
}

// isChallengePage recognizes generic bot-wall/challenge markers. A 200
// challenge page parses to zero hits — without this check that reads as "the
// query has no results" and the chain stops instead of trying the next engine.
func isChallengePage(body []byte) bool {
	b := strings.ToLower(string(body))
	for _, marker := range []string{
		"verify you are human", "are you a robot", "unusual traffic",
		"enable javascript and cookies", "captcha", "cf-challenge",
	} {
		if strings.Contains(b, marker) {
			return true
		}
	}
	return false
}

// searchDDGLite scrapes lite.duckduckgo.com — a separate, simpler endpoint
// with its own throttle bucket; often up when /html is angry.
func searchDDGLite(ctx context.Context, query string, limit int, r Region) ([]SearchHit, error) {
	u := "https://lite.duckduckgo.com/lite/?q=" + url.QueryEscape(query)
	if r.DDG != "" {
		u += "&kl=" + url.QueryEscape(r.DDG)
	}
	body, err := fetchSearchPage(ctx, u, "duckduckgo-lite")
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Walk rows structurally: a snippet row belongs to the link row above it.
	// Index-pairing (link i ↔ snippet i) shifts every later snippet onto the
	// wrong hit as soon as one result lacks a snippet row.
	var hits []SearchHit
	doc.Find("tr").Each(func(_ int, tr *goquery.Selection) {
		if a := tr.Find("a.result-link").First(); a.Length() > 0 {
			if len(hits) >= limit {
				return
			}
			if link := decodeDDGLink(a.AttrOr("href", "")); link != "" {
				hits = append(hits, SearchHit{Title: strings.TrimSpace(a.Text()), URL: link})
			}
			return
		}
		if s := tr.Find("td.result-snippet").First(); s.Length() > 0 && len(hits) > 0 && hits[len(hits)-1].Snippet == "" {
			hits[len(hits)-1].Snippet = strings.TrimSpace(s.Text())
		}
	})
	if len(hits) == 0 && isDDGAnomaly(body) {
		return nil, fmt.Errorf("duckduckgo-lite anomaly page: %w", errRateLimited)
	}
	return hits, nil
}

// searchBrave scrapes Brave Search's HTML — an independent index. Selectors
// use stable class TOKENS (.snippet, .title, .snippet-description); the
// svelte-* hash classes churn per deploy and are never referenced.
func searchBrave(ctx context.Context, query string, limit int, r Region) ([]SearchHit, error) {
	u := "https://search.brave.com/search?q=" + url.QueryEscape(query)
	if r.Country != "" {
		u += "&country=" + strings.ToLower(r.Country)
	}
	body, err := fetchSearchPage(ctx, u, "brave")
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var hits []SearchHit
	doc.Find("div.snippet").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		a := s.Find("a[href]").First()
		href, _ := a.Attr("href")
		if !strings.HasPrefix(href, "http") || strings.Contains(hostOf(href), "brave.com") {
			return true
		}
		title := strings.TrimSpace(s.Find(".title").First().Text())
		if title == "" {
			return true // ads/widgets — organic snippets always carry a title
		}
		hits = append(hits, SearchHit{
			Title:   title,
			URL:     href,
			Snippet: strings.TrimSpace(s.Find(".snippet-description, .generic-snippet").First().Text()),
		})
		return len(hits) < limit
	})
	if len(hits) == 0 && isChallengePage(body) {
		return nil, fmt.Errorf("brave challenge page: %w", errRateLimited)
	}
	return hits, nil
}

// searchEcosia scrapes ecosia.org, whose organic results come from
// Bing/Google — big-index coverage without Bing's degraded bot page.
// Selectors pin the stable data-test-id attributes, not styling classes.
func searchEcosia(ctx context.Context, query string, limit int, _ Region) ([]SearchHit, error) {
	u := "https://www.ecosia.org/search?q=" + url.QueryEscape(query)
	body, err := fetchSearchPage(ctx, u, "ecosia")
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var hits []SearchHit
	doc.Find("div.mainline__result-wrapper").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		a := s.Find(`a[data-test-id="result-link"]`).First()
		href, _ := a.Attr("href")
		if !strings.HasPrefix(href, "http") {
			return true
		}
		hits = append(hits, SearchHit{
			Title:   strings.TrimSpace(s.Find("div.result__title").First().Text()),
			URL:     href,
			Snippet: strings.TrimSpace(s.Find(`[data-test-id="result-description"]`).First().Text()),
		})
		return len(hits) < limit
	})
	if len(hits) == 0 && isChallengePage(body) {
		return nil, fmt.Errorf("ecosia challenge page: %w", errRateLimited)
	}
	return hits, nil
}

// searchYahoo scrapes Yahoo Search. Its organic results come from Bing's index
// but behind a SEPARATE frontend, IP and throttle bucket from Ecosia — a
// distinct regime worth having when the others cool down. Yahoo pads the top
// with bing.com/aclick ADS; those live in data-matarget="ad" blocks that never
// match div.algo, and any aclick URL that still leaks through is dropped — so
// sponsored junk never reads as an organic result (the reason bare Bing was
// rejected). Organic links are wrapped in a /RU=<encoded>/RK= redirect that
// decodeYahooLink unwraps. Region: Yahoo has no clean kl-style param; rankHits
// re-biases by region afterward, so the query stays plain.
// ponytail: no region param — the post-rank handles locale bias.
func searchYahoo(ctx context.Context, query string, limit int, _ Region) ([]SearchHit, error) {
	u := "https://search.yahoo.com/search?p=" + url.QueryEscape(query)
	body, err := fetchSearchPage(ctx, u, "yahoo")
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var hits []SearchHit
	doc.Find("div.algo").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		a := s.Find(`a[href*="r.search.yahoo.com"]`).First()
		link := decodeYahooLink(a.AttrOr("href", ""))
		if link == "" || strings.Contains(link, "bing.com/aclick") {
			return true // unresolved redirect or a sponsored aclick leak
		}
		title := strings.TrimSpace(s.Find("h3.title").First().Text())
		if title == "" {
			return true
		}
		hits = append(hits, SearchHit{
			Title:   title,
			URL:     link,
			Snippet: strings.TrimSpace(s.Find(".compText").First().Text()),
		})
		return len(hits) < limit
	})
	if len(hits) == 0 && isChallengePage(body) {
		return nil, fmt.Errorf("yahoo challenge page: %w", errRateLimited)
	}
	return hits, nil
}

// decodeYahooLink unwraps Yahoo's /RU=<url-encoded>/RK= result redirect to the
// real destination. Returns "" for anything that isn't a resolvable http(s) URL.
func decodeYahooLink(href string) string {
	i := strings.Index(href, "/RU=")
	if i < 0 {
		return ""
	}
	rest := href[i+len("/RU="):]
	j := strings.Index(rest, "/RK=")
	if j < 0 {
		return ""
	}
	real, err := url.QueryUnescape(rest[:j])
	if err != nil || !strings.HasPrefix(real, "http") {
		return ""
	}
	return real
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
		return nil, fmt.Errorf("searxng 429: %w", errRateLimited)
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
			return rec.fetched(false, ""), true, nil // fresh
		}
		if rec.ETag != "" || rec.LastModified != "" {
			f, notMod, err := revalidate(ctx, rawURL, rec.ETag, rec.LastModified)
			switch {
			case err == nil && notMod:
				store.touchCache(rawURL)
				return rec.fetched(false, ""), true, nil
			case err == nil:
				store.putCache(rawURL, f.Title, f.Text, f.ETag, f.LastModified, kind)
				return f, false, nil
			default:
				// Revalidation blocked → serve stale, SAYING SO — a week-old
				// price must never read as current.
				return rec.fetched(true, err.Error()), true, nil
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
			return rec.fetched(true, err.Error()), true, nil // serve stale on failure, flagged
		}
		return Fetched{}, false, err
	}
	f.FetchedAt = time.Now()
	// Don't cache scanned-PDF image results (the cache only holds text — a hit
	// would return empty text with no images), and don't cache near-empty
	// text: a JS-shell SPA would poison the cache for the whole TTL and block
	// a later render=true from ever seeing fresh content.
	if len(f.Images) == 0 && len(strings.TrimSpace(f.Text)) >= minCacheChars {
		store.putCache(rawURL, f.Title, f.Text, f.ETag, f.LastModified, kind)
	}
	return f, false, nil
}

// minCacheChars: extractions thinner than this are not worth caching — almost
// always a JS-only shell or a challenge page, not real content.
const minCacheChars = 200

func (r cacheRec) fetched(stale bool, reason string) Fetched {
	return Fetched{Title: r.Title, Text: r.Text, ETag: r.ETag, LastModified: r.LastModified,
		FetchedAt: r.FetchedAt, Stale: stale, StaleReason: reason}
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
	ImageTotal   int        // page images found in the document (>len(Images) = some dropped)
	FetchedAt    time.Time  // when the content was actually downloaded (cache hits keep the original time)
	Stale        bool       // served from cache PAST its TTL because the live fetch failed
	StaleReason  string     // why the live fetch failed when Stale
	Truncated    bool       // the source exceeded the size cap — text is a PREFIX, not the whole document
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
		raw, truncated, err := readCapped(br, 64<<20) // 64MB PDF cap
		if err != nil {
			return Fetched{}, err
		}
		if truncated {
			// A PDF's xref table lives at the end — a cut PDF is unparseable
			// (or worse, half-parseable). Refuse rather than return junk.
			return Fetched{}, fmt.Errorf("pdf %s exceeds the 64MB cap — find a smaller mirror of the document", rawURL)
		}
		t, x, perr := extractPDF(bytes.NewReader(raw))
		f.Title, f.Text = t, x
		// Scanned/image-only PDF (no usable text layer) — or a text extraction
		// failure: fall back to page images so vision can OCR the datasheet.
		if perr != nil || len(strings.Fields(x)) < scannedWordThreshold || len(strings.TrimSpace(x)) < scannedTextThreshold {
			if imgs, total := pdfPageImages(raw, 5); len(imgs) > 0 {
				f.Images, f.ImageTotal = imgs, total
			} else if perr != nil {
				// No text layer AND no images — an empty success here would
				// cache "the datasheet says nothing" for a month.
				return Fetched{}, fmt.Errorf("unreadable pdf %s: %w", rawURL, perr)
			}
		}
		return f, nil
	}
	buf, truncated, err := readCapped(br, 8<<20) // 8MB page cap
	if err != nil {
		return Fetched{}, err
	}
	f.Truncated = truncated // partial HTML still extracts useful text — flagged, not fatal
	t, x, err := extractHTML(buf, rawURL, ct)
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
// yields worse extraction than an unblocked one. contentType drives charset
// decoding (older shops still serve ISO-8859-1 — æ/ø/å and "kr." prices must
// not extract as mojibake); "" means already-UTF-8 (the renderer's DOM dump).
func extractHTML(buf []byte, rawURL, contentType string) (title, text string, err error) {
	if contentType != "" {
		if cr, cerr := charset.NewReader(bytes.NewReader(buf), contentType); cerr == nil {
			if decoded, derr := io.ReadAll(cr); derr == nil {
				buf = decoded
			}
		}
	}
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
