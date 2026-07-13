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
	// A panic in any tool handler would otherwise crash the whole server — the
	// SDK doesn't recover them. Turn a handler panic into a normal error so one
	// bad request never takes the process down.
	s.AddReceivingMiddleware(recoverMiddleware)
	registerTools(s)

	defer stopRenderer() // kill any lightpanda we spawned
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		stopRenderer()
		log.Fatal(err)
	}
}

// recoverMiddleware catches a panic in any request handler and returns it as an
// error instead of letting it crash the server process.
func recoverMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "parts-finder: recovered panic in %s: %v\n", method, r)
				err = fmt.Errorf("internal error in %s: %v", method, r)
			}
		}()
		return next(ctx, method, req)
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
	Kind   string `json:"kind,omitempty" jsonschema:"cache freshness bucket: spec (datasheets ~30d), listing (prices ~1h), page (default ~1d)"`
	Render bool   `json:"render,omitempty" jsonschema:"force headless-browser rendering (auto-managed lightpanda). Bot-blocked sites (403/429) escalate to this automatically"`
}
type fetchOut struct {
	Title    string `json:"title"`
	Text     string `json:"text"`
	Cached   bool   `json:"cached"`
	Rendered bool   `json:"rendered,omitempty"`
	Images   int    `json:"images,omitempty"` // scanned-PDF page images returned as vision blocks
}

type savePartOut struct {
	ID string `json:"id"`
}

type imageIn struct {
	URL     string `json:"url" jsonschema:"image URL to fetch for visual reading"`
	Text    bool   `json:"text,omitempty" jsonschema:"the image is mostly text/a document (spec sheet, label, screenshot, scan): binarize to 1-bit black-and-white for the FEWEST bytes while staying legible. Use whenever you're reading text off the image"`
	Color   bool   `json:"color,omitempty" jsonschema:"keep colour (default is grayscale). Use only when colour is the signal, e.g. connector colour-coding. Ignored if text=true"`
	MaxEdge int    `json:"max_edge,omitempty" jsonschema:"cap the long edge to this many pixels — fewer pixels = fewer vision tokens. Shrink hard for a sparse label (e.g. 640); raise for a dense table if small text is unreadable. Default: 1000 for text, 1568 otherwise"`
}
type imageMeta struct {
	URL   string `json:"url"`
	MIME  string `json:"mime"`
	Bytes int    `json:"bytes"`
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
	KWHPrice        float64  `json:"kwh_price,omitempty" jsonschema:"electricity price per kWh in the display currency; adds an indicative yearly power cost (24/7 at total TDP) per spec — capex vs opex in one view"`
}
type specOption struct {
	ID              string      `json:"id"`
	Name            string      `json:"name,omitempty"`
	Compatible      bool        `json:"compatible"`
	Violations      []Violation `json:"violations,omitempty"`
	Gaps            []string    `json:"gaps,omitempty"`
	Needs           []Need      `json:"needs,omitempty"`
	TotalTDPW       int         `json:"total_tdp_w"`
	TotalBest       float64     `json:"total_best"`                  // sum of best usable listings, converted (gross as listed)
	TotalExVAT      float64     `json:"total_ex_vat,omitempty"`      // ex-VAT sum over listings with known VAT basis
	ExVATCovers     int         `json:"ex_vat_covers,omitempty"`     // units the ex-VAT total includes
	VATUnknownCount int         `json:"vat_unknown_count,omitempty"` // best listings with unrecorded VAT basis
	YearlyPowerCost float64     `json:"yearly_power_cost,omitempty"` // kwh_price * TDP * 24*365, indicative (TDP = peak)
	TotalCovers     int         `json:"total_covers"`
	PartCount       int         `json:"part_count"`
	OwnedCount      int         `json:"owned_count,omitempty"` // units already owned, excluded from the total
	BuyLinks        []string    `json:"buy_links,omitempty"`   // best usable URL per covered part
	UncoveredIDs    []string    `json:"uncovered_ids,omitempty"`
}
type compareOut struct {
	Region        Region       `json:"region"`
	TotalCurrency string       `json:"total_currency,omitempty"`
	Options       []specOption `json:"options"`
}

type saveSpecIn struct {
	ID       string   `json:"id" jsonschema:"spec id (slug); reused id overwrites"`
	Name     string   `json:"name,omitempty"`
	PartIDs  []string `json:"part_ids" jsonschema:"part ids; repeat for quantity; 'spec:<id>' inlines a saved build (racks = specs of specs)"`
	OwnedIDs []string `json:"owned_ids,omitempty" jsonschema:"units in this build you already own, REPEATED PER UNIT (own 3 of 8 sticks = id 3 times); 'spec:<id>' = own that whole sub-build — excluded from the purchase total, never shopped"`
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

type historyIn struct {
	PartID string `json:"part_id"`
}
type historyOut struct {
	Observations []PriceObs `json:"observations"` // oldest first
}

type substituteIn struct {
	PartID   string  `json:"part_id"`
	Budget   float64 `json:"budget,omitempty" jsonschema:"max total price in the comparison currency; 0 = no cap"`
	Currency string  `json:"currency,omitempty" jsonschema:"currency to compare budget/prices in; defaults to region currency. Listings in other currencies are converted (indicative ECB rates)."`
	Country  string  `json:"country,omitempty" jsonschema:"override detected region (ISO alpha-2)"`
	RankBy   string  `json:"rank_by,omitempty" jsonschema:"rank candidates by this numeric attribute DESCENDING instead of cheapest-first — any saved attr, e.g. passmark, cores, perf_per_watt; candidates missing the attr sort last"`
}
type substituteOut struct {
	Substitutes []Substitute `json:"substitutes"`
}

type shopIn struct {
	SpecID          string   `json:"spec_id,omitempty" jsonschema:"saved spec to shop for (or pass part_ids)"`
	PartIDs         []string `json:"part_ids,omitempty" jsonschema:"parts to shop for when no spec_id"`
	OwnedIDs        []string `json:"owned_ids,omitempty" jsonschema:"part ids you already own, repeated per unit owned — excluded from the total (ignored when spec_id is used; the spec carries its own owned set)"`
	Country         string   `json:"country,omitempty" jsonschema:"override detected region (ISO alpha-2)"`
	DisplayCurrency string   `json:"display_currency,omitempty" jsonschema:"currency for the guiding totals; defaults to region currency"`
	NoSearch        bool     `json:"no_search,omitempty" jsonschema:"skip web searches for parts that lack live listings"`
}

func toSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// toCount tallies ids; repeats mean quantity — the same convention as
// repeating a part id in a spec.
func toCount(ids []string) map[string]int {
	m := make(map[string]int, len(ids))
	for _, id := range ids {
		m[id]++
	}
	return m
}

// uniqueInOrder returns ids deduplicated, first-seen order preserved.
func uniqueInOrder(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

type shopItem struct {
	Part         Part        `json:"part"`
	Qty          int         `json:"qty"`                    // units this build needs (repeats in the spec)
	OwnedQty     int         `json:"owned_qty,omitempty"`    // units already owned — only qty-owned_qty are bought
	Owned        bool        `json:"owned,omitempty"`        // every needed unit is owned — nothing to buy
	SupplyShort  bool        `json:"supply_short,omitempty"` // best listing has fewer units than needed — plan can't be filled from it alone
	Best         *Listing    `json:"best,omitempty"`         // cheapest live, shippable listing — the link to click
	Alternatives []Listing   `json:"alternatives,omitempty"` // other live options, sorted
	SearchHits   []SearchHit `json:"search_hits,omitempty"`  // buy-page candidates when nothing is on record
}

// cart groups the purchase plan per vendor so shipping consolidation is
// visible — 8 cheapest-per-part picks across 8 shops means 8x shipping.
type cart struct {
	Vendor   string   `json:"vendor"`             // host of the buy links
	PartIDs  []string `json:"part_ids"`           // repeated per unit
	Subtotal float64  `json:"subtotal,omitempty"` // converted gross subtotal
}

type shopOut struct {
	Region          Region      `json:"region"`
	Compatible      bool        `json:"compatible"`
	Partial         bool        `json:"partial,omitempty"`                // not a full bootable build (may be intentional)
	MissingForBuild []string    `json:"missing_for_full_build,omitempty"` // core categories absent
	Violations      []Violation `json:"violations,omitempty"`
	Gaps            []string    `json:"gaps,omitempty"`
	Needs           []Need      `json:"needs,omitempty"` // resource shortages to also shop for (cables, adapters, ...)
	Items           []shopItem  `json:"items"`
	Carts           []cart      `json:"carts,omitempty"`             // plan grouped per vendor (shipping consolidation)
	TotalBest       float64     `json:"total_best"`                  // sum of best totals x qty, converted (gross as listed)
	TotalExVAT      float64     `json:"total_ex_vat,omitempty"`      // ex-VAT sum over listings with known VAT basis
	ExVATCovers     int         `json:"ex_vat_covers,omitempty"`     // units the ex-VAT total includes
	VATUnknownCount int         `json:"vat_unknown_count,omitempty"` // best listings with unrecorded VAT basis — record vat_included to firm the totals
	TotalCurrency   string      `json:"total_currency,omitempty"`
	TotalCovers     int         `json:"total_covers"` // how many units the total includes
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

type exportIn struct {
	SpecIDs         []string `json:"spec_ids" jsonschema:"saved spec ids to export (one sheet each; a Compare sheet is added for 2+)"`
	Path            string   `json:"path,omitempty" jsonschema:"output .xlsx path; defaults to ./parts-finder-<spec>.xlsx"`
	Append          bool     `json:"append,omitempty" jsonschema:"edit an existing workbook at path instead of overwriting it: each spec's sheet is added or replaced in place, other sheets untouched"`
	Country         string   `json:"country,omitempty"`
	DisplayCurrency string   `json:"display_currency,omitempty" jsonschema:"currency for the price columns; defaults to region currency"`
}
type exportOut struct {
	Path  string `json:"path"`
	Specs int    `json:"specs"`
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

// exVATContribution is the ex-VAT counterpart — only listings whose VAT basis
// is known contribute; the caller reports coverage so a partial ex-VAT total
// can't masquerade as a full one.
func exVATContribution(l Listing, display string) (float64, bool) {
	if l.DisplayExVAT > 0 {
		return l.DisplayExVAT, true
	}
	if ex, ok := l.exVATTotal(); ok && l.Currency == display {
		return ex, true
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
		Description: "LIVE region-biased web search for hardware parts/spec pages. Returns result links to fetch. Scope to a marketplace with the site: operator in the query (e.g. 'site:ebay.de EPYC 9334', 'site:ebay.com ...') — this surfaces direct item links + price snippets even for sites whose pages block direct fetching. The live web is the source of truth for what exists: NEVER claim a model/SKU doesn't exist from prior knowledge — hardware releases outpace training data, so search first and trust the results.",
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
		Description: "Fetch a URL and return readable text (HTML tables + PDFs preserved), smartly cached. Bot-blocked sites auto-escalate to a headless browser. `kind` tunes cache freshness: \"spec\" (datasheets, ~30d), \"listing\" (prices, ~1h), or \"page\" (default, ~1d); stale entries are cheaply revalidated. Use this to read spec/listing pages, then save_part / save_listing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in fetchIn) (*mcp.CallToolResult, fetchOut, error) {
		f, cached, err := fetchCached(ctx, in.URL, in.Kind, in.Render)
		if err != nil {
			return nil, fetchOut{}, err
		}
		out := fetchOut{Title: f.Title, Text: f.Text, Cached: cached, Rendered: f.Rendered, Images: len(f.Images)}
		// Scanned PDF: no text layer, so return the page images as vision
		// blocks for the model to OCR directly.
		if len(f.Images) > 0 {
			res := &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("Scanned/image-only PDF — no text layer. Returning %d page image(s); read the specs visually.", len(f.Images))},
			}}
			for _, img := range f.Images {
				res.Content = append(res.Content, &mcp.ImageContent{Data: img.Data, MIMEType: img.MIME})
			}
			return res, out, nil
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "fetch_image",
		Description: "Download an image and return it for VISUAL reading — read model numbers, socket markings, dimensions, connector layouts straight off a picture; also the fallback when a listing's only detail is photos. Set text=true when the image is mostly text/a document (label, spec sheet, screenshot, scan) to send the fewest possible bytes at full legibility. Images are auto-downscaled to vision-optimal size.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in imageIn) (*mcp.CallToolResult, imageMeta, error) {
		mode := modeAuto
		switch {
		case in.Text:
			mode = modeText
		case in.Color:
			mode = modeColor
		}
		data, mime, err := fetchImage(ctx, in.URL, mode, in.MaxEdge)
		if err != nil {
			return nil, imageMeta{}, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: mime}},
		}, imageMeta{URL: in.URL, MIME: mime, Bytes: len(data)}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "save_part",
		Description: "Persist a structured Part (extracted from a spec page) into the local store. Returns its id. Leave fields unknown rather than guessing — unknown attributes are treated as gaps, not incompatibilities. For full build validation across ANY part type, fill provides/requires with resource tokens ('kind:variant' -> count): motherboard provides {\"dimm:ddr5\":12,\"pcie:x16\":3,\"m2:2280\":2}; RAM stick requires {\"dimm:ddr5\":1}; HBA requires {\"pcie:x8\":1}; case provides {\"bay:3.5\":8}; drive requires {\"bay:3.5\":1}. Rack-scale uses the same tokens: rack provides {\"u:rack\":42}, PDU provides {\"outlet:c13\":24} and requires {\"u:rack\":1}, 2U chassis requires {\"u:rack\":2,\"outlet:c13\":2}, switch provides {\"port:sfp28\":48}, NIC requires {\"port:sfp28\":2}. The engine checks sum(requires) <= sum(provides) per token; wider pcie slots satisfy narrower cards, and sas ports satisfy sata drives (controller provides {\"port:sas\":8}, sata drive requires {\"port:sata\":1}). Attr conventions the compat engine also checks when present: mem_module (RDIMM/UDIMM/LRDIMM — on ram AND motherboard; mismatch won't boot), mem_type on cpu too (DDR4/DDR5 controller), capacity_gb per RAM stick vs motherboard mem_max_gb / dimm_max_gb, cooler height_mm + sockets (list) vs case cooler_max_mm + board socket, psu length_mm vs case psu_max_mm.",
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
		Description: "Compose a build from stored parts: compatibility over KNOWN data, gaps (anything unverifiable is flagged loudly — e.g. GPU length vs chassis, undeclared power cables), needs (resource shortages to shop for), and total TDP. Ids may include 'spec:<id>' to inline a saved build (repeat for quantity) — a rack is 12x 'spec:node' plus switches/PDUs. compatible=true is only trustworthy when gaps is empty; run deep_specs on flagged parts until it is.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in idsIn) (*mcp.CallToolResult, Spec, error) {
		partIDs, _, err := store.expandSpecIDs(in.PartIDs, nil)
		if err != nil {
			return nil, Spec{}, err
		}
		parts, err := store.getParts(partIDs)
		if err != nil {
			return nil, Spec{}, err
		}
		return nil, composeSpec(parts), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "save_spec",
		Description: "Persist a build (list of part ids) under an id for later recall. Repeat an id for quantity. Ids may be 'spec:<other-id>' to nest a saved build — a rack spec is 12x 'spec:node' + switches + PDU + rails; nested specs expand on load everywhere. List units you ALREADY OWN in owned_ids, REPEATED PER UNIT owned (own 3 of 8 DIMMs = the id 3 times; 'spec:<id>' = own that whole sub-build) — owned units still count for compatibility/TDP but are excluded from the purchase total and never shopped.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in saveSpecIn) (*mcp.CallToolResult, savePartOut, error) {
		if in.ID == "" {
			return nil, savePartOut{}, fmt.Errorf("id is required")
		}
		if err := store.saveSpec(in.ID, in.Name, in.PartIDs, in.OwnedIDs); err != nil {
			return nil, savePartOut{}, err
		}
		return nil, savePartOut{ID: in.ID}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "save_listing",
		Description: "Record a price observation for a stored part (extracted from a listing/reseller page). Prices are point-in-time; find_deals flags stale ones, and every price change is kept in history (price_history). ALWAYS record vat_included (+vat_rate) when the page states it — consumer shops list incl VAT, B2B resellers ex VAT, and a business buyer compares ex-VAT; omit when unstated (flagged, never guessed). Also record qty_available, in_stock, and lead_days when shown.",
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
		liveCheckAll(ctx, listings)             // probe URLs so gone deals get FLAGGED
		markShippable(listings, region.Country) // flag, never drop — no deal is hidden
		display := in.DisplayCurrency
		if display == "" {
			display = region.Currency
		}
		annotateDisplay(ctx, listings, display)
		sortListings(listings)
		out := dealsOut{Region: region, Listings: listings}
		if in.Search {
			if name := strings.TrimSpace(parts[0].Vendor + " " + parts[0].Model); name != "" {
				out.Hits, _ = searchRegion(ctx, name+" buy price", 10, region) // best-effort
			}
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "price_history",
		Description: "Price observations over time for a part, oldest first — every save_listing price change is kept, so re-saving a listing never erases what it cost before. Use to judge 'buy now or wait' and whether a deal is actually below trend.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, historyOut, error) {
		if in.PartID == "" {
			return nil, historyOut{}, fmt.Errorf("part_id is required")
		}
		obs, err := store.priceHistory(in.PartID)
		if err != nil {
			return nil, historyOut{}, err
		}
		return nil, historyOut{Observations: obs}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "find_substitute",
		Description: "Find cheaper drop-in replacements for a part: same category, attribute-compatible (socket/mem/form factor), with a recorded listing within budget. Ranked cheapest first, or by any saved numeric attribute descending via rank_by (e.g. passmark, cores) — save benchmark scores as attrs to rank by real performance. Note: without rank_by, 'similar performance' is approximated by compatibility only; listings here are NOT live-checked — confirm the pick with find_deals before buying.",
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
		if in.RankBy != "" {
			// Rank by the attribute descending (perf-style: more is better);
			// candidates missing it sink; ties fall back to cheapest.
			attr := strings.ToLower(strings.TrimSpace(in.RankBy))
			val := func(p Part) (float64, bool) { return toFloat(flatten(p)[attr]) }
			sort.SliceStable(scoredSubs, func(i, j int) bool {
				vi, oki := val(scoredSubs[i].sub.Part)
				vj, okj := val(scoredSubs[j].sub.Part)
				if oki != okj {
					return oki
				}
				if vi != vj {
					return vi > vj
				}
				return scoredSubs[i].total < scoredSubs[j].total
			})
		} else {
			sort.Slice(scoredSubs, func(i, j int) bool {
				return scoredSubs[i].total < scoredSubs[j].total
			})
		}
		subs := make([]Substitute, len(scoredSubs))
		for i, s := range scoredSubs {
			subs[i] = s.sub
		}
		return nil, substituteOut{Substitutes: subs}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "shop_spec",
		Description: "One-stop purchase plan for a build: per part, the cheapest usable (live + ships-to-region + in-stock) listing as the buy link, ALL other recorded listings as flagged alternatives (nothing filtered out), converted build totals (gross AND ex-VAT where the basis is known), per-vendor carts for shipping consolidation, resource shortages to also shop for (needs: cables, adapters, bays), and buy-page search hits for parts with no usable listing. Repeat a part id in the spec to buy multiples; supply_short flags a best listing with fewer units than needed. Feed hits to fetch_content + save_listing, then re-run to complete the plan.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in shopIn) (*mcp.CallToolResult, shopOut, error) {
		partIDs := in.PartIDs
		ownedIDs := in.OwnedIDs
		if in.SpecID != "" {
			_, ids, owned, err := store.loadSpec(in.SpecID)
			if err != nil {
				return nil, shopOut{}, err
			}
			partIDs, ownedIDs = ids, owned
		}
		partIDs, ownedIDs, err := store.expandSpecIDs(partIDs, ownedIDs)
		if err != nil {
			return nil, shopOut{}, err
		}
		if len(partIDs) == 0 {
			return nil, shopOut{}, fmt.Errorf("pass spec_id or part_ids")
		}
		parts, err := store.getParts(partIDs)
		if err != nil {
			return nil, shopOut{}, err
		}
		partByID := map[string]Part{}
		for _, p := range parts {
			partByID[p.ID] = p
		}
		demand := toCount(partIDs)
		ownedQty := toCount(ownedIDs)
		region := regionFor(ctx, in.Country)
		display := in.DisplayCurrency
		if display == "" {
			display = region.Currency
		}
		prewarmLiveness(ctx, partIDs) // one parallel probe sweep; per-part pricing then hits cache
		spec := composeSpec(parts)
		out := shopOut{
			Region: region, Compatible: spec.Compatible,
			Partial: spec.Partial, MissingForBuild: spec.MissingForBuild,
			Violations: spec.Violations, Gaps: spec.Gaps, Needs: spec.Needs,
			TotalCurrency: display,
		}
		carts := map[string]*cart{}
		var cartOrder []string
		for _, id := range uniqueInOrder(partIDs) {
			p := partByID[id]
			qty := demand[id]
			ownedN := min(ownedQty[id], qty)
			buyN := qty - ownedN
			item := shopItem{Part: p, Qty: qty, OwnedQty: ownedN}
			if buyN == 0 {
				item.Owned = true // every unit on hand: counts for compat/TDP, not for buying
				out.Items = append(out.Items, item)
				continue
			}
			ls, err := pricePart(ctx, p.ID, region, display)
			if err != nil {
				return nil, shopOut{}, err
			}
			if len(ls) > 0 && ls[0].usable() {
				best := ls[0]
				item.Best = &best
				item.Alternatives = ls[1:] // includes flagged dead/unshippable — nothing hidden
				// A 1-unit auction can't fill a 24-DIMM order: flag, never hide.
				item.SupplyShort = best.QtyAvailable > 0 && best.QtyAvailable < buyN
				if best.VATUnknown {
					out.VATUnknownCount++
				}
				var contributed float64
				if v, ok := bestContribution(best, display); ok {
					contributed = v * float64(buyN)
					out.TotalBest += contributed
					out.TotalCovers += buyN
				}
				if v, ok := exVATContribution(best, display); ok {
					out.TotalExVAT += v * float64(buyN)
					out.ExVATCovers += buyN
				}
				vendor := hostOf(best.URL)
				if vendor == "" {
					vendor = best.Vendor
				}
				c, ok := carts[vendor]
				if !ok {
					c = &cart{Vendor: vendor}
					carts[vendor] = c
					cartOrder = append(cartOrder, vendor)
				}
				for range buyN {
					c.PartIDs = append(c.PartIDs, p.ID)
				}
				c.Subtotal += contributed
			} else {
				item.Alternatives = ls // only flagged listings on record — show them all
				if name := strings.TrimSpace(p.Vendor + " " + p.Model); !in.NoSearch && name != "" {
					item.SearchHits, _ = searchRegion(ctx, name+" buy price", 5, region) // best-effort
				}
			}
			out.Items = append(out.Items, item)
		}
		for _, v := range cartOrder {
			out.Carts = append(out.Carts, *carts[v])
		}
		sort.SliceStable(out.Carts, func(i, j int) bool {
			return len(out.Carts[i].PartIDs) > len(out.Carts[j].PartIDs)
		})
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "compare_specs",
		Description: "Compare saved builds side by side to pick one: per spec — compatibility, gaps, needs, total TDP, live-checked converted price totals (gross + ex-VAT where known; best usable listing per part x quantity), direct buy links, which parts still lack a usable listing, and (given kwh_price) an indicative yearly power cost. Prices are probed live at call time.",
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
			name, rawIDs, rawOwned, err := store.loadSpec(sid)
			if err != nil {
				return nil, compareOut{}, fmt.Errorf("spec %s: %w", sid, err)
			}
			partIDs, ownedIDs, err := store.expandSpecIDs(rawIDs, rawOwned)
			if err != nil {
				return nil, compareOut{}, fmt.Errorf("spec %s: %w", sid, err)
			}
			parts, err := store.getParts(partIDs)
			if err != nil {
				return nil, compareOut{}, err
			}
			partByID := map[string]Part{}
			for _, p := range parts {
				partByID[p.ID] = p
			}
			demand := toCount(partIDs)
			ownedQty := toCount(ownedIDs)
			prewarmLiveness(ctx, partIDs)
			spec := composeSpec(parts)
			opt := specOption{
				ID: sid, Name: name, Compatible: spec.Compatible,
				Violations: spec.Violations, Gaps: spec.Gaps, Needs: spec.Needs,
				TotalTDPW: spec.TotalTDPW, PartCount: len(parts),
			}
			if in.KWHPrice > 0 {
				opt.YearlyPowerCost = in.KWHPrice * float64(spec.TotalTDPW) / 1000 * 24 * 365
			}
			for _, id := range uniqueInOrder(partIDs) {
				ownedN := min(ownedQty[id], demand[id])
				buyN := demand[id] - ownedN
				opt.OwnedCount += ownedN
				if buyN == 0 {
					continue // every unit owned — not a purchase
				}
				ls, err := pricePart(ctx, id, region, display)
				if err != nil {
					return nil, compareOut{}, err
				}
				if len(ls) > 0 && ls[0].usable() {
					opt.BuyLinks = append(opt.BuyLinks, ls[0].URL)
					if ls[0].VATUnknown {
						opt.VATUnknownCount++
					}
					if v, ok := exVATContribution(ls[0], display); ok {
						opt.TotalExVAT += v * float64(buyN)
						opt.ExVATCovers += buyN
					}
					if v, ok := bestContribution(ls[0], display); ok {
						opt.TotalBest += v * float64(buyN)
						opt.TotalCovers += buyN
						continue
					}
				}
				opt.UncoveredIDs = append(opt.UncoveredIDs, id)
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
				f, _, ferr := fetchCached(ctx, h.URL, "spec", false)
				if ferr != nil {
					continue // unreadable source — move on, search gave us more
				}
				text := f.Text
				if len(text) > maxDeepSourceChars {
					text = text[:maxDeepSourceChars] + "\n…(truncated)"
				}
				out.Sources = append(out.Sources, deepSource{URL: h.URL, Title: f.Title, Text: text})
			}
			if len(out.Sources) >= maxDeepSources {
				break
			}
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "export_spec",
		Description: "Export one or more saved specs to an .xlsx workbook: a sheet per spec (parts, key specs, live best price + buy link, owned-vs-buy totals, gaps, needs) plus a Compare sheet for multiple specs. Returns the file path to open. Pass append=true to update an existing workbook in place — a spec's sheet is added or replaced, leaving your other sheets/edits intact.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in exportIn) (*mcp.CallToolResult, exportOut, error) {
		if len(in.SpecIDs) == 0 {
			return nil, exportOut{}, fmt.Errorf("pass spec_ids")
		}
		region := regionFor(ctx, in.Country)
		display := in.DisplayCurrency
		if display == "" {
			display = region.Currency
		}
		defaultName := "parts-finder-" + slug(in.SpecIDs[0]) + ".xlsx"
		path := in.Path
		if path == "" {
			// Ask the user where to save (Home/Documents/current/custom); falls
			// back to the current folder if the client can't elicit.
			path = elicitExportPath(ctx, req.Session, defaultName)
		}
		if path == "" {
			cwd, _ := os.Getwd()
			path = filepath.Join(cwd, defaultName)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, exportOut{}, fmt.Errorf("create output dir: %w", err)
		}
		out, err := exportSpecsXLSX(ctx, in.SpecIDs, path, region, display, in.Append)
		if err != nil {
			return nil, exportOut{}, err
		}
		return nil, exportOut{Path: out, Specs: len(in.SpecIDs)}, nil
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
		_, rawIDs, rawOwned, err := store.loadSpec(in.ID)
		if err != nil {
			return nil, loadSpecOut{}, err
		}
		partIDs, ownedIDs, err := store.expandSpecIDs(rawIDs, rawOwned)
		if err != nil {
			return nil, loadSpecOut{}, err
		}
		parts, err := store.getParts(partIDs)
		if err != nil {
			return nil, loadSpecOut{}, err
		}
		spec := composeSpec(parts)
		spec.Owned = ownedIDs
		return nil, loadSpecOut{Spec: &spec}, nil
	})
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

func slug(parts ...string) string {
	s := strings.ToLower(strings.Join(parts, "-"))
	s = slugStrip.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
