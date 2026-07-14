package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Open Icecat (icecat.biz) is a free, brand-authorized catalog of ~18M product
// datasheets — normalized spec tables plus links to the vendor's own PDF
// datasheets/manuals. deep_specs pulls the structured JSON record directly:
// an independent, vendor-authorized source for cross-examining scraped specs.
//
// ZERO CONFIG: Icecat's public open-content user "openicecat-live" works
// without any registration (verified live 2026-07-13: full records for
// HP/Lenovo server gear). ICECAT_USER overrides it for a personal/full
// account with wider catalog access.
//
// API: https://live.icecat.biz/api?UserName=<u>&Language=en&GTIN=<ean>
//
//	or ...&Brand=<vendor>&ProductCode=<mpn/model>
//
// The API is exact-lookup (GTIN/EAN or brand+MPN), not free-text search —
// "searchable" comes from the pipeline: web search surfaces the identifiers
// (listings print EANs, spec pages print MPNs), save them as attrs, and the
// lookup goes exact from then on. Open content covers sponsoring brands
// (HP/HPE, Dell, Lenovo, ... — hosting gear is well covered); misses are
// normal and fall through to web search, never silently swallowed.
const icecatPublicUser = "openicecat-live"

type icecatResp struct {
	Msg  string `json:"msg"`
	Data struct {
		GeneralInfo struct {
			Title string `json:"Title"`
		} `json:"GeneralInfo"`
		Multimedia []struct {
			URL         string `json:"URL"`
			Type        string `json:"Type"`
			Description string `json:"Description"`
		} `json:"Multimedia"`
		FeaturesGroups []struct {
			FeatureGroup struct {
				Name struct {
					Value string `json:"Value"`
				} `json:"Name"`
			} `json:"FeatureGroup"`
			Features []struct {
				Feature struct {
					Name struct {
						Value string `json:"Value"`
					} `json:"Name"`
				} `json:"Feature"`
				PresentationValue string `json:"PresentationValue"`
			} `json:"Features"`
		} `json:"FeaturesGroups"`
	} `json:"data"`
}

// icecatQuery builds the API URL for a part, preferring an exact GTIN/EAN
// attr over brand+product-code. Empty when the part has nothing to look up by.
func icecatQuery(p Part) string {
	user := os.Getenv("ICECAT_USER")
	if user == "" {
		user = icecatPublicUser
	}
	base := "https://live.icecat.biz/api?UserName=" + url.QueryEscape(user) + "&Language=en"
	for _, key := range []string{"gtin", "ean", "upc"} {
		if v, ok := flattenStr(p, key); ok {
			return base + "&GTIN=" + url.QueryEscape(v)
		}
	}
	mpn := p.Model
	if v, ok := flattenStr(p, "mpn"); ok {
		mpn = v // exact manufacturer part number beats a display model name
	}
	if p.Vendor == "" || mpn == "" {
		return ""
	}
	return base + "&Brand=" + url.QueryEscape(p.Vendor) + "&ProductCode=" + url.QueryEscape(mpn)
}

// icecatSource fetches a part's Open Icecat record and renders it as a
// deep_specs source: spec table as markdown plus the vendor PDF URLs to
// fetch_content next. A catalog MISS returns (_, false, nil) and deep_specs
// carries on with web search; a transport/server failure returns an error —
// "couldn't check the brand-authorized source" must never masquerade as
// "the part isn't in the catalog".
func icecatSource(ctx context.Context, p Part) (deepSource, bool, error) {
	u := icecatQuery(p)
	if u == "" {
		return deepSource{}, false, nil
	}
	resp, err := doRequest(ctx, http.MethodGet, u, map[string]string{"Accept": "application/json"})
	if err != nil {
		return deepSource{}, false, fmt.Errorf("icecat: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return deepSource{}, false, nil // not in Open Icecat / brand not open-content — a legit miss
	default:
		return deepSource{}, false, fmt.Errorf("icecat returned %s", resp.Status)
	}
	var body icecatResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return deepSource{}, false, fmt.Errorf("icecat: decode: %w", err)
	}
	src, ok := renderIcecat(u, body)
	return src, ok, nil
}

// renderIcecat turns an Icecat record into readable source text. Pure —
// testable without network.
func renderIcecat(srcURL string, r icecatResp) (deepSource, bool) {
	if !strings.EqualFold(r.Msg, "OK") {
		return deepSource{}, false
	}
	var b strings.Builder
	for _, g := range r.Data.FeaturesGroups {
		if len(g.Features) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", g.FeatureGroup.Name.Value)
		b.WriteString("| Feature | Value |\n| --- | --- |\n")
		for _, f := range g.Features {
			fmt.Fprintf(&b, "| %s | %s |\n", f.Feature.Name.Value, f.PresentationValue)
		}
		b.WriteString("\n")
	}
	var pdfs []string
	for _, m := range r.Data.Multimedia {
		if strings.HasSuffix(strings.ToLower(m.URL), ".pdf") ||
			strings.Contains(strings.ToLower(m.Type), "pdf") {
			label := m.Description
			if label == "" {
				label = m.Type
			}
			pdfs = append(pdfs, fmt.Sprintf("- %s (%s)", m.URL, label))
		}
	}
	if len(pdfs) > 0 {
		b.WriteString("## Vendor PDF datasheets — fetch_content these for full detail\n\n")
		b.WriteString(strings.Join(pdfs, "\n"))
		b.WriteString("\n")
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return deepSource{}, false // record exists but carries nothing usable
	}
	return deepSource{
		URL:   srcURL,
		Title: "Icecat (brand-authorized datasheet): " + r.Data.GeneralInfo.Title,
		Text:  text,
	}, true
}
