# parts-finder-mcp

Local Go MCP server for speccing servers from compatible hardware.

## Install

Homebrew (macOS + Linux):

```sh
brew install stubbedev/parts-finder/parts-finder
```

Nix flake:

```sh
nix run github:stubbedev/parts-finder-mcp
# or
nix profile install github:stubbedev/parts-finder-mcp
```

From source:

```sh
just build        # ./bin/parts-finder
# or
go build -o parts-finder .
```

## Register (Claude Code)

```sh
claude mcp add parts-finder -- /abs/path/to/parts-finder
```

DB path defaults to `~/.parts-finder.db`; override with `PARTS_DB`.

Optional env:
- `SEARXNG_URL` — base URL of a SearXNG instance; when set it becomes the
  FIRST search engine in the chain (your own instance, no rate-limit
  exposure).
- `LIGHTPANDA_URL` — CDP endpoint of an externally managed
  [lightpanda](https://github.com/lightpanda-io/browser)/Chrome. **Optional**:
  without it, parts-finder finds `lightpanda` on PATH, or downloads it to
  `~/.cache/parts-finder/` and spawns it on demand. Bot-blocked sites (eBay
  et al. return 403 to plain HTTP) automatically escalate through the
  renderer — zero configuration needed.
- `REGION_COUNTRY` / `REGION_CURRENCY` — override the IP-detected region
  (e.g. `DK` / `DKK`). By default the region is auto-detected from your IP
  (ifconfig.co over https) and cached for the process.

## Region & currency

Search is biased to your region: DuckDuckGo `kl` locale + ranking that floats
local ccTLD domains and known EU server resellers to the top and demotes US-only
shops (nothing is dropped). `find_deals` / `find_substitute` accept a `country`
override and a display/comparison currency (defaults to the region currency);
listing totals in other currencies are converted with indicative ECB rates
(frankfurter.app) so you get one guiding figure. `convert_currency` exposes the
same conversion directly.

`find_deals` and `shop_spec` live-check every listing URL — but **nothing is
ever dropped**: dead (404/unreachable), unshippable, and stale (>14 days)
listings are flagged and sorted below usable ones, so no deal is hidden by a
filter.

## M1 tools

| Tool | Does |
|------|------|
| `search_parts(query, category?, limit?)` | LIVE keyless region-biased search → result links (web is truth, not training data) |
| `fetch_content(url, kind?, render?)` | fetch → text (smart-cached by kind); PDF + HTML tables preserved; bot-blocked pages auto-render via lightpanda |
| `fetch_image(url)` | download + downscale an image (jpeg/png/gif/webp/bmp/tiff) → vision block for reading specs/labels/diagrams off pictures |
| `export_spec(spec_ids[], path?, append?)` | polished .xlsx: per-spec sheet (parts, live prices, buy links, owned-vs-buy totals) + Compare sheet; `append` edits an existing workbook in place; prompts for a save location when `path` is omitted |
| `save_part(Part)` | persist a part: scalars + provides/requires + free-form `attrs` |
| `query_parts(ids? \| category?, where[]?)` | query parts by any attribute: `cuda_compute >= 8.9`, `l3_cache_mb >= 256`; ops eq/ne/gt/gte/lt/lte/contains/exists |
| `compose_spec(part_ids[])` | build report: compat over known data, loud gaps, needs, total TDP |
| `save_spec(id, name?, part_ids[])` / `load_spec(id?)` | persist/recall builds; `load_spec` without id lists all |
| `compare_specs(spec_ids[], kwh_price?)` | side-by-side configurations: compat, TDP, live-checked converted totals (gross + ex-VAT), buy links, uncovered parts; `kwh_price` adds indicative yearly power cost (capex vs opex) |
| `save_listing(Listing)` | record a point-in-time price for a part (incl. VAT basis, stock, qty available, lead time); every price change is kept in history |
| `price_history(part_id)` | price observations over time — judge "buy now or wait" |
| `find_deals(part_id, search?, country?, display_currency?, ...)` | live-checked, region-filtered deals, cheapest-converted first + staleness |
| `find_substitute(part_id, budget?, currency?, country?, rank_by?)` | cheaper drop-in replacements within budget (cross-currency aware); `rank_by` ranks by any saved numeric attr (e.g. passmark) instead of price |
| `shop_spec(spec_id \| part_ids, ...)` | one-stop purchase plan: per part the best usable link + ALL alternatives (flagged, never dropped), gross + ex-VAT totals, per-vendor carts (shipping consolidation), needs (cables/adapters short), search hits for uncovered parts |
| `deep_specs(part_id, queries?)` | deep-drill a part: multi-angle search + fetch top sources (tables kept) + checklist of still-empty fields |
| `convert_currency(amount, from, to)` | indicative ECB currency conversion |

Typical flow: `search_parts` → `fetch_content` on a spec page (HTML or PDF) →
extract fields → `save_part` → `compose_spec`. For deals: `find_deals(search=true)`
→ `fetch_content` a listing → `save_listing` → `find_deals` / `find_substitute`.
Extraction is done by the client model; unknown attributes are left blank and
surface as gaps, never as false incompatibilities.

## Test

```sh
go test ./...
```

Compat rules are data tables, not per-pair code. Match rules (must be equal
when both sides declare): cpu/mobo socket, ram/mobo + cpu/mobo memory gen,
ram/mobo module type (RDIMM/UDIMM/LRDIMM — the used-server-RAM boot killer),
cooler supported-sockets list vs board. Capacity rules (numeric limits):
total/per-DIMM memory capacity vs board max, GPU length / cooler height / PSU
length vs case clearances. Bespoke rules: PSU headroom (×1.3, summed across
PSUs — dual-PSU servers count both; losing N+1 redundancy is a gap),
motherboard form factor vs case, GPU power connectors vs PSU. RAM faster than
the board's max flags a downclock gap (paid-for headroom, not a failure).
Port tokens know protocol supersets: SAS ports satisfy SATA drives, never the
reverse. Unknown attributes are always treated as gaps, never violations.
Extending coverage = adding a table row.

Business-buyer pricing: listings carry a VAT basis (`vat_included` +
`vat_rate`) — consumer shops list incl VAT, B2B resellers ex VAT, and comparing
the two raw skews picks by 25%. Ranking uses the **ex-VAT** total where the
basis is known; unknown-basis listings compare by gross (can only overestimate,
never sneak ahead) and are flagged `vat_unknown`. Totals come out both ways
(`total_best` gross, `total_ex_vat` + coverage). Listings also carry
`qty_available` / `in_stock` / `lead_days`: out-of-stock sinks (never dropped),
and a best listing with fewer units than the build needs is flagged
`supply_short` — a 1-unit auction can't fill a 24-DIMM order.

## Generic resource accounting (any part type)

Beyond the named rules, every part can declare `provides` / `requires` maps of
resource tokens (`"kind:variant" -> count`): a motherboard provides
`{"dimm:ddr5":12,"pcie:x16":3,"m2:2280":2}`, a RAM stick requires
`{"dimm:ddr5":1}`, an HBA requires `{"pcie:x8":1}`, a case provides
`{"bay:3.5":8}`, a drive requires `{"bay:3.5":1}`. One engine rule checks
`sum(requires) <= sum(provides)` per token; wider PCIe slots satisfy narrower
cards. This validates builds across drives, controllers, NICs, risers,
backplanes — any category — without new code per pair.

`fetch_content` preserves `<table>` data as markdown (spec sheets are tables),
so the client can extract these tokens reliably.

Already own some parts? List them in a spec's `owned_ids`, **repeated per unit
owned** (own 3 of 8 DIMMs = the id 3 times) — owned units still count for
compatibility and TDP but are excluded from the purchase total and never
shopped, so a piecemeal upgrade prices exactly the units you're missing.

Racks are specs of specs: a part id may be `spec:<id>`, inlining a saved build
(repeat for quantity) — `["spec:node" x12, "switch", "pdu", "rails"]` is a rack.
Nested specs expand everywhere (compose/shop/compare/export), sub-spec owned
parts carry up, and the same resource tokens cover rack physics: rack provides
`u:rack: 42`, a 2U chassis requires `u:rack: 2` + `outlet:c13: 2`, the PDU
provides `outlet:c13: 24`, a switch provides `port:sfp28: 48`.

Reading images & scanned PDFs: `fetch_image` downscales any common format
(jpeg/png/gif/webp/bmp/tiff) to a vision-optimal size. `fetch_content` on a
scanned/image-only PDF (no text layer) automatically returns its page images as
vision blocks so specs can be read straight off the scan.

Quantities: repeat a part id in a spec (`["mb","cpu","cpu","psu"]` = dual CPU).
Each instance consumes resources and counts toward TDP, so cable/slot shortages
surface per unit. Small stuff (cables, adapters, rails) are just parts too —
`compose_spec`/`shop_spec` return structured `needs` (resource → count short)
telling you exactly what extras to add.

## Typical one-stop flow

1. `search_parts` (region-biased) → `fetch_content` spec pages → `save_part`
   with attributes + provides/requires.
2. `deep_specs(part_id)` per part → fill every empty field from the fetched
   sources → `save_part` again (accuracy pass).
3. `compose_spec` → fix violations/gaps, shop the `needs` (cables etc.) →
   `save_spec`.
4. `shop_spec(spec_id)` → per part: best usable link to click + all flagged
   alternatives + converted build total. Parts without usable listings come
   back with buy-page search hits — `fetch_content` + `save_listing` those,
   re-run `shop_spec`.
5. Build 2–3 candidate specs → `compare_specs` → pick by compat, TDP, and
   live-checked total.

## Retrieval hardening

The search/fetch core is the cornerstone — every request goes through one
hardened path:

- **Search engine chain** — search must ALWAYS answer. Engines are tried in
  order until one yields hits: SearXNG (if configured) → DDG html → DDG lite
  (separate throttle bucket) → Brave → Ecosia (Bing/Google-backed). A
  rate-limited engine (429/403/202/anomaly page) is put on a 5-minute cooldown
  and skipped, never hammered deeper into a ban. "Every engine blind" is a
  loud error, distinct from "query has no results".
- **Per-host throttling** — token spacing + concurrency cap + jitter, so we
  never trip a rate-limit ban (losing a host = losing its deals).
- **Retry with backoff** — 429/502/503/504 and network errors retry with
  exponential backoff + jitter, honouring `Retry-After`.
- **Browser fingerprint rotation** — a realistic UA + client-hint + Sec-Fetch
  header set, chosen per host and kept stable (flipping mid-session is itself a
  bot tell). Cookie jar carries sessions across the set-cookie→redirect dance.
- **https-first**, redirect-following, transparent gzip.
- **Headless escalation** — TLS-fingerprint walls (eBay/Akamai) auto-escalate
  to lightpanda; same extraction either way.
- **Resilient** — every goroutine and tool handler recovers from panics, so one
  bad probe/scrape/request can never crash the server. Listing liveness is
  pre-warmed in one parallel sweep before pricing a build.
- **Vision-tuned images** — fetched/scanned images are downscaled before they
  reach the model, because vision cost is paid in **tokens ∝ pixels**. Photo
  mode caps ~1568px/1.15MP; **text mode binarizes to 1-bit and caps ~1000px**
  (crisp glyph edges survive it) for the fewest tokens reading specs; a per-call
  `max_edge` shrinks further (e.g. 640 for a sparse label). Caps are
  general-purpose defaults, all env-overridable per harness/model
  (`PARTS_IMG_MAX_EDGE`, `PARTS_IMG_TEXT_EDGE`, `PARTS_IMG_MAX_PIXELS`,
  `PARTS_IMG_TEXT_PIXELS`) since different vision models tile differently.
- **Smart caching** — persistent SQLite cache keyed by URL with per-`kind` TTL
  (spec ~30d / page ~1d / listing ~1h). Stale entries revalidate cheaply via
  ETag / Last-Modified conditional GETs (304 → keep, reset TTL). On any fetch
  failure the last-known-good content is served rather than nothing.

## Freshness guarantees

- Every `shop_spec`/`find_deals`/`compare_specs` call live-probes listing URLs.
- Part data older than 30 days gets a gap: refresh with `deep_specs`.
- The live web is the source of truth: tool descriptions instruct the client to
  never deny a model's existence from training data — always search first.
