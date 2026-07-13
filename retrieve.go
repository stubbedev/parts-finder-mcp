package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	readability "github.com/go-shiori/go-readability"
	"github.com/ledongthuc/pdf"
)

// ponytail: real browser (lightpanda) deferred to M3; plain HTTP + DDG HTML
// endpoint covers static vendor/reseller pages. Add lightpanda when a target
// needs JS rendering.

// Browser-grade UA: many shops flat-out 403 non-browser agents. Marketplaces
// with TLS fingerprinting (eBay/Akamai) block regardless — those need
// fetch_content(render=true) via lightpanda.
const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var httpClient = &http.Client{Timeout: 20 * time.Second}

func get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return httpClient.Do(req)
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
func fetchContent(ctx context.Context, rawURL string) (title, text string, err error) {
	resp, err := get(ctx, rawURL)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return "", "", fmt.Errorf("fetch %s: %s — site blocks plain HTTP (bot detection); retry with render=true (needs LIGHTPANDA_URL) or extract what you can from the search snippet", rawURL, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetch %s: %s", rawURL, resp.Status)
	}
	// Sniff PDF by content-type, URL suffix, AND payload magic bytes — spec
	// sheets are often served with a generic/wrong content-type. (kindly does
	// the same %PDF- check.)
	br := bufio.NewReader(resp.Body)
	ct := resp.Header.Get("Content-Type")
	magic, _ := br.Peek(5)
	if strings.Contains(ct, "application/pdf") ||
		strings.HasSuffix(strings.ToLower(rawURL), ".pdf") ||
		bytes.HasPrefix(magic, []byte("%PDF-")) {
		return extractPDF(br)
	}
	// Buffer the HTML: readability flattens <table>s, but hardware specs ARE
	// tables — extract them separately and append as markdown.
	buf, err := io.ReadAll(io.LimitReader(br, 4<<20)) // 4MB page cap
	if err != nil {
		return "", "", err
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
