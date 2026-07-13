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

func main() {
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

	s := mcp.NewServer(&mcp.Implementation{Name: "parts-finder", Version: "0.1.0"}, nil)
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
type compatOut struct {
	OK         bool        `json:"ok"`
	Violations []Violation `json:"violations"`
}

type saveSpecIn struct {
	ID      string   `json:"id" jsonschema:"spec id (slug); reused id overwrites"`
	Name    string   `json:"name,omitempty"`
	PartIDs []string `json:"part_ids"`
}
type loadSpecIn struct {
	ID string `json:"id"`
}

type dealsIn struct {
	PartID          string `json:"part_id"`
	Search          bool   `json:"search,omitempty" jsonschema:"also run a region-biased web search for buy pages to populate more listings"`
	Country         string `json:"country,omitempty" jsonschema:"override detected region (ISO alpha-2, e.g. DK)"`
	DisplayCurrency string `json:"display_currency,omitempty" jsonschema:"convert every total into this currency for comparison; defaults to region currency"`
	IncludeDead     bool   `json:"include_dead,omitempty" jsonschema:"keep listings whose URL is no longer reachable (default: drop them)"`
	IncludeUnshippable bool `json:"include_unshippable,omitempty" jsonschema:"keep listings that don't ship to the region (default: drop them)"`
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
		Description: "Search the web (keyless DuckDuckGo) for hardware parts/spec pages. Returns result links to fetch.",
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
		Description: "Persist a structured Part (extracted from a spec page) into the local store. Returns its id. Leave fields unknown rather than guessing — unknown attributes are treated as gaps, not incompatibilities.",
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
		Name:        "get_part",
		Description: "Fetch a stored Part by id.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in loadSpecIn) (*mcp.CallToolResult, Part, error) {
		ps, err := store.getParts([]string{in.ID})
		if err != nil {
			return nil, Part{}, err
		}
		return nil, ps[0], nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "check_compat",
		Description: "Check whether stored parts are compatible. Returns violations (empty = compatible).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in idsIn) (*mcp.CallToolResult, compatOut, error) {
		parts, err := store.getParts(in.PartIDs)
		if err != nil {
			return nil, compatOut{}, err
		}
		vs := checkCompat(parts)
		return nil, compatOut{OK: len(vs) == 0, Violations: vs}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "compose_spec",
		Description: "Compose a build from stored parts: compatibility, gaps (missing categories/attrs), and total TDP.",
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
		Description: "List recorded deals for a part, cheapest total first, with staleness flags. With search=true, also returns fresh web results (vendor + reseller) to fetch and save_listing.",
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
		liveCheckAll(ctx, listings) // always probe URLs so gone deals get dropped
		// Drop dead / unshippable listings unless explicitly kept.
		kept := listings[:0]
		for _, l := range listings {
			if l.Dead && !in.IncludeDead {
				continue
			}
			if !in.IncludeUnshippable && !shipsTo(l.ShipsTo, region.Country) {
				continue
			}
			kept = append(kept, l)
		}
		listings = kept
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
		Description: "Find cheaper drop-in replacements for a part: same category, attribute-compatible (socket/mem/form factor), with a recorded listing within budget. Ranked cheapest first. Note: 'similar performance' is approximated by compatibility, not benchmark scores.",
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
		Description: "Load a saved build by id and re-compose it (fresh compat + gaps against current part data).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in loadSpecIn) (*mcp.CallToolResult, Spec, error) {
		_, partIDs, err := store.loadSpec(in.ID)
		if err != nil {
			return nil, Spec{}, err
		}
		parts, err := store.getParts(partIDs)
		if err != nil {
			return nil, Spec{}, err
		}
		return nil, composeSpec(parts), nil
	})
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

func slug(parts ...string) string {
	s := strings.ToLower(strings.Join(parts, "-"))
	s = slugStrip.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
