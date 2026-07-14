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

func ratesFor(ctx context.Context, base string) (map[string]float64, error) {
	base = strings.ToUpper(base)
	fxMu.Lock()
	rs, ok := fxCache[base]
	fxMu.Unlock()
	if ok && time.Since(rs.fetched) < fxTTL {
		return rs.rates, nil
	}
	// On any fetch failure below, an EXPIRED cached set still beats no rates:
	// hours-stale reference rates rank deals fine; a conversion failure would
	// silently drop listings out of cross-currency comparison instead.
	staleOr := func(err error) (map[string]float64, error) {
		if ok {
			fmt.Fprintf(os.Stderr, "parts-finder: fx refresh for %s failed (%v), using rates from %s\n", base, err, rs.fetched.Format(time.RFC3339))
			return rs.rates, nil
		}
		return nil, err
	}
	u := "https://api.frankfurter.app/latest?base=" + url.QueryEscape(base)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return staleOr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return staleOr(fmt.Errorf("frankfurter returned %s", resp.Status))
	}
	var body struct {
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return staleOr(err)
	}
	if body.Rates == nil { // 200 with no rates (unknown base) — nil-map write would panic
		return nil, fmt.Errorf("frankfurter returned no rates for %s", base)
	}
	body.Rates[base] = 1 // base->base
	fxMu.Lock()
	fxCache[base] = rateSet{rates: body.Rates, fetched: time.Now()}
	fxMu.Unlock()
	return body.Rates, nil
}

// convert returns amount in `from` currency expressed in `to`. Returns an error
// if either currency is unknown to the rate source — or empty: silently
// treating an unlabeled amount as already-converted is how a SEK price ends up
// ranked as DKK.
func convert(ctx context.Context, amount float64, from, to string) (float64, error) {
	from, to = strings.ToUpper(from), strings.ToUpper(to)
	if from == "" || to == "" {
		return 0, fmt.Errorf("convert: missing currency (from=%q to=%q)", from, to)
	}
	if from == to {
		return amount, nil
	}
	rates, err := ratesFor(ctx, from)
	if err != nil {
		return 0, err
	}
	rate, ok := rates[to]
	if !ok {
		return 0, fmt.Errorf("no rate %s->%s", from, to)
	}
	return amount * rate, nil
}
