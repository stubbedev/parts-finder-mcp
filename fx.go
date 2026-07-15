package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Currency conversion for presenting a single guiding figure across listings in
// mixed currencies. Rates come from frankfurter.app (ECB reference rates,
// keyless). Rates move slowly; cache per base for a day.
// ponytail: these are indicative reference rates, not the rate you'll be
// charged — good enough for ranking deals, not for accounting.

type rateSet struct {
	rates   map[string]float64
	fetched time.Time
}

var (
	fxMu    sync.Mutex
	fxCache = map[string]rateSet{} // base currency -> rates
)

const fxTTL = 24 * time.Hour

// maxFXStale caps how old a fallback rate may be. Within this window an expired
// rate still ranks deals fine (ECB reference rates drift slowly); PAST it, a
// refresh failure must ERROR rather than silently convert on a weeks-old rate —
// bounded drift, never accounting-wrong figures with no signal.
const maxFXStale = 7 * 24 * time.Hour

// ratesFor returns the rate table for base plus staleAge: 0 when fresh, or the
// age of the served-stale fallback set (caller decides whether to surface it).
func ratesFor(ctx context.Context, base string) (rates map[string]float64, staleAge time.Duration, err error) {
	base = strings.ToUpper(base)
	fxMu.Lock()
	rs, ok := fxCache[base]
	fxMu.Unlock()
	if ok && time.Since(rs.fetched) < fxTTL {
		return rs.rates, 0, nil
	}
	// On any fetch failure below, an EXPIRED-BUT-RECENT cached set still beats no
	// rates: a conversion failure would silently drop listings out of
	// cross-currency comparison. But refuse a set older than maxFXStale.
	staleOr := func(err error) (map[string]float64, time.Duration, error) {
		if ok {
			age := time.Since(rs.fetched)
			if age > maxFXStale {
				return nil, 0, fmt.Errorf("fx rates for %s are %s old and refresh failed (%w) — refusing to convert on rates this stale", base, age.Round(time.Hour), err)
			}
			fmt.Fprintf(os.Stderr, "parts-finder: fx refresh for %s failed (%v), using rates from %s\n", base, err, rs.fetched.Format(time.RFC3339))
			return rs.rates, age, nil
		}
		return nil, 0, err
	}
	// One transient fetch failure here silently degrades every total in the
	// calling tool (listings flip to Unconverted for the whole call) — worth
	// one bounded retry before falling back to stale rates.
	u := "https://api.frankfurter.app/latest?base=" + url.QueryEscape(base)
	var body struct {
		Rates map[string]float64 `json:"rates"`
	}
	fetchOnce := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("frankfurter returned %s", resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(&body)
	}
	err = fetchOnce()
	if err != nil && ctx.Err() == nil {
		time.Sleep(500 * time.Millisecond)
		err = fetchOnce()
	}
	if err != nil {
		return staleOr(err)
	}
	if body.Rates == nil { // 200 with no rates (unknown base) — nil-map write would panic
		return nil, 0, fmt.Errorf("frankfurter returned no rates for %s", base)
	}
	body.Rates[base] = 1 // base->base
	fxMu.Lock()
	fxCache[base] = rateSet{rates: body.Rates, fetched: time.Now()}
	fxMu.Unlock()
	return body.Rates, 0, nil
}

// convert returns amount in `from` currency expressed in `to`. Returns an error
// if either currency is unknown to the rate source — or empty: silently
// treating an unlabeled amount as already-converted is how a SEK price ends up
// ranked as DKK.
func convert(ctx context.Context, amount float64, from, to string) (converted float64, staleAge time.Duration, err error) {
	from, to = strings.ToUpper(from), strings.ToUpper(to)
	if from == "" || to == "" {
		return 0, 0, fmt.Errorf("convert: missing currency (from=%q to=%q)", from, to)
	}
	if from == to {
		return amount, 0, nil
	}
	rates, staleAge, err := ratesFor(ctx, from)
	if err != nil {
		return 0, 0, err
	}
	rate, ok := rates[to]
	if !ok {
		return 0, 0, fmt.Errorf("no rate %s->%s", from, to)
	}
	return amount * rate, staleAge, nil
}
