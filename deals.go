package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Listing is a price observation for a part from one vendor/reseller. Prices
// die fast: SeenAt drives the staleness flag; there is no live-price guarantee.
type Listing struct {
	ID        string    `json:"id,omitempty"` // derived from part+vendor+condition+url if omitted
	PartID    string    `json:"part_id"`
	Vendor    string    `json:"vendor,omitempty"` // seller, e.g. "ebay:seller123", "lenovo"
	Price     float64   `json:"price" jsonschema:"PER-UNIT price as listed. A lot/bundle price recorded as one unit poisons every ranking — divide it out and set qty_available. Skip teaser 'from X' prices entirely"`
	Shipping  *float64  `json:"shipping,omitempty" jsonschema:"shipping cost in the listing currency; 0 = FREE shipping, omit = unknown (flagged, so record 0 explicitly when the page says free)"`
	Currency  string    `json:"currency" jsonschema:"ISO code of price/shipping — required; a price without its currency cannot be compared"`
	Condition string    `json:"condition,omitempty"` // new, used, refurbished
	URL       string    `json:"url,omitempty"`
	ShipsTo   []string  `json:"ships_to,omitempty" jsonschema:"ISO alpha-2 country codes, 'EU', or 'WORLD'; empty = unknown"`
	SeenAt    time.Time `json:"seen_at,omitempty"`

	// VAT basis. Consumer marketplaces list incl-VAT, B2B resellers ex-VAT —
	// comparing the two raw skews picks by the VAT rate. nil = the page didn't
	// say (flagged, never guessed).
	VATIncluded *bool   `json:"vat_included,omitempty" jsonschema:"whether the price includes VAT; omit when the listing doesn't say — never guess"`
	VATRate     float64 `json:"vat_rate,omitempty" jsonschema:"VAT percent of the listing's country, e.g. 25 for DK, 19 for DE"`

	// Availability. Zero/nil = unknown; unknowns are never penalized.
	QtyAvailable int   `json:"qty_available,omitempty" jsonschema:"units available at this price (auction/lot size); 0 = unknown"`
	InStock      *bool `json:"in_stock,omitempty" jsonschema:"whether the item is in stock; omit when unknown"`
	LeadDays     int   `json:"lead_days,omitempty" jsonschema:"delivery/lead time in days when stated"`

	// Derived on read, not stored. Deals are never dropped for these — they
	// are flagged and sorted below usable ones, so nothing is hidden.
	AgeDays         int     `json:"age_days,omitempty"` // days since the price was seen — judge freshness yourself, don't wait for the stale flag
	Stale           bool    `json:"stale,omitempty"`
	Dead            bool    `json:"dead,omitempty"`             // URL no longer reachable (live-check)
	Unshippable     bool    `json:"unshippable,omitempty"`      // doesn't ship to the region
	VATUnknown      bool    `json:"vat_unknown,omitempty"`      // VAT basis not recorded — total may be ±VAT vs peers
	Unconverted     bool    `json:"unconverted,omitempty"`      // couldn't convert to the display currency — total is NATIVE, not comparable to peers
	ShippingUnknown bool    `json:"shipping_unknown,omitempty"` // shipping not recorded — total may understate the real cost
	DisplayTotal    float64 `json:"display_total,omitempty"`    // total converted to display currency
	DisplayExVAT    float64 `json:"display_ex_vat,omitempty"`   // ex-VAT total converted (when VAT basis known)
	DisplayCurr     string  `json:"display_currency,omitempty"`
}

// usable = clickable and buyable right now: reachable, ships to the region,
// and not explicitly out of stock (unknown stock stays usable).
func (l Listing) usable() bool {
	return !l.Dead && !l.Unshippable && (l.InStock == nil || *l.InStock)
}

func markShippable(ls []Listing, country string) {
	for i := range ls {
		ls[i].Unshippable = !shipsTo(ls[i].ShipsTo, country)
	}
}

func (l Listing) total() float64 {
	if l.Shipping != nil {
		return l.Price + *l.Shipping
	}
	return l.Price // shipping unknown — flagged via ShippingUnknown, never guessed
}

// effectiveTotal is the converted total when available, else the native total.
// Used for cross-currency sorting/comparison.
func (l Listing) effectiveTotal() float64 {
	if l.DisplayTotal > 0 {
		return l.DisplayTotal
	}
	return l.total()
}

// exVATTotal returns the listing total excluding VAT when the VAT basis is
// known: ex-VAT prices pass through, incl-VAT prices are divided out by the
// rate. Unknown basis (nil), or incl-VAT with no rate, is not computable.
func (l Listing) exVATTotal() (float64, bool) {
	if l.VATIncluded == nil {
		return 0, false
	}
	if !*l.VATIncluded {
		return l.total(), true
	}
	if l.VATRate <= 0 {
		return 0, false
	}
	return l.total() / (1 + l.VATRate/100), true
}

// comparisonTotal is the ranking figure: the ex-VAT converted total when the
// VAT basis is known (the business-buyer basis — VAT is deducted), else the
// gross total. Unknown-VAT listings therefore compare by gross, which can only
// OVERestimate them vs known-ex-VAT peers, never sneak them ahead; they carry
// the VATUnknown flag so the skew is visible.
func (l Listing) comparisonTotal() float64 {
	if l.DisplayExVAT > 0 {
		return l.DisplayExVAT
	}
	return l.effectiveTotal()
}

// stalenessDays: a listing older than this is flagged stale. The default —
// hardware prices move faster than 14d, so every listing also carries AgeDays
// and the caller can tighten via the max_age_days tool arg.
const stalenessDays = 14

// markStale flags listings older than maxDays (<=0 = the 14d default) and
// stamps every listing's age — a 13-day-old price must not read as current
// just because it hasn't crossed the flag threshold.
func markStale(ls []Listing, now time.Time, maxDays int) {
	if maxDays <= 0 {
		maxDays = stalenessDays
	}
	cut := now.AddDate(0, 0, -maxDays)
	for i := range ls {
		ls[i].Stale = ls[i].SeenAt.Before(cut)
		if !ls[i].SeenAt.IsZero() {
			ls[i].AgeDays = int(now.Sub(ls[i].SeenAt).Hours() / 24)
		}
	}
}

// sortListings orders usable (live + shippable + in stock) first, then
// converted-comparable before unconverted (a native SEK total sorted against
// DKK totals is meaningless — sink it, flagged, rather than misrank it), then
// cheapest by comparisonTotal (ex-VAT basis when known); ties broken by most
// recent. Dead/unshippable/out-of-stock listings sink to the bottom but are
// never removed.
func sortListings(ls []Listing) {
	sort.SliceStable(ls, func(i, j int) bool {
		if ls[i].usable() != ls[j].usable() {
			return ls[i].usable()
		}
		if ls[i].Unconverted != ls[j].Unconverted {
			return !ls[i].Unconverted
		}
		if ls[i].comparisonTotal() != ls[j].comparisonTotal() {
			return ls[i].comparisonTotal() < ls[j].comparisonTotal()
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
		if currency != "" && l.Currency != currency {
			if l.Currency == "" {
				continue // unknown currency can't be ranked against converted totals
			}
			c, _, err := convert(ctx, t, l.Currency, currency)
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
	// Cautions are pairwise differences vs the original that a same-category
	// match can't clear on its own — extra resource demands, bigger physical
	// footprint, higher draw. Flags, never filters: whether the build absorbs
	// them is checked for real when spec_id is passed.
	Cautions []string `json:"cautions,omitempty"`
	// SpecViolations: with spec_id, the violations this candidate INTRODUCES
	// when swapped into that build (empty = verified drop-in).
	SpecViolations []Violation `json:"spec_violations,omitempty"`
}

// substituteCautions compares a candidate against the original on the axes a
// category+attribute match can't see: resource demands (an extra power cable
// the build may not have spare), physical size, and draw. Unknowns stay
// silent — same convention as the rules.
func substituteCautions(orig, cand Part) []string {
	var out []string
	for tok, n := range cand.Requires {
		if n > orig.Requires[tok] {
			out = append(out, fmt.Sprintf("requires %d x %q vs original's %d — the build needs that spare", n, tok, orig.Requires[tok]))
		}
	}
	for tok, n := range orig.Provides {
		if have := cand.Provides[tok]; have < n {
			out = append(out, fmt.Sprintf("provides only %d x %q vs original's %d — something downstream may lose its slot/port", have, tok, n))
		}
	}
	if orig.LengthMM > 0 && cand.LengthMM > orig.LengthMM {
		out = append(out, fmt.Sprintf("longer than the original (%dmm vs %dmm) — clearance verified for the original may not hold", cand.LengthMM, orig.LengthMM))
	}
	if orig.TDPW > 0 && cand.TDPW > orig.TDPW {
		out = append(out, fmt.Sprintf("draws %dW more than the original — recheck PSU headroom", cand.TDPW-orig.TDPW))
	}
	sort.Strings(out) // map iteration order must not shuffle the report
	return out
}

// annotateDisplay fills DisplayTotal/DisplayExVAT/DisplayCurr by converting
// each listing's totals into the display currency, and flags listings whose
// VAT basis, shipping, or conversion is missing. Best-effort: a conversion
// error never fails the call, but it MUST flag — an unconverted native total
// silently compared against converted peers is a misranking, not a fallback.
func annotateDisplay(ctx context.Context, ls []Listing, display string) {
	for i := range ls {
		ls[i].VATUnknown = ls[i].VATIncluded == nil
		ls[i].ShippingUnknown = ls[i].Shipping == nil
		if display == "" {
			continue
		}
		if ls[i].Currency == "" {
			ls[i].Unconverted = true // saved before currency became required
			continue
		}
		if v, _, err := convert(ctx, ls[i].total(), ls[i].Currency, display); err == nil {
			ls[i].DisplayTotal = v
			ls[i].DisplayCurr = display
		} else {
			ls[i].Unconverted = true
			continue
		}
		if ex, ok := ls[i].exVATTotal(); ok {
			if v, _, err := convert(ctx, ex, ls[i].Currency, display); err == nil {
				ls[i].DisplayExVAT = v
			}
		}
	}
}

// prewarmLiveness probes every listing URL across a set of parts in one
// concurrent batch, populating the alive cache. Pricing each part afterwards
// then hits the cache instead of serially probing — turning N sequential
// per-part probe rounds into one parallel sweep. Best-effort.
func prewarmLiveness(ctx context.Context, partIDs []string) {
	var all []Listing
	seen := map[string]bool{}
	for _, id := range partIDs {
		if seen[id] {
			continue // repeated (quantity) ids share listings
		}
		seen[id] = true
		ls, err := store.listingsFor(id)
		if err != nil {
			continue
		}
		all = append(all, ls...)
	}
	liveCheckAll(ctx, all)
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
			defer func() { <-sem }() // release slot even if the probe panics
			defer recoverLog("liveCheck")
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
// PROOF — 404/410 from a GET, a redirect that lands on the site root (the
// standard "listing ended" soft-404: deep item path in, homepage out), or
// total network failure. Bot walls answer probes with 403/429/500 (eBay does
// all three depending on method); treating those as dead would hide every
// marketplace deal.
func probeURL(ctx context.Context, u string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	origPath := "/"
	if pu, err := url.Parse(u); err == nil && pu.Path != "" {
		origPath = pu.Path
	}
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		resp, err := doRequest(ctx, method, u, nil)
		if err != nil {
			continue // try GET; both failing = network-dead below
		}
		resp.Body.Close()
		// Redirect chains are followed by the client; a deep listing URL that
		// lands on "/" is the marketplace saying "this listing is gone" with a
		// 200. Challenge pages keep a path, so bot walls don't trip this.
		if resp.Request != nil && resp.Request.URL != nil &&
			len(origPath) > 1 && (resp.Request.URL.Path == "/" || resp.Request.URL.Path == "") {
			return false
		}
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
	// A deadline/cancellation (10s budget spent on gate queue + retries, or the
	// parent request ending) is NOT proof the listing is gone — killing it here
	// would drop a live deal from the totals. Only a genuine transport failure
	// with time left counts as dead.
	if ctx.Err() != nil {
		return true
	}
	return false // network-level failure on both methods
}

// priceOutlierFactor: a new price this many times above/below the part's
// median recorded price smells like a unit error, not a deal.
const priceOutlierFactor = 8

// priceSanityWarning cross-checks a new listing price against the part's
// recorded history (converted to the listing's currency, best-effort). Returns
// a warning string for order-of-magnitude outliers — the caller saves anyway;
// this is a "check your extraction" nudge, not a gate.
func priceSanityWarning(ctx context.Context, l Listing) string {
	obs, err := store.priceHistory(l.PartID)
	if err != nil || len(obs) == 0 {
		return ""
	}
	var prices []float64
	for _, o := range obs {
		p := o.Price
		if o.Currency != "" && !strings.EqualFold(o.Currency, l.Currency) {
			c, _, cerr := convert(ctx, p, o.Currency, l.Currency)
			if cerr != nil {
				continue
			}
			p = c
		}
		if p > 0 {
			prices = append(prices, p)
		}
	}
	if len(prices) == 0 {
		return ""
	}
	sort.Float64s(prices)
	median := prices[len(prices)/2]
	switch {
	case l.Price > median*priceOutlierFactor:
		return fmt.Sprintf("price %.2f %s is >%dx the part's median recorded price (%.2f) — check for a lot/bundle price recorded as one unit or a decimal-separator misread", l.Price, l.Currency, priceOutlierFactor, median)
	case l.Price < median/priceOutlierFactor:
		return fmt.Sprintf("price %.2f %s is <1/%d of the part's median recorded price (%.2f) — check for a teaser/'from' price, a part-only accessory, or a decimal-separator misread", l.Price, l.Currency, priceOutlierFactor, median)
	}
	return ""
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
	// RDIMM/UDIMM/LRDIMM: a substitute of the wrong module type won't boot —
	// the same trap the ram_module_type rule catches in builds.
	if om, ok := flattenStr(orig, "mem_module"); ok {
		if cm, cok := flattenStr(cand, "mem_module"); cok && !strings.EqualFold(om, cm) {
			return false
		}
	}
	return true
}
