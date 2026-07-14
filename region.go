package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Region is the user's locale, used to bias search toward local vendors and to
// pick a default display currency. Detected once from the caller's IP (or env
// override) and cached for the process lifetime.
type Region struct {
	Country  string `json:"country"`    // ISO-3166 alpha-2, e.g. "DK"
	Currency string `json:"currency"`   // ISO-4217, e.g. "DKK"
	DDG      string `json:"ddg_region"` // DuckDuckGo kl param, e.g. "dk-da"
}

var (
	regionMu    sync.Mutex
	regionVal   Region
	regionSet   bool
	regionRetry time.Time // after a failed lookup, don't retry before this
)

// detectRegion resolves the region. Env REGION_COUNTRY / REGION_CURRENCY
// override IP detection; missing currency is derived from the country. Only a
// SUCCESSFUL detection is cached — a failed IP lookup (offline start) is
// retried instead of locking an empty region in for the process lifetime, but
// on a 1-minute backoff: regionMu serializes callers, so retrying on EVERY
// call would stall every tool behind repeated lookups while offline. An empty
// Region means no bias and a USD display fallback; it's visible in every
// tool's region output.
func detectRegion(ctx context.Context) Region {
	regionMu.Lock()
	defer regionMu.Unlock()
	if regionSet {
		return regionVal
	}
	r := Region{
		Country:  strings.ToUpper(os.Getenv("REGION_COUNTRY")),
		Currency: strings.ToUpper(os.Getenv("REGION_CURRENCY")),
	}
	if r.Country == "" && time.Now().After(regionRetry) {
		r.Country = lookupIP(ctx).Country
		if r.Country == "" {
			regionRetry = time.Now().Add(time.Minute)
		}
	}
	if r.Currency == "" && r.Country != "" {
		r.Currency = currencyOf(r.Country)
	}
	r.DDG = ddgRegion(r.Country)
	regionVal = r
	regionSet = r.Country != ""
	return r
}

// lookupIP asks a keyless https geo-IP service for the caller's country;
// currency is derived from the ISO table below. ponytail: single provider
// (ifconfig.co, https, no key); detection is cached for the process so one
// call per run. Add a fallback provider only if it proves flaky.
func lookupIP(ctx context.Context) Region {
	// Own 5s cap: detection blocks whichever tool call triggers it (and, via
	// regionMu, any concurrent ones) — never for the full client timeout.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://ifconfig.co/json", nil)
	if err != nil {
		return Region{}
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return Region{}
	}
	defer resp.Body.Close()
	var v struct {
		CountryISO string `json:"country_iso"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil {
		return Region{}
	}
	cc := strings.ToUpper(v.CountryISO)
	return Region{Country: cc, Currency: currencyOf(cc)}
}

// currencyOf maps ISO country -> ISO currency. Standards data, not vendor
// preference: non-euro currencies listed explicitly, EU members default to
// EUR, everything else to USD (safe display fallback — override with
// REGION_CURRENCY).
var countryCurrency = map[string]string{
	"DK": "DKK", "SE": "SEK", "NO": "NOK", "IS": "ISK", "CH": "CHF",
	"GB": "GBP", "PL": "PLN", "CZ": "CZK", "HU": "HUF", "RO": "RON",
	"BG": "BGN", "US": "USD", "CA": "CAD", "AU": "AUD", "NZ": "NZD",
	"JP": "JPY", "CN": "CNY", "KR": "KRW", "IN": "INR", "BR": "BRL",
	"MX": "MXN", "SG": "SGD", "HK": "HKD", "TR": "TRY",
	// TW deliberately absent: frankfurter (ECB reference rates) carries no
	// TWD, so a TWD default would make every conversion fail. TW falls back
	// to USD; override with REGION_CURRENCY if you accept unconvertible totals.
}

func currencyOf(country string) string {
	if c, ok := countryCurrency[country]; ok {
		return c
	}
	if euCountries[country] {
		return "EUR"
	}
	return "USD"
}

// ddgRegion maps a country to DuckDuckGo's kl locale param. Unknown -> "".
var ddgLang = map[string]string{
	"DK": "dk-da", "SE": "se-sv", "NO": "no-no", "FI": "fi-fi",
	"DE": "de-de", "NL": "nl-nl", "GB": "uk-en", "US": "us-en",
	"FR": "fr-fr", "ES": "es-es", "IT": "it-it", "PL": "pl-pl",
}

func ddgRegion(country string) string { return ddgLang[strings.ToUpper(country)] }

// euCountries lets ships-to "EU" match any member.
var euCountries = map[string]bool{
	"AT": true, "BE": true, "BG": true, "HR": true, "CY": true, "CZ": true,
	"DK": true, "EE": true, "FI": true, "FR": true, "DE": true, "GR": true,
	"HU": true, "IE": true, "IT": true, "LV": true, "LT": true, "LU": true,
	"MT": true, "NL": true, "PL": true, "PT": true, "RO": true, "SK": true,
	"SI": true, "ES": true, "SE": true,
}

// ccTLDAlias maps DNS ccTLDs that differ from their ISO-3166 country code.
var ccTLDAlias = map[string]string{"uk": "GB"}

// tldCountry returns the ISO country a host's TLD belongs to, or "" for generic
// TLDs (.com/.net/.org/.eu/...) and unknowns. Purely structural — no vendor
// knowledge.
func tldCountry(host string) string {
	i := strings.LastIndex(host, ".")
	if i < 0 {
		return ""
	}
	tld := host[i+1:]
	if len(tld) != 2 { // ccTLDs are 2 letters; .com/.info/etc are not
		return ""
	}
	if a, ok := ccTLDAlias[tld]; ok {
		return a
	}
	return strings.ToUpper(tld)
}

// rankScore ranks a hit by geographic proximity to the region plus vendors we've
// actually transacted with (learned from the store). Lower sorts first.
// 0 = local / known-good, 1 = neutral, 2 = clearly foreign. No hardcoded vendor
// list: preference is derived from the domain's TLD and accrued listing data.
func rankScore(hitURL string, r Region, known map[string]bool) int {
	host := hostOf(hitURL)
	if host == "" {
		return 1
	}
	if known[host] { // a vendor we've stored a region-shippable listing from
		return 0
	}
	if r.Country == "" {
		return 1
	}
	cc := tldCountry(host)
	switch {
	case cc == r.Country: // local ccTLD, e.g. .dk in DK
		return 0
	case strings.HasSuffix(host, ".eu") && euCountries[r.Country]: // pan-EU vendor
		return 0
	case cc == "": // generic TLD (.com/.net/...) — can't place it, stay neutral
		return 1
	case euCountries[cc] && euCountries[r.Country]: // EU neighbour, still close
		return 1
	default: // a foreign country's ccTLD
		return 2
	}
}

// rankHits stably reorders hits to surface local / known-good vendors first,
// without dropping anything (bias, not filter). `known` is the set of vendor
// domains learned from stored listings that ship to the region.
func rankHits(hits []SearchHit, r Region, known map[string]bool) {
	if r.Country == "" && len(known) == 0 {
		return
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return rankScore(hits[i].URL, r, known) < rankScore(hits[j].URL, r, known)
	})
}

func hostOf(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimPrefix(s, "www."))
}

// shipsTo reports whether a listing with the given ships-to tokens reaches the
// region. Empty tokens = unknown => assume yes (don't over-filter).
func shipsTo(tokens []string, country string) bool {
	if len(tokens) == 0 || country == "" {
		return true
	}
	for _, t := range tokens {
		u := strings.ToUpper(strings.TrimSpace(t))
		switch u {
		case "WORLD", "WORLDWIDE", "GLOBAL", "*":
			return true
		case "EU":
			if euCountries[country] {
				return true
			}
		default:
			if u == country {
				return true
			}
		}
	}
	return false
}
