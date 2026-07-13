package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The chain must survive a rate-limited engine (cooldown + move on), and must
// distinguish "all engines blind" (error) from "query legitimately empty".
func TestSearchChain(t *testing.T) {
	ctx := context.Background()
	hit := []SearchHit{{Title: "x", URL: "https://example.com"}}
	limited := func(context.Context, string, int, Region) ([]SearchHit, error) {
		return nil, fmt.Errorf("throttled: %w", errRateLimited)
	}
	working := func(context.Context, string, int, Region) ([]SearchHit, error) {
		return hit, nil
	}
	empty := func(context.Context, string, int, Region) ([]SearchHit, error) {
		return nil, nil
	}

	// Rate-limited first engine: second serves; first is now cooling down.
	engines := []searchEngine{{"lim", limited}, {"ok", working}}
	hits, err := searchChain(ctx, engines, "q", 5, Region{})
	if err != nil || len(hits) != 1 {
		t.Fatalf("second engine must serve: %v %v", hits, err)
	}
	if !coolingDown("lim") {
		t.Errorf("rate-limited engine must be cooling down")
	}
	// While cooling, the engine is skipped without being called.
	called := false
	engines = []searchEngine{
		{"lim", func(context.Context, string, int, Region) ([]SearchHit, error) {
			called = true
			return nil, nil
		}},
		{"ok", working},
	}
	if _, err := searchChain(ctx, engines, "q", 5, Region{}); err != nil || called {
		t.Errorf("cooling engine must be skipped, called=%v err=%v", called, err)
	}
	cooldowns["lim"] = time.Time{} // reset for other tests

	// Every engine blind => error (caller must know search is down).
	if _, err := searchChain(ctx, []searchEngine{{"a", limited}}, "q", 5, Region{}); err == nil {
		t.Errorf("all-blind must error")
	}
	cooldowns["a"] = time.Time{}
	// An engine answered OK with zero hits => legit empty, no error.
	hits, err = searchChain(ctx, []searchEngine{{"e", empty}}, "no such thing", 5, Region{})
	if err != nil || hits != nil {
		t.Errorf("answered-empty is not an error: %v %v", hits, err)
	}
}

func TestDDGAnomalyDetection(t *testing.T) {
	if !isDDGAnomaly([]byte(`<html><body class="anomaly-modal">…</body></html>`)) {
		t.Errorf("anomaly page must be detected")
	}
	if isDDGAnomaly([]byte(`<html><body>plain results</body></html>`)) {
		t.Errorf("normal page must not be flagged")
	}
}
