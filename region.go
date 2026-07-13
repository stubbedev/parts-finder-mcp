package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
)

// Region is the user's locale, used to bias search toward local vendors and to
// pick a default display currency. Detected once from the caller's IP (or env
// override) and cached for the process lifetime.
type Region struct {
	Country  string `json:"country"`   // ISO-3166 alpha-2, e.g. "DK"
	Currency string `json:"currency"`  // ISO-4217, e.g. "DKK"
	DDG      string `json:"ddg_region"`// DuckDuckGo kl param, e.g. "dk-da"
}

var (
	regionOnce sync.Once
	regionVal  Region
)

// detectRegion resolves the region once. Env REGION_COUNTRY / REGION_CURRENCY
// override IP detection; if both detection and env are absent it returns an
// empty Region (no bias, USD display fallback handled by callers).
func detectRegion(ctx context.Context) Region {
	regionOnce.Do(func() {
		regionVal = Region{
			Country:  strings.ToUpper(os.Getenv("REGION_COUNTRY")),
			Currency: strings.ToUpper(os.Getenv("REGION_CURRENCY")),
		}
		if regionVal.Country == "" || regionVal.Currency == "" {
			if ipr := lookupIP(ctx); ipr.Country != "" {
				if regionVal.Country == "" {
					regionVal.Country = ipr.Country
				}
				if regionVal.Currency == "" {
					regionVal.Currency = ipr.Currency
				}
			}
		}
		regionVal.DDG = ddgRegion(regionVal.Country)
	})
	return regionVal
}

// lookupIP asks a keyless geo-IP service for the caller's country + currency.
// ponytail: single provider (ip-api.com, no key, 45 req/min). Add a fallback
// provider only if it proves flaky.
func lookupIP(ctx context.Context) Region {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://ip-api.com/json/?fields=countryCode,currency", nil)
	if err != nil {
		return Region{}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Region{}
	}
	defer resp.Body.Close()
	var v struct {
		CountryCode string `json:"countryCode"`
		Currency    string `json:"currency"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil {
		return Region{}
	}
	return Region{Country: strings.ToUpper(v.CountryCode), Currency: strings.ToUpper(v.Currency)}
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

// euServerResellers are trusted EU refurb/enterprise vendors to boost regardless
// of ccTLD. Edit freely as new vendors surface.
var euServerResellers = []string{
	"server-parts.eu", "servershop24.de", "serverando.de", "interbolt.eu",
	"secondhandserver.eu", "servermall.com", "renewtech.com",
	"directhardwaresupply.com", "pcserverandparts.com", "gekko-computer.de",
}

// usOnlyVendors get demoted for non-US regions (ship poorly / wrong currency).
var usOnlyVendors = []string{
	"newegg.com", "microcenter.com", "bhphotovideo.com", "walmart.com",
	"bestbuy.com", "provantage.com",
}

// rankScore: lower sorts first. 0 = local/preferred, 1 = neutral, 2 = demoted.
func rankScore(hitURL string, r Region) int {
	host := hostOf(hitURL)
	if host == "" {
		return 1
	}
	if r.Country != "" && strings.HasSuffix(host, "."+strings.ToLower(r.Country)) {
		return 0 // local ccTLD, e.g. .dk
	}
	for _, v := range euServerResellers {
		if strings.Contains(host, v) {
			if euCountries[r.Country] {
				return 0
			}
			return 1
		}
	}
	for _, v := range usOnlyVendors {
		if strings.Contains(host, v) && r.Country != "" && r.Country != "US" {
			return 2
		}
	}
	return 1
}

// rankHits stably reorders hits to surface local/preferred vendors first,
// without dropping anything (bias, not filter).
func rankHits(hits []SearchHit, r Region) {
	if r.Country == "" {
		return
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return rankScore(hits[i].URL, r) < rankScore(hits[j].URL, r)
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
