<img src="assets/icon.svg" width="64" height="64" alt="">

# aakb

An Anna's Archive MCP server and CLI in one static Go binary. It searches
books, papers and library catalog records, shows the full record for one
result, and downloads one verified file. Use it only for material you are
entitled to obtain.

Built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
and the shared [anna Go client](https://github.com/SokolskyNikita/annas-mcp)
from annas-mcp (MIT), with a unified tool surface, dedupe and ranking on top.

## Build

```bash
go build -o aakb ./cmd/aakb
```

Go 1.27 or newer. No Node, no runtime dependencies.

## MCP

Stdio, which is what local MCP clients launch:

```bash
./aakb            # same as: ./aakb mcp
```

Streamable HTTP, stateless, for a hosted server:

```bash
./aakb serve -addr :8080    # MCP at /mcp, health at /healthz
```

Client config example:

```json
{
  "mcpServers": {
    "aakb": {
      "command": "/absolute/path/to/aakb",
      "env": { "ANNAS_SECRET_KEY": "your-secret-key" }
    }
  }
}
```

Tools:

| Tool | What it does |
| --- | --- |
| `search` | One search for `book`, `article` or `metadata`. A DOI query auto-detects as an article. Exact results are deduped and optionally ranked by availability, reported quality and popularity. Looser suggestions stay in `partial_matches`. |
| `get_details` | Full record for a hash, `/md5/` URL, DOI or catalog id, with identifiers, edition, subjects and reported file problems. |
| `download` | One file by MD5 hash or paper DOI. MD5-verified, reuses a matching local file, never overwrites. |
| `account_status` | Membership, search permission and remaining fast downloads. |

Plus the `find_best_edition` prompt, which walks a client through search,
compare and download. Search needs a membership tier that allows searching.
Downloads work with any paid tier. See your Anna's Archive account page.

## CLI

The same operations without an MCP client, JSON on stdout:

```bash
./aakb search -kind book -format epub -limit 10 "pride and prejudice"
./aakb details <hash>
./aakb download -hash <md5>
./aakb download -doi 10.1038/nature14539
./aakb account
```

## Configuration

| Variable | Purpose |
| --- | --- |
| `ANNAS_SECRET_KEY` | Account secret key. Required for search and fresh downloads. Never a tool argument, never logged. Falls back to `ANNAS_ARCHIVE_API_KEY`. |

On homelab the key already lives in the encrypted systemd credential store.
Run aakb with it, without printing it:

```bash
ANNAS_ARCHIVE_API_KEY="$(systemd-creds --user decrypt --name=ANNAS_ARCHIVE_API_KEY ~/.config/credstore.encrypted/ANNAS_ARCHIVE_API_KEY -)" ./aakb account
```
| `ANNAS_DOWNLOAD_PATH` | Download folder. Default `~/annas-archive`. |
| `ANNAS_BASE_URL` | Preferred mirror. Default `https://annas-archive.gl`. The host you set receives your credentials, so use only one you trust. |
| `ANNAS_AUTO_BASE_URL` | Mirror failover, `true` or `false`. Default `true`. |
| `ANNAS_ACCOUNT_COOKIE` | Optional `aa_account_id2` cookie, used until it expires. |
| `AAKB_CACHE_DIR` | Session and mirror health cache. Default `~/.cache/aakb`, mode `0700`. |

Logs go to stderr, so MCP stdout stays clean.

## Layout

```text
cmd/aakb/              entrypoint: MCP stdio, MCP HTTP and CLI dispatch
internal/config/       environment read once, in main
internal/service/      unified search, dedupe, ranking and downloads over the anna client
internal/mcpserver/    tool, prompt and annotation registration
```

## Checks

```bash
gofmt -l .
go vet ./...
golangci-lint run
govulncheck ./...
go build ./...
```
