# parts-finder-mcp

A local MCP server that turns Claude into a server-hardware sourcing
assistant. Describe the machine you want; Claude searches the live web for
compatible parts, checks them against each other, finds real listings with
current prices in your currency, and builds a costed spec you can export to
a spreadsheet.

It runs entirely on your machine and speaks over stdio — no accounts, no API
keys, no ports.

## Install

Homebrew (macOS + Linux):

```sh
brew install stubbedev/parts-finder/parts-finder
```

Nix:

```sh
nix profile install github:stubbedev/parts-finder-mcp
```

Docker (multi-arch, runs natively on Apple Silicon):

```sh
docker pull ghcr.io/stubbedev/parts-finder-mcp:latest
```

## Register with Claude Code

Native binary:

```sh
claude mcp add parts-finder -- /abs/path/to/parts-finder
```

Docker — the named volumes keep your saved parts and specs across sessions:

```sh
claude mcp add parts-finder -- docker run -i --rm \
  -v parts-finder-data:/data \
  -v parts-finder-cache:/root/.cache \
  ghcr.io/stubbedev/parts-finder-mcp:latest
```

## Register with Claude Desktop

Open **Settings → Developer → Edit Config** (this opens
`claude_desktop_config.json`), add parts-finder under `mcpServers`, then
restart Claude Desktop.

Native binary:

```json
{
  "mcpServers": {
    "parts-finder": {
      "command": "/abs/path/to/parts-finder"
    }
  }
}
```

Docker:

```json
{
  "mcpServers": {
    "parts-finder": {
      "command": "docker",
      "args": [
        "run", "-i", "--rm",
        "-v", "parts-finder-data:/data",
        "-v", "parts-finder-cache:/root/.cache",
        "ghcr.io/stubbedev/parts-finder-mcp:latest"
      ]
    }
  }
}
```

## Optional: your own SearXNG search backend

Web search runs through a chain of keyless engines (DuckDuckGo, Brave,
Ecosia). Heavy sessions can get rate-limited. The fix is a private
[SearXNG](https://docs.searxng.org/) instance: when `SEARXNG_URL` is set it
becomes the FIRST engine in the chain, with no rate-limit exposure — it
queries Google, Startpage, DuckDuckGo and friends server-side, no API keys,
and the public engines remain as fallbacks. (Public SearXNG instances don't
work for this: they disable the JSON API and block automated clients by
design — run your own.)

Set it up (two commands):

```sh
docker run -d --name searxng --restart unless-stopped \
  -p 127.0.0.1:8888:8080 \
  -v searxng-config:/etc/searxng \
  searxng/searxng

# parts-finder uses the JSON API, which is off by default:
docker exec searxng sh -c 'printf "\nsearch:\n  formats:\n    - html\n    - json\n" >> /etc/searxng/settings.yml' \
  && docker restart searxng
```

Verify — should print JSON, not a 403:

```sh
curl 'http://localhost:8888/search?format=json&q=test'
```

Then pass `SEARXNG_URL` when registering. Claude Code:

```sh
claude mcp add parts-finder -e SEARXNG_URL=http://localhost:8888 -- /abs/path/to/parts-finder
```

Claude Desktop:

```json
{
  "mcpServers": {
    "parts-finder": {
      "command": "/abs/path/to/parts-finder",
      "env": { "SEARXNG_URL": "http://localhost:8888" }
    }
  }
}
```

If parts-finder itself runs in Docker, `localhost` is the container — point
it at the host instead: add `--add-host=host.docker.internal:host-gateway`
(Linux; built in on macOS/Windows) to the `docker run` args and use
`SEARXNG_URL=http://host.docker.internal:8888` (pass it with `-e`).

## What you can ask

Once registered, just talk to Claude in plain language:

- **Spec a build** — "Spec a 24-bay TrueNAS box around an Epyc 7003, 256 GB
  RAM, budget 15k DKK." Claude finds parts, verifies they fit together
  (socket, memory type, slots, power, clearance), and flags anything missing.
- **Price it** — every listing is checked live and priced in your local
  currency, VAT-aware, so business and consumer prices compare fairly. Dead
  or out-of-stock listings are flagged, never silently dropped.
- **Shop it** — get the best buy link per part plus alternatives, grouped by
  vendor so you can consolidate shipping.
- **Compare options** — build a few candidate specs and compare them
  side-by-side on compatibility, power draw, and total cost (including
  indicative yearly power cost).
- **Track prices** — record prices over time to judge "buy now or wait", and
  find cheaper drop-in substitutes within a budget.
- **Export** — get a polished `.xlsx` with a sheet per spec and a compare
  sheet.

Region and currency are detected automatically from your IP and search is
biased toward local and EU vendors. The live web is always the source of
truth — Claude searches rather than guessing from training data.

Already own some parts? Tell Claude — owned units count toward compatibility
and power but are excluded from the purchase total, so a piecemeal upgrade
prices only what you're missing.

## Development

```sh
just build        # ./bin/parts-finder
go test ./...
```

Compatibility rules, the retrieval pipeline, resource accounting, and
configuration env vars are documented in [CONTRIBUTING.md](CONTRIBUTING.md).
