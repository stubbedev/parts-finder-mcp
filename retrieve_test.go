package main

import (
	"context"
	"fmt"
	"strings"
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

// A bot-walled engine gets ONE retry with its fetch routed through the
// stealth renderer. Hits from that retry count normally; a rendered wall
// (zero hits) must NOT read as "the query has no results" — the engine still
// cools down and the chain still reports blindness.
func TestSearchChainRenderRetry(t *testing.T) {
	ctx := context.Background()
	rendered := 0
	walled := func(ctx context.Context, _ string, _ int, _ Region) ([]SearchHit, error) {
		if ctx.Value(renderFetchKey{}) != nil {
			rendered++
			return []SearchHit{{Title: "x", URL: "https://example.com/rendered"}}, nil
		}
		return nil, fmt.Errorf("wall: %w", errRateLimited)
	}
	hits, err := searchChain(ctx, []searchEngine{{"walled", walled}}, "q", 5, Region{})
	if err != nil || len(hits) != 1 || rendered != 1 {
		t.Fatalf("render retry must serve the wall: hits=%v err=%v renders=%d", hits, err, rendered)
	}
	if coolingDown("walled") {
		t.Errorf("an engine the renderer unblocked must not be cooled down")
	}

	// Wall survives the render: blind, not empty.
	stillWalled := func(context.Context, string, int, Region) ([]SearchHit, error) {
		return nil, fmt.Errorf("wall: %w", errRateLimited)
	}
	hits, err = searchChain(ctx, []searchEngine{{"hard", stillWalled}}, "q", 5, Region{})
	if err == nil || hits != nil {
		t.Fatalf("a wall that survives rendering must error, got hits=%v err=%v", hits, err)
	}
	if !coolingDown("hard") || !coolingDown("render:hard") {
		t.Errorf("both the engine and its render path must cool down")
	}
	// The render path cools down separately, so the next query skips the
	// ~30s render instead of paying for it again.
	cooldowns["hard"] = time.Time{}
	tried := 0
	countingWall := func(ctx context.Context, _ string, _ int, _ Region) ([]SearchHit, error) {
		if ctx.Value(renderFetchKey{}) != nil {
			tried++
		}
		return nil, fmt.Errorf("wall: %w", errRateLimited)
	}
	if _, err := searchChain(ctx, []searchEngine{{"hard", countingWall}}, "q", 5, Region{}); err == nil {
		t.Errorf("still blind")
	}
	if tried != 0 {
		t.Errorf("render path is cooling down; it must not be retried (%d attempts)", tried)
	}
	cooldowns["hard"], cooldowns["render:hard"], cooldowns["walled"] = time.Time{}, time.Time{}, time.Time{}
}

func TestDecodeYahooLink(t *testing.T) {
	cases := map[string]string{
		"https://r.search.yahoo.com/_ylt=Aw/RV=2/RE=1/RO=10/RU=https%3a%2f%2fwww.pishop.us%2fproduct%2fpi-4%2f/RK=2/RS=x-": "https://www.pishop.us/product/pi-4/",
		"https://r.search.yahoo.com/x/RU=https%3a%2f%2fwww.bing.com%2faclick%3fld=e8/RK=2/RS=y-":                           "https://www.bing.com/aclick?ld=e8", // decoded; the aclick filter drops it at the call site
		"https://example.com/no-redirect":                  "",
		"https://r.search.yahoo.com/RU=not%20a%20url/RK=2": "",
	}
	for in, want := range cases {
		if got := decodeYahooLink(in); got != want {
			t.Errorf("decodeYahooLink(%q) = %q, want %q", in, got, want)
		}
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

func TestPageText(t *testing.T) {
	// Small text: one page, no next.
	if p, total, next := pageText("hello", 0); p != "hello" || total != 5 || next != 0 {
		t.Errorf("small: %q %d %d", p, total, next)
	}
	// Big text pages through completely with no loss and rune-safe cuts.
	line := strings.Repeat("æøå spec row | 128 GB | DDR5 ", 40) + "\n"
	big := strings.Repeat(line, 100)
	var got strings.Builder
	off, pages := 0, 0
	for {
		p, total, next := pageText(big, off)
		if total != len(big) {
			t.Fatalf("total %d != %d", total, len(big))
		}
		if next > 0 {
			if len(p) > maxFetchChars {
				t.Fatalf("page %d chars > cap", len(p))
			}
			if !strings.HasSuffix(p, "\n") {
				t.Fatalf("mid-doc page must cut on newline")
			}
		}
		got.WriteString(p)
		pages++
		if next == 0 {
			break
		}
		off = next
	}
	if got.String() != big {
		t.Fatalf("pages don't reassemble the document")
	}
	if pages < 2 {
		t.Fatalf("big doc must paginate, got %d page(s)", pages)
	}
	// Past-the-end offset: empty page, total still reported.
	if p, total, next := pageText("abc", 99); p != "" || total != 3 || next != 0 {
		t.Errorf("past-end: %q %d %d", p, total, next)
	}
}
