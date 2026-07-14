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

	"golang.org/x/text/currency"
	"golang.org/x/text/language"
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

// geoIPProviders: independent keyless https services that report the
// caller's country, tried in order — first valid answer wins. The field name
// is the JSON key carrying the ISO alpha-2 code.
var geoIPProviders = []struct{ url, field string }{
	{"https://ifconfig.co/json", "country_iso"},
	{"https://ipinfo.io/json", "country"},
}

// lookupIP asks the geo-IP provider chain for the caller's country; currency
// is derived from the ISO table below. Each provider gets its own short cap:
// detection blocks whichever tool call triggers it (and, via regionMu, any
// concurrent ones) — one hung provider must not spend the whole budget.
func lookupIP(ctx context.Context) Region {
	for _, p := range geoIPProviders {
		if cc := fetchCountry(ctx, p.url, p.field); cc != "" {
			return Region{Country: cc, Currency: currencyOf(cc)}
		}
	}
	return Region{}
}

func fetchCountry(ctx context.Context, u, field string) string {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var v map[string]any
	if json.NewDecoder(resp.Body).Decode(&v) != nil {
		return ""
	}
	cc, _ := v[field].(string)
	if len(cc) != 2 {
		return ""
	}
	return strings.ToUpper(cc)
}

// currencyOf maps ISO country -> ISO currency via CLDR (x/text) — the full
// standards table, not a hand-picked subset, so a Thai or Israeli user gets
// THB/ILS figures instead of a silent USD fallback. Unknown/unparseable
// countries fall back to USD. Note: frankfurter carries only ECB reference
// currencies — a region whose currency it lacks (e.g. TWD) gets honest
// `unconverted` flags on cross-currency listings; override with
// REGION_CURRENCY to pick a convertible display currency instead.
func currencyOf(country string) string {
	r, err := language.ParseRegion(country)
	if err != nil {
		return "USD"
	}
	if u, ok := currency.FromRegion(r); ok {
		return u.String()
	}
	return "USD"
}

// ddgRegion derives DuckDuckGo's kl locale param ("dk-da") from the country's
// CLDR likely language — data-driven, so every country DDG knows gets a locale
// bias, not just a hand-maintained few. Unknown -> "" (DDG ignores unknown kl
// values, matching the old no-locale behavior).
func ddgRegion(country string) string {
	if country == "" {
		return ""
	}
	r, err := language.ParseRegion(strings.ToUpper(country))
	if err != nil {
		return ""
	}
	tag, err := language.Compose(r)
	if err != nil {
		return ""
	}
	base, conf := tag.Base() // infers the likely language from the region
	if conf == language.No {
		return ""
	}
	cc, lang := strings.ToLower(country), base.String()
	// Standards quirks, not vendor data: DDG says "uk" for Great Britain, and
	// uses the macrolanguage code "no" where CLDR says Bokmål ("nb").
	if cc == "gb" {
		cc = "uk"
	}
	if lang == "nb" || lang == "nn" {
		lang = "no"
	}
	return cc + "-" + lang
}

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
		// Common real-world aliases — a listing saved with "UK" or "Europe"
		// must not read as unshippable to GB/DK.
		if a, ok := ccTLDAlias[strings.ToLower(u)]; ok {
			u = a
		}
		switch u {
		case "WORLD", "WORLDWIDE", "GLOBAL", "*":
			return true
		case "EU", "EUROPE", "EUROPEAN UNION":
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
