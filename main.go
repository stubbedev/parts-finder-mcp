package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var store *Store

// version is stamped at link time (-X main.version=…); "dev" for local builds.
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("parts-finder " + version)
		return
	}
	dbPath := os.Getenv("PARTS_DB")
	if dbPath == "" {
		home, _ := os.UserHomeDir()
		dbPath = filepath.Join(home, ".parts-finder.db")
	}
	var err error
	store, err = openStore(dbPath)
	if err != nil {
		log.Fatalf("open store %s: %v", dbPath, err)
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "parts-finder", Version: version}, nil)
	registerTools(s)

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

// --- tool I/O types ---

type searchIn struct {
	Query    string `json:"query" jsonschema:"search query"`
	Category string `json:"category,omitempty" jsonschema:"hardware category to bias the query, e.g. cpu, motherboard"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max results (default 10)"`
}
type searchOut struct {
	Hits []SearchHit `json:"hits"`
}

type fetchIn struct {
	URL    string `json:"url" jsonschema:"page or spec-sheet URL to fetch"`
	Render bool   `json:"render,omitempty" jsonschema:"render with headless browser (lightpanda) for JS-heavy pages; requires LIGHTPANDA_URL"`
}
type fetchOut struct {
	Title  string `json:"title"`
	Text   string `json:"text"`
	Cached bool   `json:"cached"`
}

type savePartOut struct {
	ID string `json:"id"`
}

type idsIn struct {
	PartIDs []string `json:"part_ids" jsonschema:"stored part IDs to evaluate together"`
}

type queryIn struct {
	IDs      []string `json:"ids,omitempty" jsonschema:"exact part ids to fetch"`
	Category string   `json:"category,omitempty" jsonschema:"restrict to one category; empty = all"`
	Where    []Where  `json:"where,omitempty" jsonschema:"attribute clauses, ANDed"`
	Limit    int      `json:"limit,omitempty"`
}
type queryOut struct {
	Parts []Part `json:"parts"`
}

type compareIn struct {
	SpecIDs         []string `json:"spec_ids" jsonschema:"saved specs to compare side by side"`
	Country         string   `json:"country,omitempty"`
	DisplayCurrency string   `json:"display_currency,omitempty" jsonschema:"currency for totals; defaults to region currency"`
}
type specOption struct {
	ID            string      `json:"id"`
	Name          string      `json:"name,omitempty"`
	Compatible    bool        `json:"compatible"`
	Violations    []Violation `json:"violations,omitempty"`
	Gaps          []string    `json:"gaps,omitempty"`
	Needs         []Need      `json:"needs,omitempty"`
	TotalTDPW     int         `json:"total_tdp_w"`
	TotalBest     float64     `json:"total_best"` // sum of best usable listings, converted
	TotalCovers   int         `json:"total_covers"`
	PartCount     int         `json:"part_count"`
	BuyLinks      []string    `json:"buy_links,omitempty"` // best usable URL per covered part
	UncoveredIDs  []string    `json:"uncovered_ids,omitempty"`
}
type compareOut struct {
	Region        Region       `json:"region"`
	TotalCurrency string       `json:"total_currency,omitempty"`
	Options       []specOption `json:"options"`
}

type saveSpecIn struct {
	ID      string   `json:"id" jsonschema:"spec id (slug); reused id overwrites"`
	Name    string   `json:"name,omitempty"`
	PartIDs []string `json:"part_ids"`
}
type loadSpecIn struct {
	ID string `json:"id,omitempty" jsonschema:"spec id; omit to list all saved specs"`
}
type loadSpecOut struct {
	Spec  *Spec      `json:"spec,omitempty"`
	Specs []SpecInfo `json:"specs,omitempty"` // when listing
}

type dealsIn struct {
	PartID          string `json:"part_id"`
	Search          bool   `json:"search,omitempty" jsonschema:"also run a region-biased web search for buy pages to populate more listings"`
	Country         string `json:"country,omitempty" jsonschema:"override detected region (ISO alpha-2, e.g. DK)"`
	DisplayCurrency string `json:"display_currency,omitempty" jsonschema:"convert every total into this currency for comparison; defaults to region currency"`
}
type dealsOut struct {
	Region   Region      `json:"region"`
	Listings []Listing   `json:"listings"`
	Hits     []SearchHit `json:"search_hits,omitempty"`
}

type substituteIn struct {
	PartID          string  `json:"part_id"`
	Budget          float64 `json:"budget,omitempty" jsonschema:"max total price in the comparison currency; 0 = no cap"`
	Currency        string  `json:"currency,omitempty" jsonschema:"currency to compare budget/prices in; defaults to region currency. Listings in other currencies are converted (indicative ECB rates)."`
	Country         string  `json:"country,omitempty" jsonschema:"override detected region (ISO alpha-2)"`
}
type substituteOut struct {
	Substitutes []Substitute `json:"substitutes"`
}

type shopIn struct {
	SpecID          string   `json:"spec_id,omitempty" jsonschema:"saved spec to shop for (or pass part_ids)"`
	PartIDs         []string `json:"part_ids,omitempty" jsonschema:"parts to shop for when no spec_id"`
	Country         string   `json:"country,omitempty" jsonschema:"override detected region (ISO alpha-2)"`
	DisplayCurrency string   `json:"display_currency,omitempty" jsonschema:"currency for the guiding totals; defaults to region currency"`
	NoSearch        bool     `json:"no_search,omitempty" jsonschema:"skip web searches for parts that lack live listings"`
}
type shopItem struct {
	Part         Part        `json:"part"`
	Best         *Listing    `json:"best,omitempty"`         // cheapest live, shippable listing — the link to click
	Alternatives []Listing   `json:"alternatives,omitempty"` // other live options, sorted
	SearchHits   []SearchHit `json:"search_hits,omitempty"`  // buy-page candidates when nothing is on record
}
type shopOut struct {
	Region        Region      `json:"region"`
	Compatible    bool        `json:"compatible"`
	Violations    []Violation `json:"violations,omitempty"`
	Gaps          []string    `json:"gaps,omitempty"`
	Needs         []Need      `json:"needs,omitempty"` // resource shortages to also shop for (cables, adapters, ...)
	Items         []shopItem  `json:"items"`
	TotalBest     float64     `json:"total_best"`               // sum of best totals, converted
	TotalCurrency string      `json:"total_currency,omitempty"`
	TotalCovers   int         `json:"total_covers"` // how many parts the total includes
}

type deepIn struct {
	PartID  string   `json:"part_id"`
	Queries []string `json:"queries,omitempty" jsonschema:"override the default search angles (specifications / datasheet pdf / manual)"`
}
type deepSource struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	Text  string `json:"text"`
}
type deepOut struct {
	Part        Part         `json:"part"`
	EmptyFields []string     `json:"empty_fields"` // what still needs filling for full accuracy
	Sources     []deepSource `json:"sources"`
}

const (
	maxDeepSources     = 3
	maxDeepSourceChars = 20000
)

// emptyFields lists the Part attributes that are still unknown — the deep-drill
// checklist. Category-blind on purpose: the client knows which of these apply.
func emptyFields(p Part) []string {
	var f []string
	add := func(name string, empty bool) {
		if empty {
			f = append(f, name)
		}
	}
	add("socket", p.Socket == "")
	add("mem_type", p.MemType == "")
	add("mem_speed", p.MemSpeed == 0)
	add("form_factor", p.FormFactor == "")
	add("tdp_w", p.TDPW == 0)
	add("pcie_gen", p.PCIeGen == 0)
	add("pcie_lanes", p.PCIeLanes == 0)
	add("power_connectors", len(p.PowerConnectors) == 0)
	add("length_mm", p.LengthMM == 0)
	add("watts", p.Watts == 0)
	add("provides", len(p.Provides) == 0)
	add("requires", len(p.Requires) == 0)
	return f
}

type convertIn struct {
	Amount float64 `json:"amount"`
	From   string  `json:"from" jsonschema:"ISO currency code"`
	To     string  `json:"to" jsonschema:"ISO currency code"`
}
type convertOut struct {
	Amount    float64 `json:"amount"`
	From      string  `json:"from"`
	To        string  `json:"to"`
	Converted float64 `json:"converted"`
}

// pricePart returns a part's listings fully evaluated for buying NOW:
// staleness-marked, live-checked, shippability-flagged, converted to the
// display currency, usable-first sorted. Nothing dropped.
func pricePart(ctx context.Context, partID string, region Region, display string) ([]Listing, error) {
	ls, err := store.listingsFor(partID)
	if err != nil {
		return nil, err
	}
	markStale(ls, time.Now())
	liveCheckAll(ctx, ls)
	markShippable(ls, region.Country)
	annotateDisplay(ctx, ls, display)
	sortListings(ls)
	return ls, nil
}

// bestContribution adds a best listing's converted total into a running sum.
func bestContribution(l Listing, display string) (float64, bool) {
	if l.DisplayTotal > 0 {
		return l.DisplayTotal, true
	}
	if l.Currency == display {
		return l.total(), true
	}
	return 0, false
}

// regionFor resolves the effective region for a call: an explicit country
// override, else the detected region.
func regionFor(ctx context.Context, country string) Region {
	if country != "" {
		r := detectRegion(ctx)
		r.Country = strings.ToUpper(country)
		r.DDG = ddgRegion(r.Country)
		return r
	}
	return detectRegion(ctx)
}

func registerTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_parts",
		Description: "LIVE region-biased web search for hardware parts/spec pages. Returns result links to fetch. The live web is the source of truth for what exists: NEVER claim a model/SKU doesn't exist from prior knowledge — hardware releases outpace training data, so search first and trust the results.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
		q := in.Query
		if in.Category != "" {
			q = in.Category + " " + q
		}
		hits, err := search(ctx, q, in.Limit)
		if err != nil {
			return nil, searchOut{}, err
		}
		return nil, searchOut{Hits: hits}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "fetch_content",
		Description: "Fetch a URL and return readable text (cached). Use this to read spec pages, then extract fields and call save_part.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in fetchIn) (*mcp.CallToolResult, fetchOut, error) {
		if title, text, ok := store.getCached(in.URL); ok {
			return nil, fetchOut{Title: title, Text: text, Cached: true}, nil
		}
		fetch := fetchContent
		if in.Render {
			fetch = fetchRendered
		}
		title, text, err := fetch(ctx, in.URL)
		if err != nil {
			return nil, fetchOut{}, err
		}
		store.putCached(in.URL, title, text)
		return nil, fetchOut{Title: title, Text: text}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "save_part",
		Description: "Persist a structured Part (extracted from a spec page) into the local store. Returns its id. Leave fields unknown rather than guessing — unknown attributes are treated as gaps, not incompatibilities. For full build validation across ANY part type, fill provides/requires with resource tokens ('kind:variant' -> count): motherboard provides {\"dimm:ddr5\":12,\"pcie:x16\":3,\"m2:2280\":2}; RAM stick requires {\"dimm:ddr5\":1}; HBA requires {\"pcie:x8\":1}; case provides {\"bay:3.5\":8}; drive requires {\"bay:3.5\":1}. The engine checks sum(requires) <= sum(provides) per token; wider pcie slots satisfy narrower cards.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, p Part) (*mcp.CallToolResult, savePartOut, error) {
		if p.Category == "" {
			return nil, savePartOut{}, fmt.Errorf("category is required")
		}
		if p.ID == "" {
			p.ID = slug(p.Category, p.Vendor, p.Model)
		}
		if p.ID == "" {
			return nil, savePartOut{}, fmt.Errorf("cannot derive id: provide id or vendor/model")
		}
		if p.FetchedAt.IsZero() {
			p.FetchedAt = time.Now()
		}
		if err := store.savePart(p); err != nil {
			return nil, savePartOut{}, err
		}
		return nil, savePartOut{ID: p.ID}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "query_parts",
		Description: "Query stored parts by ids, category, and/or attribute clauses over scalar fields AND free-form attrs — e.g. find a GPU with cuda_compute >= 8.9, or a CPU with l3_cache_mb >= 256. Ops: eq, ne, gt, gte, lt, lte, contains, exists. Numeric when both sides parse as numbers. Parts missing the attribute never match — deep_specs them first if the pool looks thin. The store only knows what was saved: an empty result means 'not ingested yet', NOT 'does not exist' — use search_parts to look live.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in queryIn) (*mcp.CallToolResult, queryOut, error) {
		var parts []Part
		var err error
		if len(in.IDs) > 0 {
			parts, err = store.getParts(in.IDs)
		} else {
			parts, err = store.partsByCategory(in.Category)
		}
		if err != nil {
			return nil, queryOut{}, err
		}
		var out []Part
		for _, p := range parts {
			match := true
			for _, w := range in.Where {
				if !matchWhere(p, w) {
					match = false
					break
				}
			}
			if match {
				out = append(out, p)
			}
		}
		if in.Limit > 0 && len(out) > in.Limit {
			out = out[:in.Limit]
		}
		return nil, queryOut{Parts: out}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "compose_spec",
		Description: "Compose a build from stored parts: compatibility over KNOWN data, gaps (anything unverifiable is flagged loudly — e.g. GPU length vs chassis, undeclared power cables), needs (resource shortages to shop for), and total TDP. compatible=true is only trustworthy when gaps is empty; run deep_specs on flagged parts until it is.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in idsIn) (*mcp.CallToolResult, Spec, error) {
		parts, err := store.getParts(in.PartIDs)
		if err != nil {
			return nil, Spec{}, err
		}
		return nil, composeSpec(parts), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "save_spec",
		Description: "Persist a build (list of part ids) under an id for later recall.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in saveSpecIn) (*mcp.CallToolResult, savePartOut, error) {
		if in.ID == "" {
			return nil, savePartOut{}, fmt.Errorf("id is required")
		}
		if err := store.saveSpec(in.ID, in.Name, in.PartIDs); err != nil {
			return nil, savePartOut{}, err
		}
		return nil, savePartOut{ID: in.ID}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "save_listing",
		Description: "Record a price observation for a stored part (extracted from a listing/reseller page). Prices are point-in-time; find_deals flags stale ones.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, l Listing) (*mcp.CallToolResult, savePartOut, error) {
		if l.PartID == "" {
			return nil, savePartOut{}, fmt.Errorf("part_id is required")
		}
		if l.ID == "" {
			l.ID = slug(l.PartID, l.Vendor, l.Condition)
		}
		if l.SeenAt.IsZero() {
			l.SeenAt = time.Now()
		}
		if err := store.saveListing(l); err != nil {
			return nil, savePartOut{}, err
		}
		return nil, savePartOut{ID: l.ID}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "find_deals",
		Description: "ALL recorded deals for a part — nothing filtered out. Usable (live + ships-to-region) sorted first by converted total; dead/unshippable/stale ones flagged and sorted last. With search=true, also returns fresh region-ranked web results to fetch and save_listing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dealsIn) (*mcp.CallToolResult, dealsOut, error) {
		parts, err := store.getParts([]string{in.PartID})
		if err != nil {
			return nil, dealsOut{}, err
		}
		region := regionFor(ctx, in.Country)
		listings, err := store.listingsFor(in.PartID)
		if err != nil {
			return nil, dealsOut{}, err
		}
		markStale(listings, time.Now())
		liveCheckAll(ctx, listings)               // probe URLs so gone deals get FLAGGED
		markShippable(listings, region.Country)   // flag, never drop — no deal is hidden
		display := in.DisplayCurrency
		if display == "" {
			display = region.Currency
		}
		annotateDisplay(ctx, listings, display)
		sortListings(listings)
		out := dealsOut{Region: region, Listings: listings}
		if in.Search {
			p := parts[0]
			q := strings.TrimSpace(p.Vendor+" "+p.Model) + " buy price"
			out.Hits, _ = searchRegion(ctx, q, 10, region) // best-effort
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "find_substitute",
		Description: "Find cheaper drop-in replacements for a part: same category, attribute-compatible (socket/mem/form factor), with a recorded listing within budget. Ranked cheapest first. Note: 'similar performance' is approximated by compatibility, not benchmark scores; listings here are NOT live-checked — confirm the pick with find_deals before buying.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in substituteIn) (*mcp.CallToolResult, substituteOut, error) {
		parts, err := store.getParts([]string{in.PartID})
		if err != nil {
			return nil, substituteOut{}, err
		}
		orig := parts[0]
		region := regionFor(ctx, in.Country)
		currency := in.Currency
		if currency == "" {
			currency = region.Currency
		}
		cands, err := store.partsByCategory(orig.Category)
		if err != nil {
			return nil, substituteOut{}, err
		}
		type scored struct {
			sub   Substitute
			total float64
		}
		var scoredSubs []scored
		for _, c := range cands {
			if !substituteMatch(orig, c) {
				continue
			}
			ls, err := store.listingsFor(c.ID)
			if err != nil {
				return nil, substituteOut{}, err
			}
			best, total, ok := cheapestConverted(ctx, ls, currency)
			if !ok {
				continue
			}
			if in.Budget > 0 && total > in.Budget {
				continue
			}
			if currency != "" {
				best.DisplayTotal, best.DisplayCurr = total, currency
			}
			scoredSubs = append(scoredSubs, scored{Substitute{Part: c, Listing: best}, total})
		}
		sort.Slice(scoredSubs, func(i, j int) bool {
			return scoredSubs[i].total < scoredSubs[j].total
		})
		subs := make([]Substitute, len(scoredSubs))
		for i, s := range scoredSubs {
			subs[i] = s.sub
		}
		return nil, substituteOut{Substitutes: subs}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "shop_spec",
		Description: "One-stop purchase plan for a build: per part, the cheapest usable (live + ships-to-region) listing as the buy link, ALL other recorded listings as flagged alternatives (nothing filtered out), a converted build total, resource shortages to also shop for (needs: cables, adapters, bays), and buy-page search hits for parts with no usable listing. Repeat a part id in the spec to buy multiples. Feed hits to fetch_content + save_listing, then re-run to complete the plan.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in shopIn) (*mcp.CallToolResult, shopOut, error) {
		partIDs := in.PartIDs
		if in.SpecID != "" {
			_, ids, err := store.loadSpec(in.SpecID)
			if err != nil {
				return nil, shopOut{}, err
			}
			partIDs = ids
		}
		if len(partIDs) == 0 {
			return nil, shopOut{}, fmt.Errorf("pass spec_id or part_ids")
		}
		parts, err := store.getParts(partIDs)
		if err != nil {
			return nil, shopOut{}, err
		}
		region := regionFor(ctx, in.Country)
		display := in.DisplayCurrency
		if display == "" {
			display = region.Currency
		}
		spec := composeSpec(parts)
		out := shopOut{
			Region: region, Compatible: spec.Compatible,
			Violations: spec.Violations, Gaps: spec.Gaps, Needs: spec.Needs,
			TotalCurrency: display,
		}
		for _, p := range parts {
			item := shopItem{Part: p}
			ls, err := pricePart(ctx, p.ID, region, display)
			if err != nil {
				return nil, shopOut{}, err
			}
			if len(ls) > 0 && ls[0].usable() {
				item.Best = &ls[0]
				item.Alternatives = ls[1:] // includes flagged dead/unshippable — nothing hidden
				if v, ok := bestContribution(ls[0], display); ok {
					out.TotalBest += v
					out.TotalCovers++
				}
			} else {
				item.Alternatives = ls // only flagged listings on record — show them all
				if !in.NoSearch {
					q := strings.TrimSpace(p.Vendor+" "+p.Model) + " buy price"
					item.SearchHits, _ = searchRegion(ctx, q, 5, region) // best-effort
				}
			}
			out.Items = append(out.Items, item)
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "compare_specs",
		Description: "Compare saved builds side by side to pick one: per spec — compatibility, gaps, needs, total TDP, live-checked converted price total (best usable listing per part), direct buy links, and which parts still lack a usable listing. Prices are probed live at call time.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in compareIn) (*mcp.CallToolResult, compareOut, error) {
		if len(in.SpecIDs) == 0 {
			return nil, compareOut{}, fmt.Errorf("pass spec_ids")
		}
		region := regionFor(ctx, in.Country)
		display := in.DisplayCurrency
		if display == "" {
			display = region.Currency
		}
		out := compareOut{Region: region, TotalCurrency: display}
		for _, sid := range in.SpecIDs {
			name, partIDs, err := store.loadSpec(sid)
			if err != nil {
				return nil, compareOut{}, fmt.Errorf("spec %s: %w", sid, err)
			}
			parts, err := store.getParts(partIDs)
			if err != nil {
				return nil, compareOut{}, err
			}
			spec := composeSpec(parts)
			opt := specOption{
				ID: sid, Name: name, Compatible: spec.Compatible,
				Violations: spec.Violations, Gaps: spec.Gaps, Needs: spec.Needs,
				TotalTDPW: spec.TotalTDPW, PartCount: len(parts),
			}
			for _, p := range parts {
				ls, err := pricePart(ctx, p.ID, region, display)
				if err != nil {
					return nil, compareOut{}, err
				}
				if len(ls) > 0 && ls[0].usable() {
					opt.BuyLinks = append(opt.BuyLinks, ls[0].URL)
					if v, ok := bestContribution(ls[0], display); ok {
						opt.TotalBest += v
						opt.TotalCovers++
						continue
					}
				}
				opt.UncoveredIDs = append(opt.UncoveredIDs, p.ID)
			}
			out.Options = append(out.Options, opt)
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "deep_specs",
		Description: "Deep-drill a part's specifications: multi-angle web search (spec page, datasheet PDF, manual), fetch the top sources (tables preserved), and report which Part fields are still empty. Use the returned source texts to fill EVERY attribute + provides/requires, then save_part. Re-run with extra queries if fields stay empty.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deepIn) (*mcp.CallToolResult, deepOut, error) {
		parts, err := store.getParts([]string{in.PartID})
		if err != nil {
			return nil, deepOut{}, err
		}
		p := parts[0]
		name := strings.TrimSpace(p.Vendor + " " + p.Model)
		if name == "" {
			return nil, deepOut{}, fmt.Errorf("part %s has no vendor/model to search by", p.ID)
		}
		queries := in.Queries
		if len(queries) == 0 {
			queries = []string{
				name + " specifications",
				name + " datasheet pdf",
				name + " " + p.Category + " manual",
			}
		}
		region := detectRegion(ctx)
		out := deepOut{Part: p, EmptyFields: emptyFields(p)}
		seen := map[string]bool{p.SourceURL: true, "": true}
		for _, q := range queries {
			hits, _ := searchRegion(ctx, q, 5, region)
			for _, h := range hits {
				if seen[h.URL] || len(out.Sources) >= maxDeepSources {
					continue
				}
				seen[h.URL] = true
				title, text, ok := store.getCached(h.URL)
				if !ok {
					if title, text, err = fetchContent(ctx, h.URL); err != nil {
						continue // unreadable source — move on, search gave us more
					}
					store.putCached(h.URL, title, text)
				}
				if len(text) > maxDeepSourceChars {
					text = text[:maxDeepSourceChars] + "\n…(truncated)"
				}
				out.Sources = append(out.Sources, deepSource{URL: h.URL, Title: title, Text: text})
			}
			if len(out.Sources) >= maxDeepSources {
				break
			}
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "convert_currency",
		Description: "Convert an amount between currencies using indicative ECB reference rates (frankfurter.app). For guiding figures, not accounting.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in convertIn) (*mcp.CallToolResult, convertOut, error) {
		v, err := convert(ctx, in.Amount, in.From, in.To)
		if err != nil {
			return nil, convertOut{}, err
		}
		return nil, convertOut{Amount: in.Amount, From: strings.ToUpper(in.From), To: strings.ToUpper(in.To), Converted: v}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "load_spec",
		Description: "Load a saved build by id and re-compose it (fresh compat + gaps against current part data). Without an id, lists every saved spec (id, name, part_ids).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in loadSpecIn) (*mcp.CallToolResult, loadSpecOut, error) {
		if in.ID == "" {
			specs, err := store.listSpecs()
			if err != nil {
				return nil, loadSpecOut{}, err
			}
			return nil, loadSpecOut{Specs: specs}, nil
		}
		_, partIDs, err := store.loadSpec(in.ID)
		if err != nil {
			return nil, loadSpecOut{}, err
		}
		parts, err := store.getParts(partIDs)
		if err != nil {
			return nil, loadSpecOut{}, err
		}
		spec := composeSpec(parts)
		return nil, loadSpecOut{Spec: &spec}, nil
	})
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

func slug(parts ...string) string {
	s := strings.ToLower(strings.Join(parts, "-"))
	s = slugStrip.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
