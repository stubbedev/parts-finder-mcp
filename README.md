# parts-finder-mcp

Local Go MCP server for speccing servers from compatible hardware. See
[DESIGN.md](DESIGN.md) for the full design.

## Build

```sh
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
- `LIGHTPANDA_URL` — CDP endpoint of a running
  [lightpanda](https://github.com/lightpanda-io/browser) (`http://127.0.0.1:9222`
  or a `ws://` URL); enables `fetch_content(render=true)` for JS-heavy pages.
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

`find_deals` always live-checks each listing URL and drops dead ones (404 /
unreachable) and listings that don't ship to your region, plus flags stale
prices (>14 days). Use `include_dead` / `include_unshippable` to keep them.

## M1 tools

| Tool | Does |
|------|------|
| `search_parts(query, category?, limit?)` | keyless DuckDuckGo search → result links |
| `fetch_content(url, render?)` | fetch + readability → text (cached); PDF-aware; `render=true` uses lightpanda |
| `save_part(Part)` | persist a structured part; derives id from vendor/model if omitted |
| `get_part(id)` | load a stored part |
| `check_compat(part_ids[])` | run compat rules → violations |
| `compose_spec(part_ids[])` | build report: compat, gaps, total TDP |
| `save_spec(id, name?, part_ids[])` / `load_spec(id)` | persist/recall builds |
| `save_listing(Listing)` | record a point-in-time price for a part |
| `find_deals(part_id, search?, country?, display_currency?, ...)` | live-checked, region-filtered deals, cheapest-converted first + staleness |
| `find_substitute(part_id, budget?, currency?, country?)` | cheaper drop-in replacements within budget (cross-currency aware) |
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
