# parts-finder-mcp — Design

Go MCP server for speccing servers from compatible hardware: search vendors +
resellers, compose compatible parts, find current deals, substitute for
similar-perf/cheaper alternates. Kindly-style retrieval layer + hardware domain
layer on top. Local, single user.

## Stack (decided)

- **Language:** Go
- **MCP SDK:** `github.com/modelcontextprotocol/go-sdk` (official)
- **Search:** keyless. Start DuckDuckGo HTML endpoint (`html.duckduckgo.com/html/`).
  SearXNG = drop-in upgrade when DDG rate-limits.
- **Fetch:** `github.com/go-shiori/go-readability` → markdown
- **PDF:** `github.com/ledongthuc/pdf` (spec sheets are PDFs)
- **Store:** SQLite via `modernc.org/sqlite` (pure Go, no cgo, single file)
- **Headless browser:** deferred. Use chrome-devtools MCP manually until a
  vendor needs JS. When a real headless browser is needed (M3), use
  [lightpanda-io/browser](https://github.com/lightpanda-io/browser) — lean,
  CDP-compatible, built for automation.

## Layers

- **L1 retrieval** (kindly-equivalent, thin): `web_search` (DDG) + `get_content`
  (readability / PDF) + SQLite cache.
- **L2 hardware domain** (the value): parts store + predicate compat engine +
  spec compose + deals/substitute.

## Data model (spine)

Store attributes once; derive compat. No N² A-fits-B pair table.

```
Part{ id, category, vendor, model,
      socket, mem_type, mem_speed, form_factor,
      tdp_w, pcie_gen, pcie_lanes, power_connectors[],
      dims_mm, raw_specs_json, source_url, fetched_at }
```

## Compat engine

Predicates over attributes, not stored pairs:

```
cpu.socket == mobo.socket
ram.mem_type == mobo.mem_type
psu.watts >= sum(tdp) * 1.3
gpu.length_mm <= case.max_gpu_mm
... ~15 rules cover 90% server builds
```

## Tools (MCP surface)

| Tool | Layer | Does |
|------|-------|------|
| `search_parts(query, category)` | L1+L2 | search + parse + cache → Part[] |
| `get_part(id \| url)` | L1+L2 | cache-or-fetch → Part |
| `check_compat(partIds[])` | L2 | → {ok, violations[]:{rule, parts}} |
| `compose_spec(partIds[])` | L2 | → Spec{parts, compat, gaps[], total_tdp} |
| `save_spec` / `load_spec(id)` | L2 | persist builds |
| `find_deals(partId)` | L1+L2 | listings sorted (price+ship+freshness) |
| `find_substitute(partId, budget)` | L1+L2 | similar-perf cheaper → Part[] |

## Milestones

- **M1 — working spec composer:** go-sdk skeleton → `search_parts` + `get_part`
  (DDG + readability + SQLite) → `check_compat` + `compose_spec` over seeded
  rules. Self-test: known-good + known-bad build, assert violations fire.
- **M2:** `find_deals`, `find_substitute`, PDF parsing.
- **M3 — done:** SearXNG fallback (`SEARXNG_URL`), lightpanda headless render
  (`LIGHTPANDA_URL`, opt-in via `fetch_content(render=true)`), 2 more compat
  rules (form-factor fit, GPU power connectors).
- **M6 — done:** full sweep. Never-drop policy: dead/unshippable/stale listings
  flagged + sorted last, never filtered. Structured `needs` (resource deficits)
  = shopping list for small stuff (cables, adapters); quantities via repeated
  part ids in specs. `deep_specs` tool: multi-angle search + top-source fetch +
  empty-field checklist per part for accuracy passes.
- **M5 — done:** one-stop shop. Generic provides/requires resource accounting
  (any part type: drives, HBAs, NICs, bays, DIMM slots, PCIe with width
  flexibility) — one engine rule, no rule-per-pair. `shop_spec` purchase plan:
  per part cheapest live region-shippable link + alternatives + converted build
  total + search hits for uncovered parts. Table-preserving extraction in
  `fetch_content` (spec tables → markdown).
- **M4 — done:** region-awareness + deal freshness. IP-detected region
  (ip-api.com; `REGION_COUNTRY`/`REGION_CURRENCY` override); search biased by
  DDG `kl` + local/EU-reseller ranking (bias, not filter); `find_deals` always
  live-checks URLs (drops dead), filters ships-to region, flags staleness;
  currency conversion (frankfurter ECB rates) for a single guiding figure +
  `convert_currency` tool.

## Region ranking (generic, no vendor list)

Search results are ranked by geographic proximity derived from the domain — no
hardcoded vendor names. `rankScore` (region.go):

- vendor domain we've stored a region-shippable listing from → boost (learned)
- local ccTLD (`.dk` in DK) → boost
- `.eu` when region is in the EU → boost
- generic TLD (`.com`/`.net`) → neutral
- EU-neighbour ccTLD when region is EU → neutral
- any other foreign ccTLD → demote

Preference is data-driven and improves as listings accrue; nothing is dropped
(bias, not filter).

## Prior art (checked)

No server/hardware-specific buying MCP exists. Closest: eBay MCP (Sell-side
APIs), unofficial Amazon MCP, retailerapi MCP (US-only, UPC lookup + price
history). None region-aware or build-composing — this project's niche holds.

## Lessons from kindly (github Shelpuk-AI-.../kindly-web-search-mcp-server)

Studied its search/scrape stack. Applied now:
- PDF detection sniffs payload magic bytes (`%PDF-`), not just content-type /
  URL suffix — spec sheets are often mislabeled.
- SearXNG fallback: validate result URLs, map 403 (json format disabled) / 429
  (rate limit) to clear errors, pass a `language` param from the region.

Deferred (noted for later):
- **Table-preserving extraction.** kindly uses trafilatura with
  `include_tables=True`; go-readability's `TextContent` flattens tables. Hardware
  spec sheets *are* tables — a table-aware HTML→markdown pass would materially
  improve field extraction. Top future scrape improvement.
- Headless hardening (for when lightpanda gets serious): readiness via
  `/json/version` (already done in render.go), retry/backoff on connect errors,
  abort-wait if the browser process exits, process-group kill.
- Per-source structured handlers before generic HTML (kindly has
  StackExchange/GitHub/Wikipedia/arXiv API paths). Our analog would be
  API-based fetch for eBay etc. — opt-in, only if generic scraping falls short.

## Open risks

- **Parse reliability** — vendor pages/PDFs vary wildly. Normalizing raw specs →
  structured Part fields is the real grind. Model-assisted extraction likely:
  client Claude reads `get_content`, MCP stores the structured result.
- **DDG rate limits** — may force SearXNG sooner than M3.
- **Deal freshness** — listings die fast. `fetched_at` staleness flag only; no
  live price guarantee.
