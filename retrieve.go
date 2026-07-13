package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	readability "github.com/go-shiori/go-readability"
	"github.com/ledongthuc/pdf"
)

// ponytail: real browser (lightpanda) deferred to M3; plain HTTP + DDG HTML
// endpoint covers static vendor/reseller pages. Add lightpanda when a target
// needs JS rendering.

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) parts-finder-mcp/0.1"

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

// search queries the keyless DuckDuckGo HTML endpoint and parses result links.
// ponytail: SearXNG is the drop-in upgrade if DDG rate-limits — swap this func.
func search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if limit <= 0 {
		limit = 10
	}
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
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
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetch %s: %s", rawURL, resp.Status)
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "application/pdf") || strings.HasSuffix(strings.ToLower(rawURL), ".pdf") {
		return extractPDF(resp.Body)
	}
	pageURL, _ := url.Parse(rawURL)
	art, err := readability.FromReader(resp.Body, pageURL)
	if err != nil {
		return "", "", err
	}
	return art.Title, art.TextContent, nil
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
