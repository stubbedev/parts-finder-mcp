package main

import (
	"testing"
	"time"
)

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
