package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// One-off live harness: RENDER_LIVE=1 go test -run TestLiveRenderFanout
func TestLiveRenderFanout(t *testing.T) {
	if os.Getenv("RENDER_LIVE") == "" {
		t.Skip("live render test — set RENDER_LIVE=1")
	}
	urls := []string{
		"https://www.dustin.dk/product/5020048918/1800w-2200w-flex-slot-titanium-hot-plug-power-supply-kit",                                     // CF wall
		"https://www.fcomputer.dk/hpe-high-performance-fan-kit-ventilationspakke-for-system-2u-p48820-b21",                                      // normal
		"https://edshop.edsystem.eu/hpe-proliant-compute-27c-system-inlet-ambient-operating-temperature-configuration-tracking/product-1754187", // slow hydrator
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			start := time.Now()
			title, text, err := fetchRendered(ctx, u)
			short := u
			if len(short) > 60 {
				short = short[:60]
			}
			if err != nil {
				t.Logf("%s -> ERR after %s: %v", short, time.Since(start).Round(time.Second), err)
				return
			}
			t.Logf("%s -> %s, title=%q, %d chars, botwall=%v",
				short, time.Since(start).Round(time.Second), title, len(text), isBotWall(title+"\n"+text))
			_ = strings.TrimSpace(text)
		}(u)
	}
	wg.Wait()
}

// One-off live harness: does each engine's PARSER still work on the rendered
// DOM? The render fallback is worthless if obscura clears the wall but the
// markup it returns doesn't match the selectors written for the static page.
// RENDER_LIVE=1 go test -run TestLiveSearchRender -v
func TestLiveSearchRender(t *testing.T) {
	if os.Getenv("RENDER_LIVE") == "" {
		t.Skip("live render test — set RENDER_LIVE=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, renderFetchKey{}, true)
	// Not every engine can be rendered: Ecosia challenges the renderer too,
	// and Yahoo's rendered DOM is a consent interstitial. Those cool their
	// render path down and the chain moves on, so the guarantee that matters
	// is that SOME engine survives the render — that is what keeps a
	// fully-throttled chain from going blind.
	working := 0
	for _, e := range searchEngines() {
		hits, err := e.fn(ctx, "hpe proliant dl380 gen11 price", 10, Region{DDG: "dk-da", Country: "DK"})
		switch {
		case err != nil:
			t.Logf("%s -> ERR: %v", e.name, err)
		case len(hits) == 0:
			t.Logf("%s -> 0 hits from the rendered page (parser vs rendered markup?)", e.name)
		default:
			working++
			t.Logf("%s -> %d hits, first=%s", e.name, len(hits), hits[0].URL)
		}
	}
	if working == 0 {
		t.Error("no engine parses its rendered results page — the render fallback is dead weight")
	}
}
