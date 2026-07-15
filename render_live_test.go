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
