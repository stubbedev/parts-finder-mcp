package main

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Listing is a price observation for a part from one vendor/reseller. Prices
// die fast: SeenAt drives the staleness flag; there is no live-price guarantee.
type Listing struct {
	ID        string    `json:"id,omitempty"` // derived from part+vendor+condition if omitted
	PartID    string    `json:"part_id"`
	Vendor    string    `json:"vendor,omitempty"` // seller, e.g. "ebay:seller123", "lenovo"
	Price     float64   `json:"price"`
	Shipping  float64   `json:"shipping,omitempty"`
	Currency  string    `json:"currency,omitempty"`  // ISO code of Price/Shipping
	Condition string    `json:"condition,omitempty"` // new, used, refurbished
	URL       string    `json:"url,omitempty"`
	ShipsTo   []string  `json:"ships_to,omitempty"` // country codes / "EU" / "WORLD"; empty = unknown
	SeenAt    time.Time `json:"seen_at,omitempty"`

	// Derived on read, not stored. Deals are never dropped for these — they
	// are flagged and sorted below usable ones, so nothing is hidden.
	Stale        bool    `json:"stale,omitempty"`
	Dead         bool    `json:"dead,omitempty"`          // URL no longer reachable (live-check)
	Unshippable  bool    `json:"unshippable,omitempty"`   // doesn't ship to the region
	DisplayTotal float64 `json:"display_total,omitempty"` // total converted to display currency
	DisplayCurr  string  `json:"display_currency,omitempty"`
}

// usable = clickable right now: reachable and ships to the region.
func (l Listing) usable() bool { return !l.Dead && !l.Unshippable }

func markShippable(ls []Listing, country string) {
	for i := range ls {
		ls[i].Unshippable = !shipsTo(ls[i].ShipsTo, country)
	}
}

func (l Listing) total() float64 { return l.Price + l.Shipping }

// effectiveTotal is the converted total when available, else the native total.
// Used for cross-currency sorting/comparison.
func (l Listing) effectiveTotal() float64 {
	if l.DisplayTotal > 0 {
		return l.DisplayTotal
	}
	return l.total()
}

// stalenessDays: a listing older than this is flagged stale.
// ponytail: fixed 14d threshold; make it a tool arg if it ever needs tuning.
const stalenessDays = 14

func markStale(ls []Listing, now time.Time) {
	cut := now.AddDate(0, 0, -stalenessDays)
	for i := range ls {
		ls[i].Stale = ls[i].SeenAt.Before(cut)
	}
}

// sortListings orders usable (live + shippable) first, then cheapest
// (converted total when available); ties broken by most recent. Dead and
// unshippable listings sink to the bottom but are never removed.
func sortListings(ls []Listing) {
	sort.SliceStable(ls, func(i, j int) bool {
		if ls[i].usable() != ls[j].usable() {
			return ls[i].usable()
		}
		if ls[i].effectiveTotal() != ls[j].effectiveTotal() {
			return ls[i].effectiveTotal() < ls[j].effectiveTotal()
		}
		return ls[i].SeenAt.After(ls[j].SeenAt)
	})
}

// cheapestConverted returns the listing with the lowest total once converted to
// `currency`, along with that converted total. Listings that can't be converted
// are skipped. currency == "" means compare native totals as-is.
func cheapestConverted(ctx context.Context, ls []Listing, currency string) (Listing, float64, bool) {
	var best Listing
	var bestTotal float64
	found := false
	for _, l := range ls {
		t := l.total()
		if currency != "" && l.Currency != "" && l.Currency != currency {
			c, err := convert(ctx, t, l.Currency, currency)
			if err != nil {
				continue // can't compare -> skip
			}
			t = c
		}
		if !found || t < bestTotal {
			best, bestTotal, found = l, t, true
		}
	}
	return best, bestTotal, found
}

// Substitute is a candidate replacement part with its cheapest qualifying listing.
type Substitute struct {
	Part    Part    `json:"part"`
	Listing Listing `json:"listing"`
}

// annotateDisplay fills DisplayTotal/DisplayCurr by converting each listing's
// total into the display currency. Best-effort: on conversion error the display
// fields stay empty rather than failing the whole call.
func annotateDisplay(ctx context.Context, ls []Listing, display string) {
	if display == "" {
		return
	}
	for i := range ls {
		if ls[i].Currency == "" {
			continue
		}
		if v, err := convert(ctx, ls[i].total(), ls[i].Currency, display); err == nil {
			ls[i].DisplayTotal = v
			ls[i].DisplayCurr = display
		}
	}
}

// liveCheckAll probes each listing URL concurrently and sets Dead when the URL
// is unreachable or 4xx/5xx. ponytail: status/redirect check only — a live URL
// whose price changed still reads as alive; deep price re-verification is the
// client's job via fetch_content. Bounded to 6 concurrent probes.
func liveCheckAll(ctx context.Context, ls []Listing) {
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i := range ls {
		if ls[i].URL == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			ls[i].Dead = !urlAlive(ctx, ls[i].URL)
		}(i)
	}
	wg.Wait()
}

// aliveCache: don't re-probe the same URL on every shop_spec/compare_specs
// call. 10 min TTL keeps "fresh as fuck" while not hammering shops.
var (
	aliveMu    sync.Mutex
	aliveCache = map[string]aliveEntry{}
)

type aliveEntry struct {
	alive bool
	at    time.Time
}

const aliveTTL = 10 * time.Minute

func urlAlive(ctx context.Context, u string) bool {
	aliveMu.Lock()
	if e, ok := aliveCache[u]; ok && time.Since(e.at) < aliveTTL {
		aliveMu.Unlock()
		return e.alive
	}
	aliveMu.Unlock()
	alive := probeURL(ctx, u)
	aliveMu.Lock()
	aliveCache[u] = aliveEntry{alive: alive, at: time.Now()}
	aliveMu.Unlock()
	return alive
}

// probeURL decides listing liveness. Principle: a deal is only dead on
// PROOF — 404/410 from a GET, or total network failure. Bot walls answer
// probes with 403/429/500 (eBay does all three depending on method); treating
// those as dead would hide every marketplace deal.
func probeURL(ctx context.Context, u string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return false
		}
		browserHeaders(req)
		resp, err := httpClient.Do(req)
		if err != nil {
			continue // try GET; both failing = network-dead below
		}
		resp.Body.Close()
		ok := resp.StatusCode >= 200 && resp.StatusCode < 400
		if method == http.MethodHead && !ok {
			continue // HEAD is unreliable (bot walls 500/403 it) — GET decides
		}
		switch {
		case ok:
			return true
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			return false // definitive: listing is gone
		default:
			return true // 403/429/5xx/challenges — ambiguous, never kill a deal on ambiguity
		}
	}
	return false // network-level failure on both methods
}

// substituteMatch reports whether candidate can drop into the same build slot as
// orig: same category, and any constraining attribute the original fixes must
// match. ponytail: "similar performance" is approximated by same category +
// attribute compatibility — there are no benchmark scores in the store. Add a
// perf field + score comparison when substitution needs true perf parity.
func substituteMatch(orig, cand Part) bool {
	if cand.Category != orig.Category || cand.ID == orig.ID {
		return false
	}
	if orig.Socket != "" && cand.Socket != "" && orig.Socket != cand.Socket {
		return false
	}
	if orig.MemType != "" && cand.MemType != "" && orig.MemType != cand.MemType {
		return false
	}
	if orig.FormFactor != "" && cand.FormFactor != "" && orig.FormFactor != cand.FormFactor {
		return false
	}
	return true
}
