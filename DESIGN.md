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
- **M3 (only if hit):** SearXNG, chromedp, more rule coverage.

## Open risks

- **Parse reliability** — vendor pages/PDFs vary wildly. Normalizing raw specs →
  structured Part fields is the real grind. Model-assisted extraction likely:
  client Claude reads `get_content`, MCP stores the structured result.
- **DDG rate limits** — may force SearXNG sooner than M3.
- **Deal freshness** — listings die fast. `fetched_at` staleness flag only; no
  live price guarantee.
