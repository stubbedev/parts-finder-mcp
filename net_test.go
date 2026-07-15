package main

import (
	"net/http"
	"testing"
	"time"
)

func TestSyncFetchSite(t *testing.T) {
	mk := func(target, referer string) string {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Sec-Fetch-Site", "none")
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		syncFetchSite(req)
		return req.Header.Get("Sec-Fetch-Site")
	}
	if got := mk("https://shop.dk/p/123", ""); got != "none" {
		t.Errorf("no referer must stay none, got %q", got)
	}
	if got := mk("https://shop.dk/p/123", "https://shop.dk/search?q=x"); got != "same-origin" {
		t.Errorf("same-host referer -> same-origin, got %q", got)
	}
	if got := mk("https://shop.dk/p/123", "https://www.google.com/"); got != "cross-site" {
		t.Errorf("different-host referer -> cross-site, got %q", got)
	}
}

func TestRetryableAndTTL(t *testing.T) {
	for _, code := range []int{429, 502, 503, 504} {
		if !isRetryable(code) {
			t.Errorf("%d should be retryable", code)
		}
	}
	for _, code := range []int{200, 301, 400, 403, 404} {
		if isRetryable(code) {
			t.Errorf("%d should not be retryable", code)
		}
	}
	if ttlFor("spec") <= ttlFor("page") || ttlFor("page") <= ttlFor("listing") {
		t.Errorf("TTL ordering: spec > page > listing, got %v %v %v",
			ttlFor("spec"), ttlFor("page"), ttlFor("listing"))
	}
	if ttlFor("listing") != time.Hour {
		t.Errorf("listing TTL should be 1h, got %v", ttlFor("listing"))
	}
}

func TestFingerprintStable(t *testing.T) {
	// Same host → same fingerprint index every time (session coherence).
	a := fingerprintFor("www.ebay.de")
	b := fingerprintFor("www.ebay.de")
	if a != b {
		t.Errorf("fingerprint not stable for a host: %d != %d", a, b)
	}
	if a < 0 || a >= len(fingerprints) {
		t.Errorf("fingerprint index out of range: %d", a)
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", time.Now().Add(10*time.Second).UTC().Format(http.TimeFormat))
	if d := retryAfter(resp); d < 5*time.Second || d > 15*time.Second {
		t.Errorf("HTTP-date Retry-After should be ~10s, got %v", d)
	}
	resp.Header.Set("Retry-After", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat))
	if d := retryAfter(resp); d != 0 {
		t.Errorf("past date should be 0, got %v", d)
	}
	resp.Header.Set("Retry-After", "120")
	if d := retryAfter(resp); d != 30*time.Second {
		t.Errorf("hostile seconds should cap at 30s, got %v", d)
	}
}
