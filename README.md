<img src="assets/icon.svg" width="64" height="64" alt="">

# aakb

An Anna's Archive MCP server, CLI, REST API, web UI and Go library in one
static Go binary. It searches books, papers and library catalog records,
shows the full record for one result, and downloads one verified file. Use
it only for material you are entitled to obtain.

Built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
and the shared [anna Go client](https://github.com/SokolskyNikita/annas-mcp)
from annas-mcp (MIT), with a unified tool surface, dedupe and ranking on top.

## Install

Go install, Go 1.27 or newer:

```bash
go install github.com/bjoernaagaard/aakb/cmd/aakb@latest
```

Prebuilt binaries for Linux, macOS and Windows are attached to each
[GitHub Release](https://github.com/bjoernaagaard/aakb/releases) with a
`checksums.txt`. Verify the download against that file before running it.

Docker, for the hosted HTTP server:

```bash
docker build -t aakb .
docker run --rm -p 127.0.0.1:8080:8080 -v aakb-data:/data -e ANNAS_SECRET_KEY -e AAKB_HTTP_TOKEN aakb
```

The image keeps downloads in `/data/downloads` and its session cache in
`/data/cache`.

Or build from a clone. No Node, no runtime dependencies:

```bash
go build -o aakb ./cmd/aakb
```

## MCP

Stdio, which is what local MCP clients launch:

```bash
./aakb            # same as: ./aakb mcp
```

Streamable HTTP, stateless, for a hosted server:

```bash
./aakb serve -addr :8080    # MCP at /mcp, health at /healthz
```

The same server also serves the web UI and the REST API, described below.

Claude Desktop and Cursor, in `claude_desktop_config.json` or `.cursor/mcp.json`:

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

Claude Code:

```bash
claude mcp add aakb --env ANNAS_SECRET_KEY=your-secret-key -- /absolute/path/to/aakb
```

VS Code, in `.vscode/mcp.json`. Note the `servers` key:

```json
{
  "servers": {
    "aakb": {
      "type": "stdio",
      "command": "/absolute/path/to/aakb",
      "env": { "ANNAS_SECRET_KEY": "your-secret-key" }
    }
  }
}
```

A hosted server over Streamable HTTP, for a client that takes a URL:

```json
{
  "mcpServers": {
    "aakb": {
      "type": "http",
      "url": "https://aakb.example.com/mcp",
      "headers": { "Authorization": "Bearer your-http-token" }
    }
  }
}
```

Tools:

| Tool | What it does |
| --- | --- |
| `search` | One search for `book`, `article` or `metadata`. A DOI query auto-detects as an article. Exact results are deduped and optionally ranked by availability, reported quality and popularity. Looser suggestions stay in `partial_matches`. |
| `get_details` | Full record for a hash, `/md5/` URL, DOI or catalog id, with identifiers, edition, subjects and reported file problems. Takes an OpenAlex work id when `OPENALEX_API_KEY` is set. |
| `article_read` | A bounded passage of OpenAlex full text or abstract, with a continuation for the next passage. Needs `OPENALEX_API_KEY`. |
| `download` | One file by MD5 hash, paper DOI or OpenAlex work id. MD5-verified, reuses a matching local file, never overwrites. Sends progress notifications when the call carries a progress token. |
| `account_status` | Membership, search permission and remaining fast downloads. |

Plus the `find_best_edition` prompt, which walks a client through search,
compare and download. Search needs a membership tier that allows searching.
Downloads work with any paid tier. See your Anna's Archive account page.

## CLI

The same operations without an MCP client, JSON on stdout:

```bash
./aakb search -kind book -format epub -limit 10 "pride and prejudice"
./aakb details <hash>
./aakb read W2741809807
./aakb download -hash <md5>
./aakb download -doi 10.1038/nature14539
./aakb account
```

Every command takes `-timeout <seconds>`, capped at 600 seconds, or 7200 for
a download. With `-json-errors`, a failure is one JSON object on stderr, such
as `{"code":"NOT_FOUND","op":"details","message":"..."}`. A download prints
progress lines on stderr unless you pass `-quiet`. An OpenAlex search takes
`-author-id`, `-institution-id`, `-repository-id` and `-relation` with
`-work-id`. An OpenAlex download takes `-artifact`, `-location-id` and
`-version`. Run a command with `-h` to list its flags.

## Web UI and REST API

`aakb serve` serves a web UI at `/` and a JSON API at `/api/v1` from the same
binary. The UI is plain HTML, CSS and JavaScript embedded in the binary, so
it needs no Node and no files beside the binary.

```bash
./aakb serve                 # open http://127.0.0.1:8080/
```

| Method and path | What it does |
| --- | --- |
| `GET /api/v1/capabilities` | Version, whether OpenAlex is configured, and whether a token is required. |
| `GET /api/v1/search?q=...` | Search. Takes `kind`, `title`, `author`, `publisher`, `year`, `format`, `language`, `sort`, `page`, `limit`, `stats`, `rank` and `timeout_seconds`. With `kind=openalex` it also takes `work_type`, `author_id`, `institution_id`, `repository_id`, `relation` and `work_id`. |
| `GET /api/v1/records/lookup?id=...` | Full record for a hash, `/md5/` URL, DOI or catalog id. |
| `GET /api/v1/account` | Membership and remaining fast downloads. |
| `POST /api/v1/article-reads` | JSON body `{"id": "W...", "continuation": "..."}`. Reads OpenAlex text. |
| `POST /api/v1/downloads` | JSON body with exactly one of `hash`, `doi` or `openalex_id`, plus optional `title`, `format`, and for OpenAlex `artifact`, `location_id` and `version`. Returns `201` for a new file and `200` for a reused one, with `file_url`. |
| `GET /downloads/{name}` | The downloaded file as an attachment. Supports range requests. |
| `GET /healthz` | Health check, with `?upstream=1` to probe the mirror. |

Errors are `application/problem+json` (RFC 9457) with a stable `code`, such
as `INVALID_ARGUMENT`, `NOT_FOUND` or `RATE_LIMITED`. Each client IP gets 120
requests a minute across `/api/v1`, `/mcp` and `/downloads`. Behind a reverse
proxy, set `AAKB_TRUSTED_PROXIES` to the proxy's address so that each real
client is counted, and not the proxy.

```bash
curl -s 'http://127.0.0.1:8080/api/v1/search?q=moby+dick&kind=book&limit=3'
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"hash":"<md5>"}' http://127.0.0.1:8080/api/v1/downloads
```

Access rules:

- **With `AAKB_HTTP_TOKEN` set**, every API, MCP and file request needs
  `Authorization: Bearer <token>`. The UI asks for the token once and keeps it
  in tab memory only, so a reload asks again.
- **Without a token on a loopback address**, the UI and API work without
  sign-in, and only accept a `Host` of `localhost`, `127.0.0.1` or `[::1]`
  with the server's port.
- **Without a token on any other address**, the UI and API answer
  `403 TOKEN_REQUIRED`. Put a public server behind a TLS proxy and set a token.

## Go library

Import `github.com/bjoernaagaard/aakb` to use the same operations as the CLI
and MCP server. Pass credentials in `aakb.Config`. A client can be shared
by concurrent callers.

```go
import (
	"context"

	"github.com/bjoernaagaard/aakb"
)

func search(ctx context.Context, key string) (*aakb.SearchResult, error) {
	client, err := aakb.New(aakb.Config{SecretKey: key})
	if err != nil {
		return nil, err
	}
	return client.Search(ctx, aakb.SearchRequest{
		Kind: aakb.KindBook, Query: "Pride and Prejudice", Rank: true,
	})
}
```

Set a deadline on the context to control how long an operation can run.
Without a caller deadline, operations have 60 seconds and downloads have
30 minutes. Use `aakb.CodeOf(err)` to check the error category.

## Configuration

| Variable | Purpose |
| --- | --- |
| `ANNAS_SECRET_KEY` | Account secret key. Required for search and fresh downloads. Never a tool argument, never logged. Falls back to `ANNAS_ARCHIVE_API_KEY`. |
| `ANNAS_DOWNLOAD_PATH` | Download folder. Default `~/annas-archive`. |
| `ANNAS_BASE_URL` | Preferred mirror. Default `https://annas-archive.gl`. The host you set receives your credentials, so use only one you trust. |
| `ANNAS_AUTO_BASE_URL` | Mirror failover, `true` or `false`. Default `true`. |
| `ANNAS_ACCOUNT_COOKIE` | Optional `aa_account_id2` cookie, used until it expires. |
| `AAKB_CACHE_DIR` | Session and mirror health cache. Default `~/.cache/aakb`, mode `0700`. |
| `OPENALEX_API_KEY` | Enables `article_read`, OpenAlex search and OpenAlex downloads. |
| `ANNAS_ALLOWED_TLDS` | Optional comma-separated top-level domains, such as `gl,gd`. aakb refuses to start when `ANNAS_BASE_URL` uses another one. |
| `AAKB_TRUSTED_PROXIES` | Comma-separated proxy addresses or CIDR networks. `aakb serve` reads the client address from `X-Forwarded-For` only on connections from these. |
| `AAKB_HTTP_TOKEN` | Bearer token that `aakb serve` requires when set. Without it, serve binds to `127.0.0.1` by default. Downloads are then readable at `/downloads/{name}` with the same token. |

Optional `.env.local` and `.env` files in the working directory are read at
startup. Variables already in the environment always win, and `.env.local`
wins over `.env`.

Logs go to stderr, so MCP stdout stays clean.

## Layout

```text
cmd/aakb/              entrypoint: MCP stdio, HTTP server and CLI dispatch
internal/config/       environment read once, in main
internal/mcpserver/    tool, prompt and annotation registration
internal/httpapi/      REST API, access guard and file downloads
internal/webui/        embedded web UI
assets/                the aakb icon
./                     public Go library shared by the CLI, MCP, REST and UI
```

## Security

`aakb serve` listens on `127.0.0.1` unless you pass another address, and it
refuses REST and web UI requests on any other address until
`AAKB_HTTP_TOKEN` is set. The secret key stays on the server and is never a
tool argument. Put a public server behind a proxy that terminates TLS, and
give the proxy read and write timeouts above 30 minutes so that long
downloads finish. [SECURITY.md](SECURITY.md) says how to report a
vulnerability. [CHANGELOG.md](CHANGELOG.md) lists what each release changed.

## License

MIT. See [LICENSE](LICENSE).

## Checks

```bash
gofmt -l .
go vet ./...
golangci-lint run
govulncheck ./...
go build ./...
```
