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
- `SEARXNG_URL` — base URL of a SearXNG instance; used as fallback when
  DuckDuckGo returns nothing (rate-limited).
- `LIGHTPANDA_URL` — CDP endpoint of an externally managed
  [lightpanda](https://github.com/lightpanda-io/browser)/Chrome. **Optional**:
  without it, parts-finder finds `lightpanda` on PATH, or downloads it to
  `~/.cache/parts-finder/` and spawns it on demand. Bot-blocked sites (eBay
  et al. return 403 to plain HTTP) automatically escalate through the
  renderer — zero configuration needed.
- `REGION_COUNTRY` / `REGION_CURRENCY` — override the IP-detected region
  (e.g. `DK` / `DKK`). By default the region is auto-detected from your IP
  (ip-api.com) and cached for the process.

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
| `fetch_content(url, render?)` | fetch + readability → text (cached); PDF-aware; tables kept; `render=true` uses lightpanda |
| `save_part(Part)` | persist a part: scalars + provides/requires + free-form `attrs` |
| `query_parts(ids? \| category?, where[]?)` | query parts by any attribute: `cuda_compute >= 8.9`, `l3_cache_mb >= 256`; ops eq/ne/gt/gte/lt/lte/contains/exists |
| `compose_spec(part_ids[])` | build report: compat over known data, loud gaps, needs, total TDP |
| `save_spec(id, name?, part_ids[])` / `load_spec(id?)` | persist/recall builds; `load_spec` without id lists all |
| `compare_specs(spec_ids[])` | side-by-side configurations: compat, TDP, live-checked converted totals, buy links, uncovered parts |
| `save_listing(Listing)` | record a point-in-time price for a part |
| `find_deals(part_id, search?, country?, display_currency?, ...)` | live-checked, region-filtered deals, cheapest-converted first + staleness |
| `find_substitute(part_id, budget?, currency?, country?)` | cheaper drop-in replacements within budget (cross-currency aware) |
| `shop_spec(spec_id \| part_ids, ...)` | one-stop purchase plan: per part the best usable link + ALL alternatives (flagged, never dropped), converted total, needs (cables/adapters short), search hits for uncovered parts |
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

Compat rules: cpu/mobo socket, ram/mobo mem type, PSU headroom (×1.3), GPU
length vs case, motherboard form factor vs case, GPU power connectors vs PSU.
Unknown attributes are always treated as gaps, never violations.

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

## Freshness guarantees

- Every `shop_spec`/`find_deals`/`compare_specs` call live-probes listing URLs.
- Part data older than 30 days gets a gap: refresh with `deep_specs`.
- The live web is the source of truth: tool descriptions instruct the client to
  never deny a model's existence from training data — always search first.
