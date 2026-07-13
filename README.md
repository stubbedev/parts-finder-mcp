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

## M1 tools

| Tool | Does |
|------|------|
| `search_parts(query, category?, limit?)` | keyless DuckDuckGo search → result links |
| `fetch_content(url)` | fetch + readability → text (cached) |
| `save_part(Part)` | persist a structured part; derives id from vendor/model if omitted |
| `get_part(id)` | load a stored part |
| `check_compat(part_ids[])` | run compat rules → violations |
| `compose_spec(part_ids[])` | build report: compat, gaps, total TDP |
| `save_spec(id, name?, part_ids[])` / `load_spec(id)` | persist/recall builds |
| `save_listing(Listing)` | record a point-in-time price for a part |
| `find_deals(part_id, search?)` | recorded deals, cheapest first + staleness; `search=true` also returns fresh web hits |
| `find_substitute(part_id, budget?, currency?)` | cheaper drop-in replacements within budget |

Typical flow: `search_parts` → `fetch_content` on a spec page (HTML or PDF) →
extract fields → `save_part` → `compose_spec`. For deals: `find_deals(search=true)`
→ `fetch_content` a listing → `save_listing` → `find_deals` / `find_substitute`.
Extraction is done by the client model; unknown attributes are left blank and
surface as gaps, never as false incompatibilities.

## Test

```sh
go test ./...
```

M3 (SearXNG fallback, lightpanda headless, wider rule coverage) is not built yet.
